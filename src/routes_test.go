package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/branchkit-plugin-browser/wire/wiredoc"
)

// Tests decode strictly: an unknown field, trailing data or an undeclared
// query parameter is a 400, so drift between a handler's expectations and
// the wire table fails here. Production is lenient (routes.go strictWire).
func init() { strictWire = true }

// serveRoute runs a request through its route's bound handler — the strict
// decoder and the typed reply included — without the token check, so a test
// exercises exactly what the listener would after auth.
func serveRoute(h *Host, w http.ResponseWriter, r *http.Request) {
	b, ok := h.routeHandlers(newTicketStore())[r.Method+" "+r.URL.Path]
	if !ok {
		panic("no route for " + r.Method + " " + r.URL.Path)
	}
	b.serve(w, r)
}

// The mux is built from the wire table: building it is the check that every
// route has a handler of the declared types, and no handler lacks a route.
// (A disagreement panics, which the listener must never come up with.)
func TestExtensionMuxMatchesTheWireTable(t *testing.T) {
	h, _ := newRoutingHost(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("building the extension mux: %v", r)
		}
	}()
	h.extensionMux(listenerTestToken)
	if got, want := len(h.routeHandlers(newTicketStore())), len(wire.Routes); got != want {
		t.Errorf("%d handlers for %d routes", got, want)
	}
}

// Strictly (as under test), unknown fields are refused: a field one side
// renamed fails at the first request instead of reading as its zero value.
func TestUnknownFieldsAreRefused(t *testing.T) {
	h, _ := newRoutingHost(t)
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/session",
		strings.NewReader(`{"conn_id":"chrome","modes":{"caret":false},"carret":true}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("unknown field: status %d body %s, want 400 {ok:false}", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodGet, "/references?host=a.com&hots=b", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown query parameter: status %d, want 400", rec.Code)
	}
}

// A write route answers the one reply shape.
func TestWriteRoutesAnswerAck(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/session", strings.NewReader(`{"conn_id":"chrome","modes":{}}`)))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Errorf("status %d body %q, want 200 {\"ok\":true}", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/session", strings.NewReader(`{"modes":{}}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error":"missing conn_id"`) {
		t.Errorf("status %d body %q, want 400 with the reason", rec.Code, rec.Body.String())
	}
}

// The ticket refuses an extension built against another wire, and says which
// version this side is.
func TestTicketRefusesAWireVersionMismatch(t *testing.T) {
	handler := listenerHandler(t)
	w := serve(handler, http.MethodPost, "/events/ticket", "127.0.0.1:5000", listenerTestToken,
		strings.NewReader(`{"wire_version":"0000000000000000"}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("mismatch: status %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"wire_version":"`+wire.Version()+`"`) ||
		!strings.Contains(w.Body.String(), "wire_version_mismatch") {
		t.Errorf("mismatch body %s", w.Body.String())
	}
	w = serve(handler, http.MethodPost, "/events/ticket", "127.0.0.1:5000", listenerTestToken,
		strings.NewReader(`{"wire_version":"`+wire.Version()+`"}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ticket":"`) {
		t.Errorf("match: status %d body %s", w.Code, w.Body.String())
	}
}

// Every synthetic action's params are an object the generator can name, and
// NewAction carries them as JSON, not as a JSON string.
func TestSyntheticActionParamsAreObjects(t *testing.T) {
	for _, a := range wire.SyntheticActions {
		if reflect.TypeOf(a.Params).Kind() != reflect.Struct {
			t.Errorf("%s: params %T is not a struct", a.Name, a.Params)
		}
	}
	ev := wire.NewAction("palette_assign", wire.PaletteAssignParams{Assignments: map[string]string{"tab:1": "arch"}})
	if string(ev.Params) != `{"assignments":{"tab:1":"arch"}}` {
		t.Errorf("params %s", ev.Params)
	}
}

// The extension's committed gen.ts is generated from this package. When the
// workspace sibling is present, it must be byte-identical to a fresh run;
// `just check-browser-wire` is the gate, this is the same check from here.
func TestExtensionGenTSIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "..", "branchkit-extension", "src", "wire", "gen.ts")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no workspace extension at %s", path)
	}
	docs, err := wiredoc.Read("wire")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout (autocrlf) has CRLF; the generator writes LF.
	if strings.ReplaceAll(string(committed), "\r\n", "\n") != wire.TypeScript(docs) {
		t.Errorf("%s is stale: run `go run ./cmd/wiregen -o %s`", path, path)
	}
}

