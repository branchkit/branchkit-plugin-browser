package main

// Tab-to-desk / tab-to-window orchestration.
//
// "stash tab desk 3" / "blank tab desk 3" move the ACTIVE tab of the focused
// browser to another Mission Control desktop; "stash tab window 2" moves it
// into another window of the same browser on any desk. The tab surgery itself
// runs in the extension (tabs.move / windows.create — cross-browser, works on
// Firefox where AppleScript can't); the window→desktop move is dispatched
// through the platform to the placement plugin (dispatch_via: direct,
// dispatch_prefixes: ["placement"]).
//
// Merge is do-what-I-mean: a same-browser window already on the target desk
// receives the tab silently (pure tabs.move, no space switch); otherwise the
// tab pops into a new window here and rides the placement plugin's visible
// drag-move. The extension↔CG window correlation is per-command frame
// matching — no persistent mapping to drift.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

const (
	surgeryQueryTimeout = 2 * time.Second
	surgeryMoveTimeout  = 4 * time.Second
	surgeryPopTimeout   = 4 * time.Second

	// CGWindowList can lag windows.create by a frame or two; poll briefly.
	cgAppearPollInterval = 100 * time.Millisecond
	cgAppearPollBudget   = 750 * time.Millisecond

	// Frame-correlation tolerance between WebExtension window bounds and
	// kCGWindowBounds, both global top-left-origin points. A few px of
	// rounding slack; real matches are exact or near-exact.
	frameMatchTolerance = 8
)

// extWindowInfo is one window as the extension reports it (query_windows /
// pop_active_tab replies over POST /surgery/result).
type extWindowInfo = wire.WindowInfo

// surgeryReply is the body of POST /surgery/result (wire.SurgeryResult). One
// envelope for every surgery action; unused fields stay zero.
type surgeryReply = wire.SurgeryResult

var ()

// queryWindowsParams is query_windows's params: the request id alone.
func queryWindowsParams(id string) any { return wire.QueryWindowsParams{RequestID: id} }

func newSurgeryRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// deliverSurgeryReply routes an inbound /surgery/result body to its waiter.
// A late reply (waiter timed out and unregistered) is dropped silently — the
// initiating flow already logged the timeout.
func (h *Host) deliverSurgeryReply(r surgeryReply) {
	h.surgeryMu.Lock()
	ch, ok := h.surgeryWaiters[r.RequestID]
	h.surgeryMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- r:
	default: // double reply — first one wins
	}
}

// sendSurgeryAndWait pushes one surgery ActionEvent to the focused browser's
// SSE stream and blocks until its /surgery/result reply or the timeout. The
// first plugin→extension request/response on this transport; generic on
// purpose so future queries reuse it.
//
// params builds the action's typed params (wire.*Params) around the request id
// minted here.
func (h *Host) sendSurgeryAndWait(action string, params func(requestID string) any, corr string, timeout time.Duration) (*surgeryReply, error) {
	id := newSurgeryRequestID()
	ch := make(chan surgeryReply, 1)
	h.surgeryMu.Lock()
	h.surgeryWaiters[id] = ch
	h.surgeryMu.Unlock()
	defer func() {
		h.surgeryMu.Lock()
		delete(h.surgeryWaiters, id)
		h.surgeryMu.Unlock()
	}()

	event := wire.NewAction(action, params(id))
	event.CorrelationID = corr
	h.mu.Lock()
	sent, reason := h.sendToFocusedClientsLocked(event)
	h.mu.Unlock()
	if sent == 0 {
		// Nothing will ever reply; fail now rather than after the timeout.
		return nil, fmt.Errorf("%s: not delivered: %s", action, reason)
	}

	select {
	case r := <-ch:
		if !r.OK {
			return nil, fmt.Errorf("%s: extension reported: %s", action, r.Error)
		}
		return &r, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("%s: no extension reply within %s", action, timeout)
	}
}

// noSuchDeskError is resolveDeskSpace's answer for a desk that does not
// exist — the person's mistake, told as such rather than as a failure.
type noSuchDeskError struct{ desk, have int }

