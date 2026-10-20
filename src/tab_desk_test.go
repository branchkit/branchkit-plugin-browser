package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

func cgWin(id string, x, y, w, h int, title string) branchkit.WindowInfo {
	return branchkit.WindowInfo{ID: id, X: x, Y: y, W: w, H: h, Title: title}
}

func extWin(id, left, top, w, h int, state, tabTitle string) extWindowInfo {
	return extWindowInfo{WindowID: id, Left: left, Top: top, Width: w, Height: h, State: state, ActiveTabTitle: tabTitle}
}

func pick(cands []branchkit.WindowInfo, exts []extWindowInfo) (branchkit.WindowInfo, extWindowInfo, bool, *branchkit.WindowInfo) {
	return pickByFrame(cands, eligibleSurgeryWindows(exts))
}

func TestPickByFrameExactFrame(t *testing.T) {
	cands := []branchkit.WindowInfo{cgWin("101", 0, 25, 1440, 875, "Docs - Google Chrome")}
	exts := []extWindowInfo{
		extWin(7, 0, 25, 1440, 875, "normal", "Docs"),
		extWin(8, 200, 200, 800, 600, "normal", "Other"),
	}
	cg, ext, ok, _ := pick(cands, exts)
	if !ok || ext.WindowID != 7 || cg.ID != "101" {
		t.Fatalf("expected match cg=101 ext=7, got ok=%v cg=%s ext=%d", ok, cg.ID, ext.WindowID)
	}
}

func TestPickByFrameToleratesRounding(t *testing.T) {
	cands := []branchkit.WindowInfo{cgWin("101", 0, 25, 1440, 875, "")}
	exts := []extWindowInfo{extWin(7, 4, 22, 1436, 880, "normal", "")}
	if _, ext, ok, _ := pick(cands, exts); !ok || ext.WindowID != 7 {
		t.Fatalf("within-tolerance frame should match, got ok=%v", ok)
	}
}

func TestPickByFrameRejectsBeyondTolerance(t *testing.T) {
	cands := []branchkit.WindowInfo{cgWin("101", 0, 25, 1440, 875, "")}
	exts := []extWindowInfo{extWin(7, 50, 25, 1440, 875, "normal", "")}
	if _, _, ok, amb := pick(cands, exts); ok || amb != nil {
		t.Fatal("frame 50px off should neither match nor be ambiguous")
	}
}

func TestPickByFrameSkipsMinimizedFullscreenAndSource(t *testing.T) {
	cands := []branchkit.WindowInfo{cgWin("101", 0, 25, 1440, 875, "")}
	exts := []extWindowInfo{
		extWin(7, 0, 25, 1440, 875, "minimized", ""),
		extWin(8, 0, 25, 1440, 875, "fullscreen", ""),
		{WindowID: 9, Left: 0, Top: 25, Width: 1440, Height: 875, State: "normal", IsSource: true},
	}
	if _, _, ok, amb := pick(cands, exts); ok || amb != nil {
		t.Fatal("minimized/fullscreen/source ext windows must not be targets")
	}
}

func TestPickByFrameTitleTieBreak(t *testing.T) {
	// Two same-frame ext windows — a non-empty CG title decides.
	cands := []branchkit.WindowInfo{cgWin("101", 0, 25, 1440, 875, "Release notes - Google Chrome")}
	exts := []extWindowInfo{
		extWin(7, 0, 25, 1440, 875, "normal", "Dashboard"),
		extWin(8, 0, 25, 1440, 875, "normal", "Release notes"),
	}
	_, ext, ok, _ := pick(cands, exts)
	if !ok || ext.WindowID != 8 {
		t.Fatalf("title tie-break should pick 8, got ok=%v ext=%d", ok, ext.WindowID)
	}
}

