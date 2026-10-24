package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// The extension listener's routes are declared once, in wire.Routes, with
// their request and reply types. This file binds each to its handler and
// builds the mux from the table: a handler whose types differ from its
// route's, a route with no handler, or a handler with no route fails at
// startup (and in TestExtensionMuxMatchesTheWireTable), so no route exists
// untyped and none can drift from the extension's generated gen.ts.

// boundRoute is one route's handler with the types it was written against.
type boundRoute struct {
	req, resp reflect.Type
	serve     http.HandlerFunc
}

// httpError is a refusal with its status. Anything else a handler returns is
// a 500.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{http.StatusBadRequest, msg} }

func statusError(status int, msg string) error { return &httpError{status, msg} }

// ack is the reply of a write route that returns no data.
func ack() (wire.Ack, error) { return wire.Ack{OK: true}, nil }

// typed binds a handler that takes a decoded request and returns a reply.
// Decoding is strict under test and lenient-with-a-warning in production
// (strictWire): a renamed field fails `go test`, and in the field it is a
// logged drift line rather than a 400 that silently breaks a voice command.
func typed[Req, Resp any](fn func(r *http.Request, req *Req) (Resp, error)) boundRoute {
	return boundRoute{
		req:  reflect.TypeOf((*Req)(nil)).Elem(),
		resp: reflect.TypeOf((*Resp)(nil)).Elem(),
		serve: func(w http.ResponseWriter, r *http.Request) {
			var req Req
			var err error
			if r.Method == http.MethodGet {
				err = decodeQuery(r, &req, strictWire)
			} else {
				err = decodeBody(r, &req, strictWire)
			}
			if err != nil {
				writeError(w, badRequest("bad request: "+err.Error()))
				return
			}
			resp, err := fn(r, &req)
			if err != nil {
				writeError(w, err)
				return
			}
			writeReply(w, http.StatusOK, resp)
		},
	}
}

// stream binds the event stream: it writes its own response.
func stream[Req any](fn func(w http.ResponseWriter, r *http.Request, req *Req)) boundRoute {
	return boundRoute{
		req: reflect.TypeOf((*Req)(nil)).Elem(),
		serve: func(w http.ResponseWriter, r *http.Request) {
			var req Req
			if err := decodeQuery(r, &req, strictWire); err != nil {
				writeError(w, badRequest("bad request: "+err.Error()))
				return
			}
			fn(w, r, &req)
		},
	}
}

// strictWire makes decoding refuse what the wire table does not declare: an
// unknown body field, trailing data, an undeclared query parameter. Tests turn
// it on (routes_test.go), so drift between the two sides fails `go test`.
// Production stays lenient: a stray key in a real payload must not 400 a
// voice command. There it is logged, once per route and field (wireDrift),
// so drift is visible in the plugin log without breaking anything.
var strictWire = false

// wireDriftSeen dedups drift warnings: one line per route and field.
var wireDriftSeen sync.Map

// wireDrift reports a key the wire table does not declare, once per route.
func wireDrift(route, what string) {
	key := route + "|" + what
	if _, dup := wireDriftSeen.LoadOrStore(key, true); dup {
		return
	}
	branchkit.Logf("browser", "[WIRE_DRIFT] warn route=%q %s — not in the wire declaration (plugins/browser/src/wire); ignored. The extension and plugin disagree: regenerate gen.ts / update both.", route, what)
}

// decodeBody reads one JSON value into v. Strict (tests): unknown fields and
// trailing data are refused. Lenient (production): both are ignored after a
// one-time warning per route and field. An empty body is the empty object
// (a route whose request has no fields).
//
// Lenient decoding still decodes strictly first: a body that matches its
// declaration — every one, while the two sides agree — is parsed once. Only a
// body the strict decode refuses is parsed again, leniently into v and
// generically to name the drifted fields. Scanning every body generically cost
// a second full parse of each grammar batch and debug screenshot (up to 64 MB)
// to find nothing.
func decodeBody(r *http.Request, v any, strict bool) error {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	route := r.Method + " " + r.URL.Path
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	strictErr := dec.Decode(v)
	if strictErr != nil && strict {
		return strictErr
	}
	if strictErr != nil {
		// Start over from the zero value: the refused decode may have filled
		// part of v before it stopped.
		rv := reflect.ValueOf(v).Elem()
		rv.Set(reflect.Zero(rv.Type()))
		dec = json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(v); err != nil {
			return err
		}
		wireDriftScans.Add(1)
		// The first JSON value only, the one decoded above (trailing data
		// is warned about below).
		var raw any
		if json.NewDecoder(bytes.NewReader(body)).Decode(&raw) == nil {
			for _, f := range unknownFields(reflect.TypeOf(v).Elem(), raw, "") {
				wireDrift(route, "unknown field "+strconv.Quote(f))
			}
		}
	}
	if dec.More() {
		if strict {
			return errors.New("trailing data after the JSON body")
		}
		wireDrift(route, "trailing data after the JSON body")
	}
	return nil
}

