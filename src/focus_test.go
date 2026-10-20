package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/internal/logcapture"
	"github.com/branchkit/plugin-sdk-go"
)

// newFocusTestState returns a PluginState with the maps the focus path touches.
func newFocusTestState() *PluginState {
	return &PluginState{
		PluginID:          "browser",
		LastPushedTabKeys: make(map[string]bool),
		FrameSessions:     make(map[frameKey]*FrameSession),
	}
}

// --- connForAssertedBundleLocked: the pure OS-identity → connection lookup ---
// (recomputeFocusedSourceLocked is exercised in sse_test.go; this covers the
// primitive it's built on directly.)

func TestConnForAssertedBundleLocked(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()
	h.state.conn("connA").AssertedBundle = "com.google.Chrome"
	h.state.conn("connB").AssertedBundle = "org.mozilla.firefox"

	if got := h.connForAssertedBundleLocked("com.google.Chrome"); got != "connA" {
		t.Errorf("Chrome bundle -> %q, want connA", got)
	}
	if got := h.connForAssertedBundleLocked("org.mozilla.firefox"); got != "connB" {
		t.Errorf("Firefox bundle -> %q, want connB", got)
	}
	if got := h.connForAssertedBundleLocked("com.apple.Safari"); got != "" {
		t.Errorf("unconnected bundle -> %q, want empty (not a mispair)", got)
	}
	if got := h.connForAssertedBundleLocked(""); got != "" {
		t.Errorf("empty bundle -> %q, want empty", got)
	}
}

// FocusedBrowserBundleID must stay sticky when a non-browser steals focus, so a
// return to the same browser reads as a refocus (not a switch-wipe). sse_test
// checks the release but not this invariant.
func TestRecomputeReleaseKeepsBrowserBundleSticky(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()
	h.state.conn("connA").AssertedBundle = "com.google.Chrome"
	h.state.FocusedConnID = "connA"
	h.state.FocusedBrowserBundleID = "com.google.Chrome"
	h.state.FocusedTabID = 3
	h.state.BrowserFocused = true

	h.state.FocusedBundleID = "com.apple.Finder"
	if isBrowser, _ := h.recomputeFocusedSourceLocked("test"); isBrowser {
		t.Fatal("non-browser frontmost must not report a focused browser")
	}
	if h.state.FocusedConnID != "" || h.state.BrowserFocused {
		t.Errorf("expected release, got conn=%q browser=%v", h.state.FocusedConnID, h.state.BrowserFocused)
	}
	if h.state.FocusedBrowserBundleID != "com.google.Chrome" {
		t.Errorf("FocusedBrowserBundleID = %q, want sticky com.google.Chrome", h.state.FocusedBrowserBundleID)
	}
}

// --- handleFocusPost / handleActiveTabPost request validation ---
// The dispatch entry points reject malformed input before touching any state
// or RPC. These paths had no coverage.

