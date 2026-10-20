package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// The dev-build focus pin (focus_pin.go): one connection is the focused
// browser whatever app the OS has in front, and nothing the plugin does while
// pinned reaches the person's real app.

const pinTestToken = "pin-token"

// pinMux is the listener's route table under the given app-support folder.
func pinMux(t *testing.T, h *Host, appSupport string) http.Handler {
	t.Helper()
	t.Setenv("BRANCHKIT_APP_SUPPORT", appSupport)
	return h.extensionMux(pinTestToken)
}

func pinRequest(t *testing.T, mux http.Handler, method, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/dev/focus-pin", nil)
	} else {
		req = httptest.NewRequest(method, "/dev/focus-pin", strings.NewReader(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// pin posts a pin (conn "" unpins) through the handler and fails on non-200.
func pin(t *testing.T, h *Host, body string) focusPinState {
	t.Helper()
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPost, "/dev/focus-pin", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /dev/focus-pin %s: status %d: %s", body, rec.Code, rec.Body.String())
	}
	var st focusPinState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode pin state: %v", err)
	}
	return st
}

func focusedConn(h *Host) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.FocusedConnID, h.state.BrowserFocused
}

// The route is a dev-build route: under a release build's folder, or none, it
// does not exist — 404 even with the right token. Under BranchKitDev it
// answers.
func TestFocusPinRouteOnlyInDevBuild(t *testing.T) {
	for _, dir := range []string{"", "/Users/x/Library/Application Support/BranchKit", "/tmp/BranchKitDevX", "/tmp/BranchKitDev/sub"} {
		t.Run("off "+dir, func(t *testing.T) {
			h, _ := newRoutingHost(t)
			mux := pinMux(t, h, dir)
			if rec := pinRequest(t, mux, http.MethodGet, "", pinTestToken); rec.Code != http.StatusNotFound {
				t.Errorf("GET under %q: status %d, want 404", dir, rec.Code)
			}
			if rec := pinRequest(t, mux, http.MethodPost, `{"conn_id":"x"}`, pinTestToken); rec.Code != http.StatusNotFound {
				t.Errorf("POST under %q: status %d, want 404", dir, rec.Code)
			}
		})
	}
	for _, dir := range []string{"/Users/x/Library/Application Support/BranchKitDev", "/home/x/.local/share/BranchKitDev/"} {
		t.Run("on "+dir, func(t *testing.T) {
			h, _ := newRoutingHost(t)
			mux := pinMux(t, h, dir)
			if rec := pinRequest(t, mux, http.MethodGet, "", pinTestToken); rec.Code != http.StatusOK {
				t.Errorf("GET under %q: status %d, want 200", dir, rec.Code)
			}
		})
	}
}

// Same bearer token as every other route.
func TestFocusPinRequiresToken(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "fox", firefoxBundle)
	mux := pinMux(t, h, "/tmp/BranchKitDev")
	for _, tok := range []string{"", "wrong"} {
		if rec := pinRequest(t, mux, http.MethodGet, "", tok); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET token %q: status %d, want 401", tok, rec.Code)
		}
		if rec := pinRequest(t, mux, http.MethodPost, `{"conn_id":"fox"}`, tok); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST token %q: status %d, want 401", tok, rec.Code)
		}
	}
	if c := h.pin.current(); c != "" {
		t.Fatalf("an unauthenticated POST pinned %q", c)
	}
	if rec := pinRequest(t, mux, http.MethodPost, `{"conn_id":"fox"}`, pinTestToken); rec.Code != http.StatusOK {
		t.Fatalf("authenticated POST: status %d", rec.Code)
	}
	if c := h.pin.current(); c != "fox" {
		t.Errorf("pinned %q, want fox", c)
	}
}