// Every route's request type round-trips strictly: what the wire table
// declares, marshaled, decodes back with unknown fields refused. A field
// whose json tag cannot read back what it writes fails here.
func TestEveryRouteRequestRoundTripsStrictly(t *testing.T) {
	for _, rt := range wire.Routes {
		typ := reflect.TypeOf(rt.Req)
		zero := reflect.New(typ)
		if rt.Method == http.MethodGet {
			// Queries: every declared parameter is accepted.
			q := url.Values{}
			for name := range wire.QueryFields(typ) {
				q.Set(name, "x")
			}
			req := httptest.NewRequest(http.MethodGet, rt.Path+"?"+q.Encode(), nil)
			if err := decodeQuery(req, zero.Interface(), true); err != nil {
				t.Errorf("%s: %v", rt.Key(), err)
			}
			continue
		}
		body := []byte("{}")
		if _, opaque := rt.Req.(wire.DebugSnapshot); !opaque {
			var err error
			body, err = json.Marshal(filled(typ).Interface())
			if err != nil {
				t.Fatalf("%s: marshal: %v", rt.Key(), err)
			}
		}
		req := httptest.NewRequest(rt.Method, rt.Path, bytes.NewReader(body))
		if err := decodeBody(req, zero.Interface(), true); err != nil {
			t.Errorf("%s: strict decode of its own type failed: %v (%s)", rt.Key(), err, body)
		}
	}
}

// filled is a value of t with every field set, so omitempty fields are on the
// wire too and the strict decode sees each one.
func filled(t reflect.Type) reflect.Value {
	v := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				v.Field(i).Set(filled(t.Field(i).Type))
			}
		}
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		p.Elem().Set(filled(t.Elem()))
		v.Set(p)
	case reflect.Slice:
		if t == reflect.TypeOf(json.RawMessage{}) {
			v.Set(reflect.ValueOf(json.RawMessage(`{"k":1}`)))
		} else {
			v.Set(reflect.Append(reflect.MakeSlice(t, 0, 1), filled(t.Elem())))
		}
	case reflect.Map:
		m := reflect.MakeMap(t)
		m.SetMapIndex(reflect.ValueOf("k"), filled(t.Elem()))
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Uint64, reflect.Uint, reflect.Uint32:
		v.SetUint(1)
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	}
	return v
}

// Production decoding is lenient: a stray key, trailing data or an unknown
// query parameter is ignored (and logged once per route and field), and the
// request goes through — a drift must never 400 a real voice command.
func TestProductionDecodingIsLenientAndNamesTheDrift(t *testing.T) {
	var req wire.DispatchResult
	r := httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"c","action":"activate","fp":"x","extra":{"a":1}} {}`))
	if err := decodeBody(r, &req, false); err != nil {
		t.Fatalf("lenient decode refused: %v", err)
	}
	if req.ConnID != "c" || req.Action != "activate" {
		t.Errorf("declared fields lost: %+v", req)
	}

	var frame wire.GrammarFrameRequest
	got := unknownFields(reflect.TypeOf(frame),
		map[string]any{"tab_id": 1.0, "app_id": "", "elements": []any{map[string]any{"id": 1.0, "label": "x"}}}, "")
	if strings.Join(got, ",") != "app_id,elements[].label" {
		t.Errorf("unknown fields %v, want app_id and elements[].label", got)
	}

	var q wire.ReferencesQuery
	r = httptest.NewRequest(http.MethodGet, "/references?host=a.com&hots=b", nil)
	if err := decodeQuery(r, &q, false); err != nil || q.Host != "a.com" {
		t.Errorf("lenient query: %v %+v", err, q)
	}
}

// A body that matches its declaration is parsed once even in production: the
// generic drift scan (a second full parse — 64 MB for a debug screenshot)
// runs only for a body the strict decode refused.
func TestLenientDecodingScansOnlyADriftedBody(t *testing.T) {
	before := wireDriftScans.Load()
	var req wire.DispatchResult
	r := httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"c","action":"activate","ok":true}`))
	if err := decodeBody(r, &req, false); err != nil || req.ConnID != "c" {
		t.Fatalf("clean body: %v %+v", err, req)
	}
	if n := wireDriftScans.Load() - before; n != 0 {
		t.Fatalf("a clean body was re-parsed for drift %d time(s), want 0", n)
	}

	req = wire.DispatchResult{}
	r = httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"c","stray":1,"action":"activate"}`))
	if err := decodeBody(r, &req, false); err != nil {
		t.Fatalf("drifted body refused: %v", err)
	}
	if req.ConnID != "c" || req.Action != "activate" {
		t.Errorf("declared fields lost past the stray key: %+v", req)
	}
	if n := wireDriftScans.Load() - before; n != 1 {
		t.Fatalf("drift scans = %d, want 1 (the drifted body)", n)
	}
}