func (e noSuchDeskError) Error() string { return fmt.Sprintf("no desk %d (have %d)", e.desk, e.have) }

// resolveDeskSpace maps a Mission Control desk ordinal (the Ctrl+N index,
// user spaces counted in managed-display order — same convention as the
// placement plugin) to its space id. Returns a noSuchDeskError when the desk
// doesn't exist.
func (h *Host) resolveDeskSpace(desk int) (int, error) {
	spaces, err := h.listSpaces()
	if err != nil {
		return 0, fmt.Errorf("list_spaces: %w", err)
	}
	ordinal := 0
	for _, s := range spaces {
		if s.SpaceType != "user" {
			continue
		}
		ordinal++
		if ordinal == desk {
			return s.SpaceID, nil
		}
	}
	return 0, noSuchDeskError{desk: desk, have: ordinal}
}

// onSpace reports whether a window's world-model space membership includes
// the given space. Membership rides on the snapshot itself (populated at
// enumeration), so window set and space placement are self-consistent —
// no second query to race.
func onSpace(w branchkit.WindowInfo, spaceID int) bool {
	for _, id := range w.SpaceIds {
		if id == spaceID {
			return true
		}
	}
	return false
}

// frameHits returns the eligible extension windows whose bounds match a CG
// window's frame.
func frameHits(c branchkit.WindowInfo, eligible []extWindowInfo) []extWindowInfo {
	var hits []extWindowInfo
	for _, e := range eligible {
		if boundsMatch(c, e) {
			hits = append(hits, e)
		}
	}
	return hits
}

// boundsMatch reports whether a CG window frame and an extension window's
// bounds describe the same window, within tolerance.
func boundsMatch(w branchkit.WindowInfo, e extWindowInfo) bool {
	abs := func(v int) int {
		if v < 0 {
			return -v
		}
		return v
	}
	return abs(w.X-e.Left) <= frameMatchTolerance &&
		abs(w.Y-e.Top) <= frameMatchTolerance &&
		abs(w.W-e.Width) <= frameMatchTolerance &&
		abs(w.H-e.Height) <= frameMatchTolerance
}

// eligibleSurgeryWindows filters the extension's inventory down to valid
// merge targets. Excluded: minimized and fullscreen windows (stashing into an
// invisible window reads as the tab vanishing; fullscreen windows live on
// their own spaces), and the SOURCE window — the one holding the tab being
// moved. The source exclusion is load-bearing, not hygiene: the pop path
// creates the far-desk window at the source frame, so the two share bounds
// forever — and structural exclusion beats hoping a tie-break saves the
// pick (field bug 2026-07-25: tabs.move into the tab's own window reported
// ok while visibly doing nothing).
func eligibleSurgeryWindows(extWins []extWindowInfo) []extWindowInfo {
	eligible := make([]extWindowInfo, 0, len(extWins))
	for _, e := range extWins {
		if e.State == "minimized" || e.State == "fullscreen" || e.IsSource {
			continue
		}
		eligible = append(eligible, e)
	}
	return eligible
}

// pickByFrame correlates candidate CG windows with eligible extension
// windows by frame. A candidate with exactly one frame-hit (or a title
// tie-break winner, when CG titles exist) is a confident match. When every
// hit-bearing candidate is ambiguous — multiple same-frame ext windows and
// no usable title — the first such candidate is returned for the caller to
// resolve with the identity probe; guessing is how a tab silently landed in
// a window on a different desk (field bug 2026-07-25, second occurrence:
// three full-size Chrome windows across three desks shared one frame).
// Pure — unit-tested.
func pickByFrame(candidates []branchkit.WindowInfo, eligible []extWindowInfo) (cg branchkit.WindowInfo, ext extWindowInfo, ok bool, ambiguous *branchkit.WindowInfo) {
	for i := range candidates {
		c := candidates[i]
		hits := frameHits(c, eligible)
		if len(hits) == 1 {
			return c, hits[0], true, nil
		}
		if len(hits) > 1 {
			// Title tie-break: the window title is the active tab's title
			// (plus browser suffix), so containment is the right test.
			// Candidates arrive AX-title-enriched (axEnrichTitles), so this
			// fires without Screen Recording; an AX read failure leaves the
			// title empty and we fall through to the probe.
			for _, h := range hits {
				if h.ActiveTabTitle != "" && strings.Contains(c.Title, h.ActiveTabTitle) {
					return c, h, true, nil
				}
			}
			if ambiguous == nil {
				ambiguous = &candidates[i]
			}
		}
	}
	return branchkit.WindowInfo{}, extWindowInfo{}, false, ambiguous
}

