package main

import (
	"testing"
	"time"
)

// batchIsFocusedSourceLocked is the self-describing projection gate that
// replaced the tab-id-vs-/active-tab comparison.
// It admits a batch iff it comes from the OS-focused browser's OS-focused
// window's visible tab, failing open only where a focus input hasn't landed yet.
func TestBatchIsFocusedSourceLocked(t *testing.T) {
	h := newTestHost()
	cases := []struct {
		name        string
		focusedConn string
		focusedWin  map[string]int
		req         GrammarFrameRequest
		want        bool
	}{
		{
			name:        "focused conn + focused window + visible → focused",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{"conn-a": 10},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 10, Visible: true},
			want:        true,
		},
		{
			name:        "wrong conn → not focused",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{"conn-a": 10},
			req:         GrammarFrameRequest{ConnID: "conn-b", WindowID: 10, Visible: true},
			want:        false,
		},
		{
			name:        "focused conn, non-focused window → not focused (multi-window guard)",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{"conn-a": 10},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 11, Visible: true},
			want:        false,
		},
		{
			name:        "focused conn + focused window but hidden → not focused (background tab)",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{"conn-a": 10},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 10, Visible: false},
			want:        false,
		},
		{
			name:        "cold start: no focused conn yet, visible → fail open on conn",
			focusedConn: "",
			focusedWin:  map[string]int{},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 10, Visible: true},
			want:        true,
		},
		{
			name:        "focused conn, focused window unknown yet, visible → fail open on window",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{}, // no /focus recorded a window for conn-a
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 10, Visible: true},
			want:        true,
		},
		{
			name:        "focused conn + focused window known, batch window_id unstamped (0), visible → fail open on window",
			focusedConn: "conn-a",
			focusedWin:  map[string]int{"conn-a": 10},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 0, Visible: true},
			want:        true,
		},
		{
			name:        "cold start but hidden → still not focused (visible always governs)",
			focusedConn: "",
			focusedWin:  map[string]int{},
			req:         GrammarFrameRequest{ConnID: "conn-a", WindowID: 10, Visible: false},
			want:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.state = &PluginState{
				FocusedConnID: tc.focusedConn,
				Conns:         windowConns(tc.focusedWin),
			}
			if got := h.batchIsFocusedSourceLocked(&tc.req); got != tc.want {
				t.Errorf("batchIsFocusedSourceLocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// batchWindowConfirmedLocked is the stronger gate gating tab ADOPTION: we only
// reassign FocusedTabID off a batch when we positively know it is the focused
// window (both ids present and equal). Fail-open admission (unknown window) must
// NOT confirm, so two cold-start windows can't thrash FocusedTabID.
func TestBatchWindowConfirmedLocked(t *testing.T) {
	h := newTestHost()
	cases := []struct {
		name       string
		focusedWin map[string]int
		req        GrammarFrameRequest
		want       bool
	}{
		{
			name:       "window known and matches → confirmed",
			focusedWin: map[string]int{"conn-a": 10},
			req:        GrammarFrameRequest{ConnID: "conn-a", WindowID: 10},
			want:       true,
		},
		{
			name:       "window known, batch differs → not confirmed",
			focusedWin: map[string]int{"conn-a": 10},
			req:        GrammarFrameRequest{ConnID: "conn-a", WindowID: 11},
			want:       false,
		},
		{
			name:       "focused window unknown (cold start) → not confirmed (no adoption, no thrash)",
			focusedWin: map[string]int{},
			req:        GrammarFrameRequest{ConnID: "conn-a", WindowID: 10},
			want:       false,
		},
		{
			name:       "batch window unstamped (0) → not confirmed",
			focusedWin: map[string]int{"conn-a": 10},
			req:        GrammarFrameRequest{ConnID: "conn-a", WindowID: 0},
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.state = &PluginState{Conns: windowConns(tc.focusedWin)}
			if got := h.batchWindowConfirmedLocked(&tc.req); got != tc.want {
				t.Errorf("batchWindowConfirmedLocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// A self-describing visible batch that names a tab the plugin didn't have as
// focused (a lost/lagging /active-tab) is authoritative: it adopts the tab,
// parks the previously-focused tab's projection, and reprojects — no rescan
// round-trip. This is the STORED_FOCUSED_DRIFT self-heal.
func TestAdoptFocusedTabFromBatch(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
		Conns:         conns(map[string]Conn{"conn-a": {FocusedWindow: 10}}),
		FocusedConnID: "conn-a",
		FocusedTabID:  1,
	}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	// Tab 1 is focused and projected; tab 2 has stored (background) grammar.
	tab1 := h.ensureFrameSession(frameKey{ConnID: "conn-a", TabID: 1, FrameID: 0})
	tab1.Codewords["arch bake"] = map[string]string{"id": "1", "codeword": "arch bake"}
	tab2 := h.ensureFrameSession(frameKey{ConnID: "conn-a", TabID: 2, FrameID: 0})
	tab2.Codewords["cape dart"] = map[string]string{"id": "2", "codeword": "cape dart"}
	// Tab 2 aged in the background past the dormant TTL — revive must un-park it.
	tab2.LastBatchAt = time.Now().Add(-2 * staleFrameSessionTTL)
	tab2.SweptStale = true

	h.reprojectFocusedSourceLocked("browser")
	if _, ok := pushes["plugin.browser.hints_arch"]["bake"]; !ok {
		t.Fatalf("precondition: tab 1 must be projected, got %v", pushes)
	}

	// A visible batch arrives for tab 2 (the user actually switched, /active-tab
	// was lost). Adopt it.
	h.adoptFocusedTabFromBatchLocked("conn-a", 2)

	if h.state.FocusedTabID != 2 {
		t.Errorf("FocusedTabID = %d, want 2 (batch is authoritative)", h.state.FocusedTabID)
	}
	if h.state.peek("conn-a").ActiveTab != 2 {
		t.Errorf("conn-a ActiveTab = %d, want 2 (fast-path kept coherent)", h.state.peek("conn-a").ActiveTab)
	}
	if tab2.SweptStale {
		t.Error("adopted tab's sessions must be revived (SweptStale cleared)")
	}
	// Tab 1's contribution drained; tab 2's projected.
	if len(pushes["plugin.browser.hints_arch"]) != 0 {
		t.Errorf("old tab 1 must be deprojected, arch still has %v", pushes["plugin.browser.hints_arch"])
	}
	if _, ok := pushes["plugin.browser.hints_cape"]["dart"]; !ok {
		t.Errorf("adopted tab 2 must be reprojected, got %v", pushes["plugin.browser.hints_cape"])
	}
	// Tab 1's session survives (parked) for a zero-traffic switch-back.
	if s := h.state.FrameSessions[frameKey{ConnID: "conn-a", TabID: 1, FrameID: 0}]; s == nil || len(s.Codewords) != 1 {
		t.Error("old tab session must survive the adoption (keep-but-deproject)")
	}
}

// A same-tab batch (no drift) is a no-op adoption — FocusedTabID unchanged, no
// deproject churn.
func TestAdoptFocusedTabFromBatchNoOpSameTab(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
		Conns:         conns(map[string]Conn{"conn-a": {ActiveTab: 5, FocusedWindow: 10}}),
		FocusedConnID: "conn-a",
		FocusedTabID:  5,
	}
	h.adoptFocusedTabFromBatchLocked("conn-a", 5)
	if h.state.FocusedTabID != 5 {
		t.Errorf("FocusedTabID = %d, want 5 (unchanged)", h.state.FocusedTabID)
	}
}