func TestHandleFocusPostRejectsBadInput(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", "not json"},
		{"missing conn_id", `{"focused":true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/focus", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			serveRoute(h, rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestHandleActiveTabPostRejectsBadInput(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", "{"},
		{"missing conn_id", `{"tab_id":5}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/active-tab", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			serveRoute(h, rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// A background connection's active-tab report is recorded even when it isn't the
// focused browser, so a later focus flip picks up the right tab with no ordering
// race. With no browser frontmost, the recompute releases (returns false) and
// nothing is armed — so this exercises the no-RPC path end to end.
func TestHandleActiveTabPostRecordsBackgroundConn(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()
	// No browser is frontmost (FocusedBundleID empty), so recompute won't arm.
	req := httptest.NewRequest(http.MethodPost, "/active-tab", strings.NewReader(`{"conn_id":"connA","tab_id":42}`))
	rec := httptest.NewRecorder()
	serveRoute(h, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := h.state.peek("connA").ActiveTab; got != 42 {
		t.Errorf("connA ActiveTab = %d, want 42 (recorded even when not focused)", got)
	}
	if h.state.BrowserFocused {
		t.Error("no browser should be focused when none is frontmost")
	}
}

// A focused:false claim only releases its connection's claim (a blur surfaces
// as an app-focus event for the newly-frontmost app; focus_order_test.go has
// the rest). From a connection with no claim it must not error or focus.
func TestHandleFocusPostReleaseIsNoop(t *testing.T) {
	h := newTestHost()
	h.state = newFocusTestState()
	req := httptest.NewRequest(http.MethodPost, "/focus", strings.NewReader(`{"conn_id":"connA","focused":false}`))
	rec := httptest.NewRecorder()
	serveRoute(h, rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if h.state.BrowserFocused {
		t.Error("a focused:false release must not focus a browser")
	}
}

// --- capture.progress forwarding ---
//
// The signal is keyed on the STRUCTURAL property (this plugin owns the
// command, a further capture step is pending), not on the bound step being
// named `prefix`. The name-keyed version silently forwarded nothing for every
// other multi-step capture on every other surface.

// captureProgressEvents wires one SSE client and returns the channel the
// forwarded noop lands on. Mirrors captureModeExitEvents in caret_test.go.
func captureProgressEvents(h *Host, t *testing.T) chan ActionEvent {
	t.Helper()
	h.state = newFocusTestState()
	ch := make(chan ActionEvent, 8)
	h.state.SSEClients = map[string]chan ActionEvent{"client-1": ch}
	return ch
}

func forwardedPrefix(ch chan ActionEvent) (string, bool) {
	select {
	case evt := <-ch:
		return evt.Param("prefix"), evt.Action == "noop"
	default:
		return "", false
	}
}

func TestCaptureProgressForwardsBoundPrefix(t *testing.T) {
	h := newTestHost()
	// The live hint-pattern payload, verbatim from branchkit.log.
	ch := captureProgressEvents(h, t)
	deliverCaptureProgress(t, h, (`{"captured":{"prefix":{"codeword":"flag","letter":"f"}},` +
		`"command_phrase":"<prefix:plugin.browser.alpha> <suffix:plugin.browser.alpha>",` +
		`"next_capture":"suffix","next_collection":"plugin.browser.alpha","owner_plugin":"browser"}`))

	got, isNoop := forwardedPrefix(ch)
	if !isNoop {
		t.Fatalf("expected a noop ActionEvent, got none")
	}
	if got != "flag" {
		t.Errorf("prefix = %q, want %q", got, "flag")
	}
}

// The regression this whole change exists to fix: a multi-step capture whose
// first step is NOT named `prefix` must still forward. Before role-keying this
// returned silently and the surface got no narrowing at all.
func TestCaptureProgressForwardsUnderAnyCaptureName(t *testing.T) {
	h := newTestHost()
	ch := captureProgressEvents(h, t)
	deliverCaptureProgress(t, h, (`{"captured":{"first":{"codeword":"ocean","letter":"o"}},` +
		`"next_capture":"second","next_collection":"browser_palette","owner_plugin":"browser"}`))

	got, isNoop := forwardedPrefix(ch)
	if !isNoop {
		t.Fatalf("expected a noop ActionEvent for a non-`prefix` capture name, got none")
	}
	if got != "ocean" {
		t.Errorf("prefix = %q, want %q", got, "ocean")
	}
}

func TestCaptureProgressSkipsWhenNothingPending(t *testing.T) {
	h := newTestHost()
	// No next_capture => a completed match, not progress. Nothing to narrow
	// toward, so forwarding would dim against a prefix already consumed.
	ch := captureProgressEvents(h, t)
	deliverCaptureProgress(t, h, (`{"captured":{"prefix":{"codeword":"flag","letter":"f"}},` +
		`"next_capture":"","owner_plugin":"browser"}`))

	if _, isNoop := forwardedPrefix(ch); isNoop {
		t.Error("forwarded a narrow for a capture with no pending next step")
	}
}

func TestCaptureProgressIgnoresOtherPlugins(t *testing.T) {
	h := newTestHost()
	ch := captureProgressEvents(h, t)
	deliverCaptureProgress(t, h, (`{"captured":{"prefix":{"codeword":"flag","letter":"f"}},` +
		`"next_capture":"suffix","owner_plugin":"tiling"}`))

	if _, isNoop := forwardedPrefix(ch); isNoop {
		t.Error("forwarded a narrow for another plugin's capture")
	}
}

// captureStderr runs fn and returns what it wrote to os.Stderr, which is
// where branchkit.Logf writes — the plugin log the platform keeps. It moves
// the descriptor rather than assigning os.Stderr, which races every
// goroutine that logs (see logcapture).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := logcapture.Stderr(fn)
	if err != nil {
		t.Fatalf("capture stderr: %v", err)
	}
	return out
}

// The words a partial command has captured are what the user said. They are
// forwarded to the extension, never written to browser's log — on every path
// through the handler, including the ones that skip. The spoken word appears
// in no format string, so finding it in the output means the payload leaked.
// deliverCaptureProgress decodes a wire payload the way the SDK's typed
// helper does, then hands it to the handler. A payload that does not decode
// never reaches the handler (the SDK skips it and logs its size only).
func deliverCaptureProgress(t *testing.T, h *Host, payload string) {
	t.Helper()
	var event branchkit.CaptureProgressEventParams
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	h.handleCaptureProgress(event)
}

func TestCaptureProgressLogsNoCapturedWords(t *testing.T) {
	const spoken = "marigold"
	payloads := map[string]string{
		"forwarded": `{"captured":{"prefix":{"codeword":"` + spoken + `","letter":"m"}},` +
			`"next_capture":"suffix","next_collection":"plugin.browser.alpha","owner_plugin":"browser"}`,
		"bare word forwarded": `{"captured":{"prefix":"` + spoken + `"},` +
			`"next_capture":"suffix","owner_plugin":"browser"}`,
		"two captures bound": `{"captured":{"a":{"codeword":"` + spoken + `"},"b":{"codeword":"` + spoken + `"}},` +
			`"next_capture":"c","owner_plugin":"browser"}`,
		"nothing pending": `{"captured":{"prefix":{"codeword":"` + spoken + `"}},` +
			`"next_capture":"","owner_plugin":"browser"}`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			h := newTestHost()
			ch := captureProgressEvents(h, t)
			logged := captureStderr(t, func() { deliverCaptureProgress(t, h, (payload)) })
			if strings.TrimSpace(logged) == "" {
				t.Fatalf("logged nothing — the path under test did not run")
			}
			if strings.Contains(strings.ToLower(logged), spoken) {
				t.Errorf("log output contains the captured word %q:\n%s", spoken, logged)
			}
			if name == "forwarded" || name == "bare word forwarded" {
				if got, isNoop := forwardedPrefix(ch); !isNoop || got != spoken {
					t.Errorf("prefix = %q (noop=%v), want %q forwarded to the extension", got, isNoop, spoken)
				}
			}
		})
	}
}

func TestBoundCodeword(t *testing.T) {
	obj := func(s string) json.RawMessage { return json.RawMessage(s) }

	tests := []struct {
		name     string
		captured map[string]json.RawMessage
		want     string
		wantOK   bool
	}{
		{"single object-shaped row", map[string]json.RawMessage{
			"prefix": obj(`{"codeword":"flag","letter":"f"}`)}, "flag", true},
		{"capture name is irrelevant", map[string]json.RawMessage{
			"anything": obj(`{"codeword":"ocean","letter":"o"}`)}, "ocean", true},
		{"empty map", map[string]json.RawMessage{}, "", false},
		// A map carries no step ordering, so "prefix so far" is undefined with
		// two bound steps. Refuse rather than let map iteration pick.
		{"two bound steps refuses", map[string]json.RawMessage{
			"a": obj(`{"codeword":"one"}`),
			"b": obj(`{"codeword":"two"}`)}, "", false},
		// The platform's surface prefix-index entries bind SCALAR values
		// (named-entities: the prefix word itself) — the `@palette`
		// migration's shape. Accepted since then.
		{"scalar row", map[string]json.RawMessage{
			"prefix": obj(`"flag"`)}, "flag", true},
		{"row without codeword field", map[string]json.RawMessage{
			"prefix": obj(`{"letter":"f"}`)}, "", false},
		{"empty codeword", map[string]json.RawMessage{
			"prefix": obj(`{"codeword":""}`)}, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := boundCodeword(tc.captured)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("boundCodeword() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