// findNudgedCGWindow picks the CG window sitting at the probe-nudged frame:
// exact nudged width (the un-nudged twins differ by exactly 1px, so the
// usual tolerance would defeat the probe), tolerant on the other edges.
// Pure — unit-tested.
func findNudgedCGWindow(e extWindowInfo, nudgedW int, windows []branchkit.WindowInfo) (branchkit.WindowInfo, bool) {
	abs := func(v int) int {
		if v < 0 {
			return -v
		}
		return v
	}
	var hits []branchkit.WindowInfo
	for _, w := range windows {
		if w.W == nudgedW &&
			abs(w.X-e.Left) <= frameMatchTolerance &&
			abs(w.Y-e.Top) <= frameMatchTolerance &&
			abs(w.H-e.Height) <= frameMatchTolerance {
			hits = append(hits, w)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
	}
	return branchkit.WindowInfo{}, false
}

// resolveExtToCG pins the CG identity of one extension window via the
// REVERSE identity probe: the extension shrinks its own window's width by
// 1px (browser window APIs work regardless of which macOS space the window
// is on — unlike AX, which cannot reach non-visible spaces at all,
// field-verified 2026-07-26), the CG world model observes the nudge (CG
// frames are readable on every space with no extra permission), and the
// width-exact CG window IS that ext window. The frame is restored either
// way. Last resort after both passive channels (frames, AX titles) fail to
// discriminate — this makes the frame briefly unique instead; invisible in
// practice because the ambiguous window is off-screen by definition.
func (h *Host) resolveExtToCG(e extWindowInfo, corr string) (string, error) {
	if e.Width <= frameMatchTolerance+2 {
		return "", fmt.Errorf("ext window %d too small to probe (w=%d)", e.WindowID, e.Width)
	}
	nudgedW := e.Width - 1
	setWidth := func(w int) error {
		params := func(id string) any {
			return wire.ResizeWindowParams{RequestID: id, WindowID: e.WindowID, Width: w}
		}
		_, err := h.sendSurgeryAndWait("resize_window", params, corr, surgeryQueryTimeout)
		return err
	}
	if err := setWidth(nudgedW); err != nil {
		return "", fmt.Errorf("reverse probe nudge: %w", err)
	}
	defer func() {
		if err := setWidth(e.Width); err != nil {
			branchkit.Logf(h.state.PluginID, "reverse probe: restore ext window %d: %v", e.WindowID, err)
		}
	}()
	// CGWindowList observes the resize asynchronously — poll briefly instead
	// of trusting one snapshot.
	for attempt := 0; attempt < 3; attempt++ {
		time.Sleep(100 * time.Millisecond)
		wm, err := h.worldModel()
		if err != nil {
			continue
		}
		if w, found := findNudgedCGWindow(e, nudgedW, wm.Windows); found {
			return w.ID, nil
		}
	}
	return "", fmt.Errorf("reverse probe: no unique CG window at nudged width %d for ext %d", nudgedW, e.WindowID)
}

// axEnrichTitles fills empty candidate titles with per-window AX reads
// (native.get_window_info). World-model titles come from CGWindowList and
// need Screen Recording permission (empty on this machine class); AX titles
// need only Accessibility, which the platform always holds. Reach is
// limited (field-verified 2026-07-26): AX exposes only windows on the
// CURRENTLY VISIBLE space of each display — so this arms the title
// tie-break when the target desk happens to be showing on another display,
// and reads nothing for genuinely off-screen candidates (those still need
// the probe or the pop fallback). Best-effort: a failed read leaves the
// title empty.
func (h *Host) axEnrichTitles(candidates []branchkit.WindowInfo) {
	for i := range candidates {
		if candidates[i].Title != "" {
			continue
		}
		info, err := h.plugin.NativeGetWindowInfo(branchkit.NativeGetWindowInfoRequest{WindowID: candidates[i].ID})
		if err != nil {
			continue
		}
		if info.Title != nil {
			candidates[i].Title = *info.Title
		}
	}
}

// correlateExtWindow is the full correlation: passive frame match first
// (with AX-sourced titles arming the tie-break), the reverse identity probe
// only when frames AND titles are both ambiguous. Returns ok=false when the
// candidate set has no confident extension counterpart.
func (h *Host) correlateExtWindow(candidates []branchkit.WindowInfo, extWins []extWindowInfo, corr string) (branchkit.WindowInfo, extWindowInfo, bool) {
	h.axEnrichTitles(candidates)
	eligible := eligibleSurgeryWindows(extWins)
	cg, ext, ok, ambiguous := pickByFrame(candidates, eligible)
	if ok {
		return cg, ext, true
	}
	if ambiguous == nil {
		return branchkit.WindowInfo{}, extWindowInfo{}, false
	}
	// Reverse probe: pin each same-frame ext twin to its CG identity until
	// one lands on a candidate. Bounded by the twin count (2-3 in practice);
	// a twin that resolves to a non-candidate CG window is simply not on the
	// target desk.
	for _, e := range frameHits(*ambiguous, eligible) {
		cgID, err := h.resolveExtToCG(e, corr)
		if err != nil {
			branchkit.Logf(h.state.PluginID, "correlate: %v", err)
			continue
		}
		for i := range candidates {
			if candidates[i].ID == cgID {
				return candidates[i], e, true
			}
		}
	}
	return branchkit.WindowInfo{}, extWindowInfo{}, false
}

// dispatchPlatformAction routes another plugin's action through the platform
// dispatch op — the sanctioned cross-plugin path (op-auth `dispatch`
// privilege + interaction gate + this plugin's declared dispatch_prefixes:
// windows.* for tab surgery, input.* for native browser-chrome shortcuts).
//
// Wire shape: the dispatch RPC takes the platform's tagged Action enum,
// where `Type` is the VARIANT ("plugin" | "sequence") and the dotted action
// name rides in `ActionType` — NOT the flattened commands.json convention
// ({"type": "placement.snap", ...}), which only the matcher's own command
// parser accepts.
//
// `branchkit.Action` is generated from that enum as of 2026-09-19, so the
// distinction is now the type's, not this comment's: there is no field to
// put a dotted name in but `ActionType`. This used to build an untyped
// map[string]any, which is how the 2026-07-25 field bug was possible.
func buildDispatchAction(actionType string, params map[string]any) (branchkit.Action, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return branchkit.Action{}, fmt.Errorf("marshal params for %s: %w", actionType, err)
	}
	return branchkit.Action{
		Type:       branchkit.ActionKindPlugin,
		ActionType: &actionType,
		Params:     encoded,
	}, nil
}