// Only a live connection can be pinned.
func TestFocusPinRefusesUnknownConnection(t *testing.T) {
	h, _ := newRoutingHost(t)
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPost, "/dev/focus-pin", strings.NewReader(`{"conn_id":"ghost"}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("status %d, want 409", rec.Code)
	}
	if c := h.pin.current(); c != "" {
		t.Errorf("pinned %q", c)
	}
}

// Pinned, the connection is the focused browser with Terminal in front, and
// stays so across OS focus moves to another app and to another connected
// browser. The OS events still land in FocusedBundleID; actions go to it.
func TestFocusPinFollowsPinAcrossAppFocus(t *testing.T) {
	h, log := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	_, foxCh := connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, "com.apple.Terminal")
	if conn, _ := focusedConn(h); conn != "" {
		t.Fatalf("precondition: focused %q with Terminal in front", conn)
	}

	st := pin(t, h, `{"conn_id":"fox"}`)
	if !st.Pinned || st.FocusedConnID != "fox" || !st.BrowserFocused {
		t.Fatalf("after pin: %+v", st)
	}
	if log.count("put_active") == 0 {
		t.Error("pinning did not arm plugin.browser.active")
	}

	for _, front := range []string{chromeBundle, "com.apple.Terminal", "com.microsoft.VSCode"} {
		focusApp(h, front)
		conn, focused := focusedConn(h)
		if conn != "fox" || !focused {
			t.Errorf("after app.focused %s: focused %q browser=%v, want fox", front, conn, focused)
		}
		h.mu.Lock()
		got := h.state.FocusedBundleID
		h.mu.Unlock()
		if got != front {
			t.Errorf("FocusedBundleID = %q, want the OS's %q", got, front)
		}
	}
	// A real browser's own claim does not take focus from the pin either.
	claimFocus(t, h, "chrome")
	if conn, _ := focusedConn(h); conn != "fox" {
		t.Errorf("after chrome's claim: focused %q, want fox", conn)
	}

	received(foxCh) // the arm's traffic
	dispatch(h, "scroll_down")
	if got := received(foxCh); len(got) != 1 || got[0] != "scroll_down" {
		t.Errorf("pinned conn received %v, want [scroll_down]", got)
	}
}

// Unpinning — by empty conn_id or off:true — hands focus back to the OS:
// Terminal in front means no browser, and the browser tags go. A claim the
// pinned browser made while pinned does not keep it focused.
func TestFocusUnpinRestoresOSFocus(t *testing.T) {
	for _, body := range []string{`{"conn_id":""}`, `{"off":true}`, `{"conn_id":"fox","off":true}`} {
		t.Run(body, func(t *testing.T) {
			h, log := newRoutingHost(t)
			connectBrowser(h, "fox", firefoxBundle)
			focusApp(h, "com.apple.Terminal")
			pin(t, h, `{"conn_id":"fox"}`)
			claimFocus(t, h, "fox") // the headless browser claims its own window
			h.mu.Lock()
			if timer := h.state.ClaimVerifyTimer; timer != nil {
				timer.Stop()
				h.state.ClaimVerifyTimer = nil
			}
			h.mu.Unlock()

			released := log.count("delete_active")
			st := pin(t, h, body)
			if st.Pinned {
				t.Fatalf("still pinned: %+v", st)
			}
			if conn, focused := focusedConn(h); conn != "" || focused {
				t.Errorf("after unpin: focused %q browser=%v, want none (Terminal in front)", conn, focused)
			}
			if log.count("delete_active") == released {
				t.Error("unpin did not release plugin.browser.active")
			}
			// And OS focus works as before.
			focusApp(h, firefoxBundle)
			if conn, _ := focusedConn(h); conn != "fox" {
				t.Errorf("OS focus on Firefox: focused %q, want fox", conn)
			}
		})
	}
}

// The pin never outlives its connection.
func TestFocusPinClearsOnDisconnect(t *testing.T) {
	h, log := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	foxClient, _ := connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, "com.apple.Terminal")
	pin(t, h, `{"conn_id":"fox"}`)
	released := log.count("delete_active")

	h.disconnectSSEClient("browser", "fox", foxClient)

	if c := h.pin.current(); c != "" {
		t.Fatalf("pin %q survived its connection", c)
	}
	if conn, focused := focusedConn(h); conn != "" || focused {
		t.Errorf("after disconnect: focused %q browser=%v, want none", conn, focused)
	}
	if log.count("delete_active") == released {
		t.Error("plugin.browser.active not released")
	}
	focusApp(h, chromeBundle)
	if conn, _ := focusedConn(h); conn != "chrome" {
		t.Errorf("OS focus on Chrome: focused %q, want chrome", conn)
	}
}

// dispatchRecorder stands in for the platform's dispatch op.
type dispatchRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *dispatchRecorder) dispatch(action string, _ map[string]any) error {
	r.mu.Lock()
	r.calls = append(r.calls, action)
	r.mu.Unlock()
	return nil
}

func (r *dispatchRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// newPinnedKeystrokeHost: the real keystroke path down to the dispatch RPC,
// which records; Chrome and Firefox connected, Terminal in front.
func newPinnedKeystrokeHost(t *testing.T) (*Host, *dispatchRecorder) {
	t.Helper()
	h, _ := newRoutingHost(t)
	rec := &dispatchRecorder{}
	h.pressShortcut = h.pressShortcutDefault
	h.dispatchPlatformRPC = rec.dispatch
	h.runAsync = func(f func()) { f() }
	connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)
	return h, rec
}

func suppressed(h *Host, kind string) []pinnedEffect {
	_, _, effects := h.pin.snapshot()
	var out []pinnedEffect
	for _, e := range effects {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// open_select's Alt+Down and focus_address_bar's Cmd/Ctrl+L, asked for by the
// pinned browser, are recorded and never dispatched — they would land in
// Terminal. Unpinned, with the browser really in front, the same requests
// reach the dispatch op (the control that the stub is on the real path).
func TestPinnedKeystrokesAreRecordedNotDispatched(t *testing.T) {
	h, rec := newPinnedKeystrokeHost(t)
	focusApp(h, "com.apple.Terminal")
	pin(t, h, `{"conn_id":"fox"}`)

	postDispatchResult(t, h, `{"action":"activate","ok":true,"taken":"focus","followup":"open_select","conn_id":"fox"}`)
	dispatch(h, "focus_address_bar")

	if n := rec.count(); n != 0 {
		t.Fatalf("dispatched %d platform action(s) while pinned, want 0", n)
	}
	got := suppressed(h, pinEffectDispatch)
	if len(got) != 2 {
		t.Fatalf("recorded %d suppressed dispatch(es), want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Action != "input.shortcut_by_name" || e.PinnedConn != "fox" || e.At.IsZero() {
			t.Errorf("record %+v", e)
		}
	}
	if got[0].Params["name"] != "down" || got[1].Params["name"] != "l" {
		t.Errorf("recorded keys %v, %v; want down (open_select) then l (address bar)", got[0].Params, got[1].Params)
	}

	pin(t, h, `{"off":true}`)
	focusApp(h, firefoxBundle)
	postDispatchResult(t, h, `{"action":"activate","ok":true,"taken":"focus","followup":"open_select","conn_id":"fox"}`)
	dispatch(h, "focus_address_bar")
	if n := rec.count(); n != 2 {
		t.Errorf("unpinned: dispatched %d, want 2", n)
	}
}

// The tab-desk moves go through the same gate.
func TestPinnedWindowMoveIsRecordedNotDispatched(t *testing.T) {
	h, rec := newPinnedKeystrokeHost(t)
	focusApp(h, "com.apple.Terminal")
	pin(t, h, `{"conn_id":"fox"}`)
	if err := h.dispatchPlatform("placement.move_to_space", map[string]any{"space": "2"}); err != nil {
		t.Fatalf("suppressed dispatch reported %v; the flow must see it done", err)
	}
	if rec.count() != 0 || len(suppressed(h, pinEffectDispatch)) != 1 {
		t.Errorf("dispatched %d, recorded %d; want 0 and 1", rec.count(), len(suppressed(h, pinEffectDispatch)))
	}
}

// The record is capped.
func TestPinEffectRecordIsCapped(t *testing.T) {
	var p focusPin
	p.set("fox")
	for i := 0; i < pinEffectCap+20; i++ {
		p.suppress(pinEffectDispatch, fmt.Sprint(i), nil)
	}
	_, _, effects := p.snapshot()
	if len(effects) != pinEffectCap || effects[len(effects)-1].Action != fmt.Sprint(pinEffectCap+19) {
		t.Errorf("kept %d, last %q", len(effects), effects[len(effects)-1].Action)
	}
}

// A problem while pinned is published, and its window is not shown — the
// would-show is recorded. Unpinned, the same problem shows.
func TestPinnedProblemPublishesWithoutShow(t *testing.T) {
	h, _ := newRoutingHost(t)
	var mu sync.Mutex
	var published []string
	shows := 0
	h.publishProblemDoc = func(doc branchkit.OutputState) error {
		mu.Lock()
		published = append(published, doc.Title)
		mu.Unlock()
		return nil
	}
	h.showProblemWindow = func() error { mu.Lock(); shows++; mu.Unlock(); return nil }
	t.Cleanup(func() {
		h.problems.mu.Lock()
		if h.problems.hideTimer != nil {
			h.problems.hideTimer.Stop()
		}
		h.problems.mu.Unlock()
	})
	connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, "com.apple.Terminal")
	pin(t, h, `{"conn_id":"fox"}`)

	h.reportProblemDefault(tabMoveFailed())
	mu.Lock()
	if len(published) != 1 || shows != 0 {
		t.Errorf("pinned: published %v, shown %d; want 1 and 0", published, shows)
	}
	mu.Unlock()
	got := suppressed(h, pinEffectProblemShow)
	if len(got) != 1 || got[0].Action != "tab_move_failed" || got[0].Params["title"] != tabNotMoved {
		t.Errorf("recorded %+v", got)
	}

	pin(t, h, `{"off":true}`)
	h.reportProblemDefault(tabMoveFailed())
	mu.Lock()
	if len(published) != 2 || shows != 1 {
		t.Errorf("unpinned: published %d, shown %d; want 2 and 1", len(published), shows)
	}
	mu.Unlock()
}

// Pin, unpin, OS focus, claims, dispatches, reads and a disconnect all at
// once, under -race. Whatever interleaving, a standing pin is the focused
// connection once the dust settles, and no platform action got through
// while one was up.
func TestFocusPinConcurrent(t *testing.T) {
	h, rec := newPinnedKeystrokeHost(t)
	h.runAsync = func(f func()) { go f() }
	h.frontmostBundle = func() (string, error) { return "com.apple.Terminal", nil }
	extraClient, _ := connectBrowser(h, "extra", "org.mozilla.nightly")
	focusApp(h, "com.apple.Terminal")
	pin(t, h, `{"conn_id":"fox"}`)

	// No t.Fatal off the test goroutine: these post straight to the handlers.
	post := func(path, body string) {
		serveRoute(h, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	}
	var wg sync.WaitGroup
	run := func(n int, f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				f(i)
			}
		}()
	}
	run(50, func(i int) {
		body := `{"conn_id":"fox"}`
		if i%2 == 0 {
			body = `{"off":true}`
		}
		post("/dev/focus-pin", body)
	})
	run(50, func(i int) {
		focusApp(h, []string{chromeBundle, "com.apple.Terminal", firefoxBundle}[i%3])
	})
	run(50, func(int) { post("/focus", `{"conn_id":"chrome","focused":true}`) })
	run(50, func(int) { _ = h.dispatchPlatformAction("input.shortcut_by_name", nil) })
	run(50, func(int) { _ = h.focusPinStateSnapshot() })
	run(1, func(int) { h.disconnectSSEClient("browser", "extra", extraClient) })
	wg.Wait()

	pin(t, h, `{"conn_id":"fox"}`)
	focusApp(h, chromeBundle)
	if conn, focused := focusedConn(h); conn != "fox" || !focused {
		t.Errorf("settled: focused %q browser=%v, want the pin", conn, focused)
	}
	before := rec.count()
	_ = h.dispatchPlatformAction("input.shortcut_by_name", nil)
	if rec.count() != before {
		t.Error("a dispatch got through a standing pin")
	}
	h.mu.Lock()
	if timer := h.state.ClaimVerifyTimer; timer != nil {
		timer.Stop()
	}
	h.mu.Unlock()
}