func TestPickByFrameAmbiguousWithoutTitles(t *testing.T) {
	// The 2026-07-25 field layout: three full-size Chrome windows on three
	// desks share one frame, CG titles empty (no Screen Recording
	// permission), source excluded — the remaining two hits are
	// indistinguishable. pickByFrame must refuse to guess (the old
	// hits[0] guess landed the tab in a window on a different desk) and
	// surface the candidate for the identity probe.
	cands := []branchkit.WindowInfo{cgWin("11877", 0, 25, 2560, 1415, "")}
	exts := []extWindowInfo{
		{WindowID: 148767404, Left: 0, Top: 25, Width: 2560, Height: 1415, State: "normal", IsSource: true},
		extWin(148767621, 0, 25, 2560, 1415, "normal", "Repo"),
		extWin(148767999, 0, 25, 2560, 1415, "normal", "Mail"),
	}
	_, _, ok, amb := pick(cands, exts)
	if ok {
		t.Fatal("indistinguishable same-frame windows must not produce a confident match")
	}
	if amb == nil || amb.ID != "11877" {
		t.Fatalf("ambiguous candidate should surface for probing, got %v", amb)
	}
}

func TestPickByFramePrefersUniqueCandidateOverAmbiguous(t *testing.T) {
	// First candidate is frame-ambiguous, second correlates uniquely — the
	// unique one wins without a probe.
	cands := []branchkit.WindowInfo{
		cgWin("101", 0, 25, 2560, 1415, ""),
		cgWin("102", 100, 100, 900, 700, ""),
	}
	exts := []extWindowInfo{
		extWin(7, 0, 25, 2560, 1415, "normal", ""),
		extWin(8, 0, 25, 2560, 1415, "normal", ""),
		extWin(9, 100, 100, 900, 700, "normal", ""),
	}
	cg, ext, ok, _ := pick(cands, exts)
	if !ok || cg.ID != "102" || ext.WindowID != 9 {
		t.Fatalf("expected unique candidate 102→9, got ok=%v cg=%s ext=%d", ok, cg.ID, ext.WindowID)
	}
}

func TestFindNudgedCGWindowExactWidth(t *testing.T) {
	// The reverse probe shrinks the EXT window 1px; the un-nudged CG twins
	// differ by exactly that 1px, so selection must be width-exact — the
	// usual ±8 tolerance would defeat it.
	e := extWin(148767621, 0, 25, 2560, 1415, "normal", "")
	wins := []branchkit.WindowInfo{
		cgWin("11877", 0, 25, 2559, 1415, ""), // observed the nudge
		cgWin("12364", 0, 25, 2560, 1415, ""), // un-nudged twin
	}
	w, ok := findNudgedCGWindow(e, 2559, wins)
	if !ok || w.ID != "11877" {
		t.Fatalf("expected nudged CG window 11877, got ok=%v id=%s", ok, w.ID)
	}
}

func TestFindNudgedCGWindowRejectsMultipleHits(t *testing.T) {
	e := extWin(148767621, 0, 25, 2560, 1415, "normal", "")
	wins := []branchkit.WindowInfo{
		cgWin("1", 0, 25, 2559, 1415, ""),
		cgWin("2", 0, 25, 2559, 1415, ""),
	}
	if _, ok := findNudgedCGWindow(e, 2559, wins); ok {
		t.Fatal("two CG windows at the nudged width must not identify")
	}
}