// Correlation is NOT passed explicitly. It rides the RPC envelope, stamped
// by the SDK from the ambient scope (rpc.go, `CorrelationID:
// currentCorrelation()`), so the chain is intact without anyone threading it
// through a parameter.
//
// It used to be threaded through, into `DispatchRequest.TraceID` — a field
// that held a 32-hex single-use routing credential, not a `tr_` correlation
// id. The two formats never matched, so every dispatch here presented a trace
// that could not validate and earned a TRACE_REJECTED warning accusing this
// plugin of claiming command routing it was not granted, before falling
// through to the interaction check that actually decided it. The field is
// gone now along with the credential it carried.
//
// Every platform effect this plugin causes passes through here — the
// keystrokes (keystrokes.go) and the tab-desk window moves — so this is where
// the dev focus pin holds them back: a pinned browser is not the app in front,
// and a keystroke or window move would land in the person's own app. Recorded
// and reported as done, so the flow behaves as for a real focused browser
// (focus_pin.go).
func (h *Host) dispatchPlatformAction(actionType string, params map[string]any) error {
	if h.pin.suppress(pinEffectDispatch, actionType, params) {
		branchkit.Logf(h.state.PluginID, "[FOCUS-PIN] suppressed platform dispatch %s %v", actionType, params)
		return nil
	}
	return h.dispatchPlatformRPC(actionType, params)
}

