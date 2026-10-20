package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The extension listener's exposure: /health behind the token
// and without CORS, the Host header checked, server timeouts, body caps, and
// the perf trail owner-only.

const listenerTestToken = "test-token"

func listenerHandler(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newRoutingHost(t)
	return newExtensionServer(h.extensionMux(listenerTestToken)).Handler
}

func serve(handler http.Handler, method, path, host, token string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestHealthRequiresTheTokenAndSendsNoCORS(t *testing.T) {
	handler := listenerHandler(t)

	w := serve(handler, http.MethodGet, "/health", "127.0.0.1:5000", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /health = %d, want 401", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("/health sent Access-Control-Allow-Origin %q; any page could read it", got)
	}

	w = serve(handler, http.MethodGet, "/health", "127.0.0.1:5000", listenerTestToken, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stream_clients":0`) {
		t.Errorf("authenticated /health = %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("/health sent Access-Control-Allow-Origin %q", got)
	}

	// The extension's own routes keep their CORS headers.
	w = serve(handler, http.MethodOptions, "/grammar/frame", "127.0.0.1:5000", "", nil)
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("the extension routes' preflight lost its CORS headers")
	}
}

// DNS rebinding reaches the loopback socket with the attacker's domain in
// Host; the listener refuses it before any route runs.
func TestListenerRefusesNonLoopbackHost(t *testing.T) {
	handler := listenerHandler(t)
	for _, host := range []string{"evil.example:5000", "evil.example", "192.168.1.5:5000", "127.0.0.1.evil.example"} {
		w := serve(handler, http.MethodGet, "/health", host, listenerTestToken, nil)
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q = %d, want 421", host, w.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:5000", "localhost:5000", "LOCALHOST", "[::1]:5000", "127.0.0.1"} {
		w := serve(handler, http.MethodGet, "/health", host, listenerTestToken, nil)
		if w.Code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, w.Code)
		}
	}
}

func TestExtensionServerHasTimeouts(t *testing.T) {
	srv := newExtensionServer(http.NewServeMux())
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Errorf("server limits unset: header=%v idle=%v maxHeader=%d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
	// A server-wide write or read timeout would end the event stream.
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Errorf("server-wide Write/ReadTimeout set (%v/%v): it would end the event stream", srv.WriteTimeout, srv.ReadTimeout)
	}
}

func TestRequestBodiesAreCapped(t *testing.T) {
	var readErr error
	handler := requestLimits(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	big := strings.NewReader(strings.Repeat("x", maxRequestBody+1))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/grammar/frame", big))
	var tooBig *http.MaxBytesError
	if !errors.As(readErr, &tooBig) {
		t.Errorf("a body over the cap read without error (%v)", readErr)
	}

	// The debug captures get the larger cap.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/debug-snapshot/screenshot",
		strings.NewReader(strings.Repeat("x", maxRequestBody+1))))
	if readErr != nil {
		t.Errorf("a debug capture over the default cap was refused: %v", readErr)
	}
}

// deadlineRecorder is a ResponseWriter that records the deadlines
// http.ResponseController sets on it.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write []time.Time
}

func (d *deadlineRecorder) SetReadDeadline(t time.Time) error { d.read = append(d.read, t); return nil }
func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.write = append(d.write, t)
	return nil
}

// Deadlines are per connection and keep-alive reuses connections: every
// request must set its own — cleared for the event stream (else a pooled
// connection's earlier POST deadline would end the stream), fresh otherwise.
func TestRequestDeadlinesPerRequest(t *testing.T) {
	handler := requestLimits(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	post := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPut, "/grammar/frame", nil))
	if len(post.read) != 1 || len(post.write) != 1 || post.read[0].IsZero() || post.write[0].IsZero() {
		t.Errorf("a POST must get fresh read/write deadlines, got read=%v write=%v", post.read, post.write)
	}

	stream := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(stream, httptest.NewRequest(http.MethodGet, eventStreamPath, nil))
	if len(stream.read) != 1 || len(stream.write) != 1 || !stream.read[0].IsZero() || !stream.write[0].IsZero() {
		t.Errorf("the event stream must clear both deadlines, got read=%v write=%v", stream.read, stream.write)
	}
}

// The perf trail carries page URLs: owner-only, including a trail an older
// build left world-readable.
func TestPerfTrailIsOwnerOnly(t *testing.T) {
	h := newTestHost()
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.jsonl")
	if err := h.appendPerfReportLine(fresh, []byte(`{}`), 1<<20); err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix mode bits (Go reports 0666 for any writable
	// file); there the plugin's data folder's ACL is what keeps it private.
	if runtime.GOOS == "windows" {
		return
	}
	if mode := fileMode(t, fresh); mode != 0o600 {
		t.Errorf("new trail mode = %o, want 600", mode)
	}

	old := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.appendPerfReportLine(old, []byte(`{}`), 1<<20); err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, old); mode != 0o600 {
		t.Errorf("existing trail mode = %o, want 600", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}