func TestDispatchActionWireShape(t *testing.T) {
	// The dispatch RPC takes the platform's tagged Action enum: `type` is
	// the VARIANT discriminator ("plugin"), the dotted action name rides in
	// `action_type`. The flattened commands.json shape
	// ({"type": "placement.snap"}) is rejected with "unknown variant" — this
	// pins the contract after that exact field bug (2026-07-25). The typed
	// `branchkit.Action` makes the bug unspellable; this still checks the
	// JSON it serializes to, which is what the platform actually reads.
	a, err := buildDispatchAction("placement.move_to_space", map[string]any{"space": "3"})
	if err != nil {
		t.Fatalf("buildDispatchAction: %v", err)
	}
	if a.Type != branchkit.ActionKindPlugin {
		t.Fatalf(`Type must be the variant tag "plugin", got %v`, a.Type)
	}
	if a.ActionType == nil || *a.ActionType != "placement.move_to_space" {
		t.Fatalf("ActionType mismatch: %v", a.ActionType)
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["type"] != "plugin" {
		t.Fatalf(`wire type must be "plugin", got %v`, wire["type"])
	}
	if wire["action_type"] != "placement.move_to_space" {
		t.Fatalf("wire action_type mismatch: %v", wire["action_type"])
	}
	params, ok := wire["params"].(map[string]any)
	if !ok || params["space"] != "3" {
		t.Fatalf("params not nested under key: %v", wire["params"])
	}
}

func TestSurgeryReplyDelivery(t *testing.T) {
	h := newTestHost()
	ch := make(chan surgeryReply, 1)
	h.surgeryMu.Lock()
	h.surgeryWaiters["req1"] = ch
	h.surgeryMu.Unlock()
	defer func() {
		h.surgeryMu.Lock()
		delete(h.surgeryWaiters, "req1")
		h.surgeryMu.Unlock()
	}()

	h.deliverSurgeryReply(surgeryReply{RequestID: "unknown", OK: true}) // dropped, no panic
	h.deliverSurgeryReply(surgeryReply{RequestID: "req1", OK: true, Windows: []extWindowInfo{{WindowID: 3}}})
	h.deliverSurgeryReply(surgeryReply{RequestID: "req1", OK: false}) // double reply — first wins

	select {
	case r := <-ch:
		if !r.OK || len(r.Windows) != 1 || r.Windows[0].WindowID != 3 {
			t.Fatalf("unexpected reply: %+v", r)
		}
	default:
		t.Fatal("reply not delivered")
	}
	select {
	case r := <-ch:
		t.Fatalf("second reply should have been dropped, got %+v", r)
	default:
	}
}

// --- Tab-move failures are told ---
//
// The flows run off the on_action reply, so a failure used to reach only the
// log. Each is now a problem the window shows and speech reads.

const (
	deskActiveWin = "10"
	deskSpace1    = 101
	deskSpace2    = 102
)

// newTabMoveHost is a problem host with Chrome in front, two desks, and a
// world model where Chrome's one window is the active one on desk 1. The
// platform dispatch is stubbed to succeed; tests override what they break.
func newTabMoveHost(t *testing.T) (*Host, *problemRecorder, chan ActionEvent) {
	t.Helper()
	h, rec, _ := newProblemHost(t)
	_, ch := connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)
	received(ch)
	h.listSpaces = func() ([]branchkit.SpaceInfo, error) {
		return []branchkit.SpaceInfo{
			{SpaceID: deskSpace1, SpaceType: "user"},
			{SpaceID: 900, SpaceType: "fullscreen"},
			{SpaceID: deskSpace2, SpaceType: "user"},
		}, nil
	}
	active := deskActiveWin
	h.worldModel = func() (*branchkit.WorldModel, error) {
		return &branchkit.WorldModel{
			ActiveWindowID: &active,
			Windows: []branchkit.WindowInfo{
				{ID: deskActiveWin, AppID: chromeBundle, X: 0, Y: 25, W: 1200, H: 800, SpaceIds: []int{deskSpace1}},
			},
		}, nil
	}
	h.dispatchPlatform = func(string, map[string]any) error { return nil }
	return h, rec, ch
}

// answerSurgery plays the extension: every surgery request on ch gets the
// reply `answer` builds, until the test ends.
func answerSurgery(t *testing.T, h *Host, ch chan ActionEvent, answer func(ActionEvent) surgeryReply) {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	t.Cleanup(func() { close(done); <-stopped })
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case evt := <-ch:
				id := evt.Param("request_id")
				if id == "" {
					continue
				}
				r := answer(evt)
				r.RequestID = id
				h.deliverSurgeryReply(r)
			}
		}
	}()
}

func wantTabProblem(t *testing.T, rec *problemRecorder, reason string, says ...string) {
	t.Helper()
	p := rec.last(t)
	if p.Reason != reason || !p.Show || p.Title == "" {
		t.Errorf("problem = %+v, want a shown %s", p, reason)
	}
	for _, s := range says {
		if !strings.Contains(p.Phrase, s) {
			t.Errorf("phrase %q should say %q", p.Phrase, s)
		}
	}
}

func TestTabMoveWithNoBrowserInFrontIsTold(t *testing.T) {
	h, rec, _ := newTabMoveHost(t)
	focusApp(h, "com.apple.Terminal")
	h.runTabToDesk(2, false, "")
	wantTabProblem(t, rec, dispatchNotFocused)
	rec.seen = nil
	h.runTabToWindow(2, "")
	wantTabProblem(t, rec, dispatchNotFocused)
}