// dispatchPlatformRPCDefault sends one action over the dispatch op. Seam:
// tests record instead, and never reach the platform.
func (h *Host) dispatchPlatformRPCDefault(actionType string, params map[string]any) error {
	action, err := buildDispatchAction(actionType, params)
	if err != nil {
		return err
	}
	resp, err := h.plugin.Dispatch(branchkit.DispatchRequest{Action: action})
	if err != nil {
		return fmt.Errorf("dispatch %s: %w", actionType, err)
	}
	if resp.Status != "ok" {
		msg := ""
		if resp.Message != nil {
			msg = *resp.Message
		}
		return fmt.Errorf("dispatch %s: status %s %s", actionType, resp.Status, msg)
	}
	return nil
}

// focusedBrowserSnapshot captures the focus state a surgery flow needs.
// Returns ok=false when no connected browser is focused (the app-active tag
// should prevent the commands from matching then; this is the belt).
func (h *Host) focusedBrowserSnapshot() (connID, bundle string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	connID = h.state.FocusedConnID
	bundle = h.state.FocusedBundleID
	// Pinned (focus_pin.go), the app in front is not the browser: name the
	// pinned one, whose windows the flow looks for.
	if bundle == "" || h.pin.current() != "" {
		bundle = h.state.FocusedBrowserBundleID
	}
	return connID, bundle, connID != "" && bundle != ""
}

// Telling the person a tab move did not happen. The flows below run off the
// on_action reply (it cannot wait on them), so a failure used to reach only
// the log: "stash tab desk 3" that did nothing sounded exactly like a mishear.
// Each exit now also goes through the problem path (problem.go), shown as
// well as spoken, since nothing else on screen says it. The log line keeps
// the technical cause; the problem says it in the person's words.

const tabNotMoved = "Tab not moved"

// tabMoveProblem is a problem the window shows and speech reads.
func tabMoveProblem(reason, title, phrase string) actionProblem {
	return actionProblem{Reason: reason, Title: title, Phrase: phrase, Show: true}
}

// tabMoveFailed is the common case: the browser or the platform did not
// finish the move, and the tab is where it was.
func tabMoveFailed() actionProblem {
	return tabMoveProblem("tab_move_failed", tabNotMoved, "The browser didn't move the tab. Try again.")
}

// noBrowserForTabMove is the flows' "no connected browser in front", worded
// as for any browser command that reached no browser.
func noBrowserForTabMove() actionProblem {
	p := problemForUndelivered(dispatchNotFocused, "")
	p.Title = tabNotMoved
	return p
}

