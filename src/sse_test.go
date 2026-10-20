package main

import "testing"

func TestRemotePortFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{"127.0.0.1:50741", 50741},
		{"[::1]:50741", 50741},
		{"127.0.0.1", 0}, // no port
		{"garbage", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := remotePortFromAddr(c.addr); got != c.want {
			t.Errorf("remotePortFromAddr(%q) = %d, want %d", c.addr, got, c.want)
		}
	}
}

func TestFocusedSourceFromAsserted(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		FocusedBundleID: "com.google.Chrome",
		Conns: conns(map[string]Conn{
			"chrome": {AssertedBundle: "com.google.Chrome"},
			"fox":    {AssertedBundle: "org.mozilla.firefox"},
		}),
	}
	// OS frontmost is Chrome → the conn asserting Chrome is the focused source.
	if got := h.focusedSourceFromAsserted(); got != "chrome" {
		t.Errorf("focusedSourceFromAsserted = %q, want chrome", got)
	}

	// OS frontmost is a non-browser no conn asserts → no focused browser source
	// (this is the Warp/CoreLocationAgent case — never mispairs to a browser).
	h.state.FocusedBundleID = "com.apple.CoreLocationAgent"
	if got := h.focusedSourceFromAsserted(); got != "" {
		t.Errorf("focusedSourceFromAsserted on non-browser = %q, want empty", got)
	}

	// Nothing focused yet.
	h.state.FocusedBundleID = ""
	if got := h.focusedSourceFromAsserted(); got != "" {
		t.Errorf("focusedSourceFromAsserted with no OS bundle = %q, want empty", got)
	}
}

func TestRecomputeFocusedSourceFromAsserted(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
		Conns: conns(map[string]Conn{
			"chrome": {AssertedBundle: "com.google.Chrome", ActiveTab: 7},
			"fox":    {AssertedBundle: "org.mozilla.firefox", ActiveTab: 9},
		}),
		SSEClientBundles: map[string]string{},
	}
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)

	// Chrome frontmost → Chrome's connection is the focused source.
	h.state.FocusedBundleID = "com.google.Chrome"
	if isBrowser, _ := h.recomputeFocusedSourceLocked("test"); !isBrowser {
		t.Fatal("expected browser focused when frontmost is a connected browser")
	}
	if h.state.FocusedConnID != "chrome" || h.state.FocusedTabID != 7 || !h.state.BrowserFocused {
		t.Errorf("focused source = %q/tab%d/browser%v, want chrome/7/true",
			h.state.FocusedConnID, h.state.FocusedTabID, h.state.BrowserFocused)
	}

	// A background agent steals focus → released, NEVER mispaired to a browser
	// (this is the CoreLocationAgent/Warp class that drained the grammar).
	h.state.FocusedBundleID = "com.apple.CoreLocationAgent"
	if isBrowser, _ := h.recomputeFocusedSourceLocked("test"); isBrowser {
		t.Error("non-browser frontmost must not report a focused browser")
	}
	if h.state.FocusedConnID != "" || h.state.BrowserFocused {
		t.Errorf("expected release on non-browser focus, got conn=%q browser=%v",
			h.state.FocusedConnID, h.state.BrowserFocused)
	}

	// Return to Chrome → refocus (FocusedBrowserBundleID stayed sticky as Chrome).
	h.state.FocusedBundleID = "com.google.Chrome"
	h.recomputeFocusedSourceLocked("test")
	if h.state.FocusedConnID != "chrome" {
		t.Errorf("expected chrome refocus, got %q", h.state.FocusedConnID)
	}

	// Switch to Firefox → Firefox becomes the focused source with its own tab.
	h.state.FocusedBundleID = "org.mozilla.firefox"
	h.recomputeFocusedSourceLocked("test")
	if h.state.FocusedConnID != "fox" || h.state.FocusedTabID != 9 {
		t.Errorf("expected fox/9, got %q/%d", h.state.FocusedConnID, h.state.FocusedTabID)
	}
}

// Direct browser→browser switches:
// recovery keys on the recompute TRANSITION, not on the callers'
// idle→browser arm edge — A→B→A with no non-browser in between never
// produces an edge. Under the last mile, a switch PARKS the loser's
// sessions instead of wiping them, so the switch-back reprojects the
// kept grammar with zero extension traffic and needsRescan stays false
// throughout. (Pre-park, this test asserted the switch-back NEEDED a
// rescan — that recovery republish is now reserved for genuinely-empty
// projections: SSE disconnect, plugin restart.)
func TestRecomputeNeedsRescanOnDirectSwitchBack(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
		Conns: conns(map[string]Conn{
			"chrome": {AssertedBundle: "com.google.Chrome", ActiveTab: 7},
			"fox":    {AssertedBundle: "org.mozilla.firefox", ActiveTab: 9},
		}),
		SSEClientBundles: map[string]string{},
		HintVisibility:   "always",
	}
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)

	// Both browsers hold grammar for their active tabs.
	chrome := h.ensureFrameSession(frameKey{ConnID: "chrome", TabID: 7, FrameID: 0})
	chrome.Codewords["arch bake"] = map[string]string{"id": "1"}
	fox := h.ensureFrameSession(frameKey{ConnID: "fox", TabID: 9, FrameID: 0})
	fox.Codewords["zoo cane"] = map[string]string{"id": "2"}

	// Chrome takes focus: transition, projection restores Chrome's grammar.
	h.state.FocusedBundleID = "com.google.Chrome"
	if _, needsRescan := h.recomputeFocusedSourceLocked("test"); needsRescan {
		t.Error("Chrome focus with live grammar must not need a rescan")
	}

	// Direct switch to Firefox: Chrome (the loser) is PARKED; Firefox's own
	// sessions reproject — no rescan needed for Firefox.
	h.state.FocusedBundleID = "org.mozilla.firefox"
	if _, needsRescan := h.recomputeFocusedSourceLocked("test"); needsRescan {
		t.Error("Firefox focus with live grammar must not need a rescan")
	}
	// Chrome's session survived the switch, parked.
	chromeSess := h.state.FrameSessions[frameKey{ConnID: "chrome", TabID: 7, FrameID: 0}]
	if chromeSess == nil || !chromeSess.SweptStale {
		t.Fatalf("loser's sessions must be parked (kept + SweptStale), got %+v", chromeSess)
	}
	if _, ok := chromeSess.Codewords["arch bake"]; !ok {
		t.Error("parked session must keep its codewords")
	}

	// Direct switch BACK to Chrome: the parked sessions revive and
	// reproject — grammar restored with zero extension traffic, no rescan.
	// (No idle→browser edge anywhere in this A→B→A cycle; the transition
	// keying is what makes each hop exactly-once regardless.)
	h.state.FocusedBundleID = "com.google.Chrome"
	if _, needsRescan := h.recomputeFocusedSourceLocked("test"); needsRescan {
		t.Error("switch-back to a PARKED browser must reproject, not rescan")
	}
	if chromeSess.SweptStale {
		t.Error("switch-back must revive the parked focused-tab session")
	}

	// Second signal for the same gain (the laggy OS event): no transition,
	// no rescan — the dedup that replaced the edge guard's job.
	if _, needsRescan := h.recomputeFocusedSourceLocked("test"); needsRescan {
		t.Error("a no-op recompute for the same focus state must not re-fire the rescan")
	}
}