// popSucceeds plays an extension that pops the tab into a new window, which
// the world model then shows (and did not before the pop).
func popSucceeds(t *testing.T, h *Host, ch chan ActionEvent) {
	t.Helper()
	var popped atomic.Bool
	answerSurgery(t, h, ch, func(ActionEvent) surgeryReply {
		popped.Store(true)
		return surgeryReply{OK: true, Window: &extWindowInfo{WindowID: 5, Left: 0, Top: 25, Width: 1200, Height: 800}}
	})
	base := h.worldModel
	h.worldModel = func() (*branchkit.WorldModel, error) {
		wm, _ := base()
		if popped.Load() {
			wm.Windows = append(wm.Windows, branchkit.WindowInfo{ID: "11", AppID: chromeBundle, X: 0, Y: 25, W: 1200, H: 800, SpaceIds: []int{deskSpace1}})
		}
		return wm, nil
	}
}

func TestTabToAMissingDeskIsTold(t *testing.T) {
	h, rec, _ := newTabMoveHost(t)
	h.runTabToDesk(5, false, "")
	wantTabProblem(t, rec, "no_such_desk", "desk 5", "have 2")

	rec.seen = nil
	h.interceptTabSurgeryAction("tab_to_desk", map[string]any{"desk": "20"})
	wantTabProblem(t, rec, "no_such_desk", "desk 20")
}

func TestTabToDeskWithUnreadableDesktopsIsTold(t *testing.T) {
	h, rec, _ := newTabMoveHost(t)
	h.listSpaces = func() ([]branchkit.SpaceInfo, error) { return nil, errors.New("no spaces") }
	h.runTabToDesk(2, false, "")
	wantTabProblem(t, rec, "tab_move_failed", "desktops")
}

func TestTabAlreadyOnTheDeskIsTold(t *testing.T) {
	h, rec, _ := newTabMoveHost(t)
	h.runTabToDesk(1, false, "")
	wantTabProblem(t, rec, "already_there", "desk 1")
}

// The extension refuses the pop: the tab never left.
func TestTabPopFailureIsTold(t *testing.T) {
	h, rec, ch := newTabMoveHost(t)
	answerSurgery(t, h, ch, func(ActionEvent) surgeryReply { return surgeryReply{OK: false, Error: "windows.create failed"} })
	h.runTabToDesk(2, false, "")
	wantTabProblem(t, rec, "tab_move_failed", "didn't move")
}

// The pop landed and the placement plugin could not send the new window on:
// the tab is stranded in a new window here, and the person is told where.
func TestTabStrandedByAFailedDeskMoveIsTold(t *testing.T) {
	h, rec, ch := newTabMoveHost(t)
	popSucceeds(t, h, ch)
	h.dispatchPlatform = func(string, map[string]any) error { return errors.New("placement plugin: no permission") }
	h.runTabToDesk(2, false, "")
	wantTabProblem(t, rec, "tab_move_failed", "new window here", "desk 2")
}

func TestTabMoveThatWorksIsNotTold(t *testing.T) {
	h, rec, ch := newTabMoveHost(t)
	popSucceeds(t, h, ch)
	var sent []string
	h.dispatchPlatform = func(action string, _ map[string]any) error { sent = append(sent, action); return nil }
	h.runTabToDesk(2, false, "")
	if len(rec.seen) != 0 {
		t.Errorf("a tab move that worked told %+v", rec.seen)
	}
	if len(sent) != 1 || sent[0] != "placement.move_to_space" {
		t.Errorf("platform dispatches = %v, want one placement.move_to_space", sent)
	}
}

func TestTabToAMissingWindowIsTold(t *testing.T) {
	h, rec, _ := newTabMoveHost(t)
	h.runTabToWindow(3, "")
	wantTabProblem(t, rec, "no_such_window", "window 3", "has 1")

	rec.seen = nil
	h.runTabToWindow(1, "")
	wantTabProblem(t, rec, "already_there", "window 1")
}