// runTabToDesk executes "stash/blank tab desk N". Runs in a goroutine off
// handleOnAction: the pop path spans two extension round trips plus the
// placement plugin's drag move, and the on_action reply must not wait on that
// (5s RPC timeout, and no caller consumes the result).
func (h *Host) runTabToDesk(desk int, follow bool, corr string) {
	const action = "tab_to_desk"
	connID, bundle, ok := h.focusedBrowserSnapshot()
	if !ok {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: no focused browser connection")
		h.reportActionProblem(action, noBrowserForTabMove())
		return
	}
	spaceID, err := h.resolveDeskSpace(desk)
	if err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: %v", err)
		var missing noSuchDeskError
		if errors.As(err, &missing) {
			h.reportActionProblem(action, tabMoveProblem("no_such_desk", "No such desk",
				fmt.Sprintf("There's no desk %d. You have %d.", missing.desk, missing.have)))
		} else {
			h.reportActionProblem(action, tabMoveProblem("tab_move_failed", tabNotMoved,
				"BranchKit couldn't read your desktops, so the tab stayed here."))
		}
		return
	}

	wm, err := h.worldModel()
	if err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: world_model: %v", err)
		h.reportActionProblem(action, tabMoveProblem("tab_move_failed", tabNotMoved,
			"BranchKit couldn't read your windows, so the tab stayed here."))
		return
	}

	// Already there? The focused window sitting on the target space means
	// "move to where it already is" — nothing useful to do, but say so: a
	// command that does nothing silently sounds like a mishear.
	var candidates []branchkit.WindowInfo
	for _, w := range wm.Windows {
		if !onSpace(w, spaceID) {
			continue
		}
		if wm.ActiveWindowID != nil && w.ID == *wm.ActiveWindowID {
			branchkit.Logf(h.state.PluginID, "tab_to_desk: active window already on desk %d", desk)
			h.reportActionProblem(action, tabMoveProblem("already_there", "Already there",
				fmt.Sprintf("This window is already on desk %d.", desk)))
			return
		}
		if w.AppID == bundle {
			candidates = append(candidates, w)
		}
	}

	// Merge path: a same-browser window already lives on the target desk.
	// Pure tab move — silent, no space switching (except follow's final
	// desk switch). Correlation failure (other profile, incognito) falls
	// through to the pop path.
	if len(candidates) > 0 {
		if reply, err := h.sendSurgeryAndWait("query_windows", queryWindowsParams, corr, surgeryQueryTimeout); err == nil {
			if _, ext, found := h.correlateExtWindow(candidates, reply.Windows, corr); found {
				params := func(id string) any {
					return wire.MoveActiveTabToWindowParams{RequestID: id, WindowID: ext.WindowID, Activate: follow}
				}
				if _, err := h.sendSurgeryAndWait("move_active_tab_to_window", params, corr, surgeryMoveTimeout); err != nil {
					branchkit.Logf(h.state.PluginID, "tab_to_desk: merge move failed: %v", err)
					h.reportActionProblem(action, tabMoveFailed())
					return
				}
				branchkit.Logf(h.state.PluginID, "tab_to_desk: merged active tab into ext window %d on desk %d (conn=%s follow=%v)",
					ext.WindowID, desk, connID, follow)
				if follow {
					// The extension focused the target window, but macOS only
					// follows app activation across spaces when
					// AppleSpacesSwitchOnActivate is on (off on this machine's
					// config class). Explicit switch; a no-op when focus-follow
					// already landed us there.
					if err := h.dispatchPlatform("placement.desk_switch", map[string]any{"space": strconv.Itoa(desk)}); err != nil {
						branchkit.Logf(h.state.PluginID, "tab_to_desk: desk_switch: %v", err)
						h.reportActionProblem(action, tabMoveProblem("desk_switch_failed", "Tab moved",
							fmt.Sprintf("The tab is on desk %d. BranchKit couldn't take you there.", desk)))
					}
				}
				return
			}
			branchkit.Logf(h.state.PluginID, "tab_to_desk: %d CG candidate(s) on desk %d but none correlated — popping instead", len(candidates), desk)
		} else {
			branchkit.Logf(h.state.PluginID, "tab_to_desk: query_windows: %v — popping instead", err)
		}
	}

	// Pop path: no mergeable window. Pop the active tab into a new window at
	// the source window's frame (visually calm, and makes the CG correlation
	// trivial), then send that window to the target desk via the windows
	// plugin's drag move. stash = round trip back; blank = stay there.
	prevIDs := make(map[string]bool, len(wm.Windows))
	var srcWin *branchkit.WindowInfo
	for i, w := range wm.Windows {
		if w.AppID == bundle {
			prevIDs[w.ID] = true
			if wm.ActiveWindowID != nil && w.ID == *wm.ActiveWindowID {
				srcWin = &wm.Windows[i]
			}
		}
	}
	popParams := func(id string) any {
		p := wire.PopActiveTabParams{RequestID: id}
		if srcWin != nil {
			x, y, w, ht := srcWin.X, srcWin.Y, srcWin.W, srcWin.H
			p.Left, p.Top, p.Width, p.Height = &x, &y, &w, &ht
		}
		return p
	}
	reply, err := h.sendSurgeryAndWait("pop_active_tab", popParams, corr, surgeryPopTimeout)
	if err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: pop: %v", err)
		h.reportActionProblem(action, tabMoveFailed())
		return
	}
	// From here the tab is already in a new window on this desk: say where
	// it is, not just that the move failed.
	strandedHere := tabMoveProblem("tab_move_failed", tabNotMoved,
		fmt.Sprintf("The tab is in a new window here. BranchKit couldn't send it to desk %d.", desk))
	cgID := h.awaitNewCGWindow(bundle, prevIDs, reply.Window)
	if cgID == "" {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: popped window never appeared in the world model — tab left in new window on this desk")
		h.reportActionProblem(action, strandedHere)
		return
	}
	if err := h.dispatchPlatform("placement.move_to_space", map[string]any{
		"space":     strconv.Itoa(desk),
		"stay":      !follow,
		"window_id": cgID,
	}); err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_desk: move_to_space: %v", err)
		h.reportActionProblem(action, strandedHere)
		return
	}
	branchkit.Logf(h.state.PluginID, "tab_to_desk: popped active tab to new window %s and sent to desk %d (conn=%s follow=%v)",
		cgID, desk, connID, follow)
}