// wireDriftScans counts the generic re-parses decodeBody made to name drift
// (test seam: a body that matches its declaration must cost none).
var wireDriftScans atomic.Int64

var rawMessageType = reflect.TypeOf(json.RawMessage{})

// unknownFields lists the JSON keys in raw (dotted paths, [] for list items)
// that type t does not declare. Maps, RawMessage and interface values accept
// anything; a type that decodes itself (json.Unmarshaler) is opaque.
func unknownFields(t reflect.Type, raw any, prefix string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType || t.Kind() == reflect.Interface ||
		reflect.PointerTo(t).Implements(reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()) {
		return nil
	}
	var out []string
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				out = append(out, prefix+k)
				continue
			}
			out = append(out, unknownFields(ft, obj[k], prefix+k+".")...)
		}
	case reflect.Slice, reflect.Array:
		list, ok := raw.([]any)
		if !ok {
			return nil
		}
		for _, item := range list {
			out = append(out, unknownFields(t.Elem(), item, strings.TrimSuffix(prefix, ".")+"[].")...)
		}
	case reflect.Map:
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		for _, v := range obj {
			out = append(out, unknownFields(t.Elem(), v, prefix+"*.")...)
		}
	}
	return dedupe(out)
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := s[:0]
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// decodeQuery fills a GET request's string fields from the query. An
// undeclared parameter is refused when strict (tests), warned once and
// ignored otherwise (production).
//
// The values go in through json.Unmarshal, not reflect's SetString: the
// query type's json tags already name its parameters, and a decode the
// dead-field analyser (tools/deadfield) can follow keeps it from reporting
// every query field as never written.
func decodeQuery(r *http.Request, v any, strict bool) error {
	rv := reflect.ValueOf(v).Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("query target %s is not a struct", rv.Type())
	}
	fields := wire.QueryFields(rv.Type())
	params := map[string]string{}
	for key, vals := range r.URL.Query() {
		if _, ok := fields[key]; !ok {
			if strict {
				return fmt.Errorf("unknown query parameter %q", key)
			}
			wireDrift(r.Method+" "+r.URL.Path, "unknown query parameter "+strconv.Quote(key))
			continue
		}
		if len(vals) > 0 {
			params[key] = vals[0]
		}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func writeReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// replyError is a refusal whose body is the route's own reply type rather
// than an Ack (the ticket's version mismatch carries wire_version).
type replyError interface {
	error
	reply() (status int, body any)
}

// writeError answers a refusal as the one error shape, {ok:false, error}.
func writeError(w http.ResponseWriter, err error) {
	var re replyError
	if errors.As(err, &re) {
		status, body := re.reply()
		writeReply(w, status, body)
		return
	}
	status := http.StatusInternalServerError
	var he *httpError
	if errors.As(err, &he) {
		status = he.status
	}
	writeReply(w, status, wire.Ack{OK: false, Error: err.Error()})
}

// routeHandlers binds every route to its handler, by the route's key.
func (h *Host) routeHandlers(tickets *ticketStore) map[string]boundRoute {
	return map[string]boundRoute{
		wire.GrammarFrame.Key():         typed(h.handleGrammarFramePut),
		wire.HintsSessionEnd.Key():      typed(h.handleHintsSessionEndPost),
		wire.HintsSessionStart.Key():    typed(h.handleHintsSessionStartPost),
		wire.RangePick.Key():            typed(h.handleRangePickPost),
		wire.DispatchResultRoute.Key():  typed(h.handleDispatchResultPost),
		wire.SurgeryResultRoute.Key():   typed(h.handleSurgeryResultPost),
		wire.Session.Key():              typed(h.handleSessionPut),
		wire.Focus.Key():                typed(h.handleFocusPost),
		wire.ActiveTab.Key():            typed(h.handleActiveTabPost),
		wire.Tabs.Key():                 typed(h.handleTabsPost),
		wire.Palette.Key():              typed(h.handlePalettePost),
		wire.CommandsContribute.Key():   typed(h.handleCommandsContributePost),
		wire.CommandOverride.Key():      typed(h.handleCommandOverridePost),
		wire.CommandOverrideReset.Key(): typed(h.handleCommandOverrideResetPost),
		wire.CommandOverrides.Key():     typed(h.handleCommandOverridesGet),
		wire.CommandAlias.Key():         typed(h.handleCommandAliasPost),
		wire.CommandAliasRemove.Key():   typed(h.handleCommandAliasRemovePost),
		wire.CommandAliases.Key():       typed(h.handleCommandAliasesGet),
		wire.HintDisplay.Key():          typed(h.handleHintDisplayPost),
		wire.ReferenceNames.Key():       typed(h.handleReferencesPost),
		wire.References.Key():           typed(h.handleReferencesGet),
		wire.ReferenceSave.Key():        typed(h.handleReferenceSavePost),
		wire.SettingsBackupPut.Key():    typed(h.handleSettingsBackupPut),
		wire.SettingsBackupGet.Key():    typed(h.handleSettingsBackupGet),
		wire.DebugLog.Key():             typed(h.handleDebugLogPost),
		wire.PluginDebugLog.Key():       typed(h.handlePluginDebugLogPost),
		wire.PerfReport.Key():           typed(h.handlePerfReportPost),
		wire.DebugSnapshotRoute.Key():   typed(h.handleDebugSnapshot),
		wire.DebugScreenshot.Key():      typed(h.handleDebugSnapshotScreenshot),
		wire.DebugMatchable.Key():       typed(h.handleDebugMatchableGet),
		wire.EventsTicket.Key():         typed(h.ticketHandler(tickets)),
		wire.Events.Key():               stream(h.eventsHandler(tickets)),
		wire.Health.Key():               typed(h.handleHealthGet),
		wire.FocusPinGet.Key():          typed(h.handleFocusPinGet),
		wire.FocusPinSet.Key():          typed(h.handleFocusPinPost),
	}
}

// buildExtensionMux registers every route of the table, behind its auth.
// Panics on any disagreement between the table and the handlers: the
// listener must not come up with a route the extension's types don't
// describe.
func (h *Host) buildExtensionMux(token string) *http.ServeMux {
	tickets := newTicketStore()
	handlers := h.routeHandlers(tickets)
	mux := http.NewServeMux()
	seen := map[string]bool{}
	for _, rt := range wire.Routes {
		key := rt.Key()
		seen[key] = true
		b, ok := handlers[key]
		if !ok {
			panic("extension mux: no handler for " + key)
		}
		if b.req != reflect.TypeOf(rt.Req) {
			panic(fmt.Sprintf("extension mux: %s handler takes %s, the wire table says %s", key, b.req, reflect.TypeOf(rt.Req)))
		}
		var want reflect.Type
		if rt.Resp != nil {
			want = reflect.TypeOf(rt.Resp)
		}
		if b.resp != want {
			panic(fmt.Sprintf("extension mux: %s handler replies %v, the wire table says %v", key, b.resp, want))
		}
		if rt.Dev && !devBuild() {
			// Never registered on a release build: the mux answers 404
			// exactly as for any path it does not have.
			continue
		}
		serve := b.serve
		if rt.Auth == wire.AuthToken {
			serve = requireToken(token, serve)
		}
		mux.HandleFunc(key, serve)
	}
	for key := range handlers {
		if !seen[key] {
			panic("extension mux: handler for " + key + " has no route in the wire table")
		}
	}
	return mux
}

// requireToken refuses a request without the pairing token.
func requireToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validateToken(r, token) {
			writeError(w, statusError(http.StatusUnauthorized, "unauthorized"))
			return
		}
		next(w, r)
	}
}