// awaitNewCGWindow polls the world model for a CG window of the bundle that
// wasn't there before the pop, preferring one whose frame matches the
// extension's reply. windows.create has landed extension-side by the time
// this runs, so the poll only absorbs CGWindowList's own lag.
func (h *Host) awaitNewCGWindow(bundle string, prevIDs map[string]bool, ext *extWindowInfo) string {
	deadline := time.Now().Add(cgAppearPollBudget)
	fallback := ""
	for {
		wm, err := h.worldModel()
		if err == nil {
			for _, w := range wm.Windows {
				if w.AppID != bundle || prevIDs[w.ID] {
					continue
				}
				if ext == nil || boundsMatch(w, *ext) {
					return w.ID
				}
				fallback = w.ID
			}
		}
		if time.Now().After(deadline) {
			// A new window that never frame-matched (browser clamped the
			// requested bounds) still beats nothing.
			return fallback
		}
		time.Sleep(cgAppearPollInterval)
	}
}

// runTabToWindow executes "stash tab window N": move the active tab into the
// Nth window of the focused browser (world-model order, same convention the
// old AppleScript implementation used), staying put. Cross-desk by nature —
// the target may live anywhere.
func (h *Host) runTabToWindow(index int, corr string) {
	const action = "tab_to_window"
	connID, bundle, ok := h.focusedBrowserSnapshot()
	if !ok {
		branchkit.Logf(h.state.PluginID, "tab_to_window: no focused browser connection")
		h.reportActionProblem(action, noBrowserForTabMove())
		return
	}
	wm, err := h.worldModel()
	if err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_window: world_model: %v", err)
		h.reportActionProblem(action, tabMoveProblem("tab_move_failed", tabNotMoved,
			"BranchKit couldn't read your windows, so the tab stayed here."))
		return
	}
	var browserWindows []branchkit.WindowInfo
	for _, w := range wm.Windows {
		if w.AppID == bundle {
			browserWindows = append(browserWindows, w)
		}
	}
	if index < 1 || index > len(browserWindows) {
		branchkit.Logf(h.state.PluginID, "tab_to_window: index %d out of range (have %d)", index, len(browserWindows))
		h.reportActionProblem(action, tabMoveProblem("no_such_window", "No such window",
			fmt.Sprintf("There's no window %d. The browser has %d.", index, len(browserWindows))))
		return
	}
	target := browserWindows[index-1]
	if wm.ActiveWindowID != nil && target.ID == *wm.ActiveWindowID {
		branchkit.Logf(h.state.PluginID, "tab_to_window: window %d is the active window", index)
		h.reportActionProblem(action, tabMoveProblem("already_there", "Already there",
			fmt.Sprintf("The tab is already in window %d.", index)))
		return
	}
	reply, err := h.sendSurgeryAndWait("query_windows", queryWindowsParams, corr, surgeryQueryTimeout)
	if err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_window: %v", err)
		h.reportActionProblem(action, tabMoveFailed())
		return
	}
	_, ext, found := h.correlateExtWindow([]branchkit.WindowInfo{target}, reply.Windows, corr)
	if !found {
		branchkit.Logf(h.state.PluginID, "tab_to_window: window %d did not correlate with an extension window", index)
		h.reportActionProblem(action, tabMoveProblem("tab_move_failed", tabNotMoved,
			fmt.Sprintf("BranchKit couldn't find window %d in the browser, so the tab stayed here.", index)))
		return
	}
	params := func(id string) any {
		return wire.MoveActiveTabToWindowParams{RequestID: id, WindowID: ext.WindowID}
	}
	if _, err := h.sendSurgeryAndWait("move_active_tab_to_window", params, corr, surgeryMoveTimeout); err != nil {
		branchkit.Logf(h.state.PluginID, "tab_to_window: %v", err)
		h.reportActionProblem(action, tabMoveFailed())
		return
	}
	branchkit.Logf(h.state.PluginID, "tab_to_window: moved active tab into ext window %d (conn=%s)", ext.WindowID, connID)
}

// buildTabDeskCommands is the plugin-owned voice surface for tab surgery,
// gated on the app-active tag like every default-context browser command.
// Verb semantics mirror the placement plugin's window-level family:
// stash = move but stay, blank = move and follow.
func buildTabDeskCommands(pluginID, appTag string) []branchkit.CommandSpec {
	return []branchkit.CommandSpec{
		branchkit.Command(branchkit.Word("stash"), branchkit.Word("tab"), branchkit.Word("desk"), branchkit.Capture("", "number")).
			Action(pluginID+".tab_to_desk", map[string]any{"desk": "{number}", "follow": false}).
			Category("Tabs").
			RequiresTags(appTag).
			Description("Move the current tab to the numbered desktop but stay here.").
			Build(),
		branchkit.Command(branchkit.Word("blank"), branchkit.Word("tab"), branchkit.Word("desk"), branchkit.Capture("", "number")).
			Action(pluginID+".tab_to_desk", map[string]any{"desk": "{number}", "follow": true}).
			Category("Tabs").
			RequiresTags(appTag).
			Description("Move the current tab to the numbered desktop and follow it there.").
			Build(),
		branchkit.Command(branchkit.Word("stash"), branchkit.Word("tab"), branchkit.Word("window"), branchkit.Capture("", "number")).
			Action(pluginID+".tab_to_window", map[string]any{"index": "{number}"}).
			Category("Tabs").
			RequiresTags(appTag).
			Description("Move the current tab into the numbered window without leaving.").
			Build(),
	}
}

// interceptTabSurgeryAction claims the two orchestrated actions off the
// generic on_action → SSE forward. Returns true when handled. The heavy
// lifting runs in a goroutine — see runTabToDesk.
func (h *Host) interceptTabSurgeryAction(subAction string, rawParams map[string]any) bool {
	switch subAction {
	case "tab_to_desk":
		desk, err := strconv.Atoi(paramString(rawParams, "desk"))
		if err != nil || desk < 1 || desk > 16 {
			branchkit.Logf(h.state.PluginID, "tab_to_desk: invalid desk %q", paramString(rawParams, "desk"))
			h.reportActionProblem(subAction, tabMoveProblem("no_such_desk", "No such desk",
				fmt.Sprintf("There's no desk %s.", paramString(rawParams, "desk"))))
			return true
		}
		follow := paramBool(rawParams, "follow")
		corr := h.plugin.CurrentCorrelation()
		go h.runTabToDesk(desk, follow, corr)
		return true
	case "tab_to_window":
		index, err := strconv.Atoi(paramString(rawParams, "index"))
		if err != nil || index < 1 {
			branchkit.Logf(h.state.PluginID, "tab_to_window: invalid index %q", paramString(rawParams, "index"))
			h.reportActionProblem(subAction, tabMoveProblem("no_such_window", "No such window",
				fmt.Sprintf("There's no window %s.", paramString(rawParams, "index"))))
			return true
		}
		corr := h.plugin.CurrentCorrelation()
		go h.runTabToWindow(index, corr)
		return true
	}
	return false
}

func paramString(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func paramBool(params map[string]any, key string) bool {
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}
