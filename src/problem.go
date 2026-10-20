package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Telling the person when a spoken browser command could not run.
//
// A command that reached no browser used to answer "ok" and do nothing:
// silence, which the person cannot tell from a misrecognition. Now three
// things happen:
//
//   - the on_action reply is status "error" with the reason in `result`, so
//     the platform and any dispatch caller see the failure;
//   - a browser.action_failed event carries it on the bus;
//   - the reason becomes a `problem` document on this plugin's own HUD
//     channel (output.state), the semantic output channel every renderer
//     reads: the platform draws it in the channel's window, and the
//     announcements plugin speaks it to a person who has asked for speech.
//
// The channel is created at runtime (hud.create_channel) rather than declared
// in the manifest, and placed like the platform's notification toast.

// problemChannel is this plugin's HUD channel for problems.
const problemChannel = "browser_problem"

// problemShowFor is how long a shown problem stays up before the window
// hides and its state clears.
const problemShowFor = 5 * time.Second

// actionProblem is one thing the person needs to know about a command that
// did not run, in their words.
type actionProblem struct {
	Reason string // machine code (a dispatch* constant, or "no_such_hint")
	Title  string // the problem, in a few words
	Phrase string // the problem and the way out, in a sentence
	// Show the channel's window. False when another surface already shows
	// it (the extension's own "no such hint" toast): the document still
	// reaches the renderers that are not windows.
	Show bool
}

// problemForUndelivered words a dispatch* reason for the person. `bundle` is
// the app the OS has in front, used to name the browser.
func problemForUndelivered(reason, bundle string) actionProblem {
	browser := "the browser"
	if bundle != "" {
		browser = browserLabel(bundle)
	}
	switch reason {
	case dispatchNoBrowser:
		return actionProblem{Reason: reason, Show: true,
			Title:  "Browser not connected",
			Phrase: fmt.Sprintf("%s isn't connected to BranchKit. Reload the BranchKit extension, then try again.", browser)}
	case dispatchNotFocused:
		return actionProblem{Reason: reason, Show: true,
			Title:  "No browser in front",
			Phrase: "That's a browser command, and no connected browser is in front. Switch to the browser, then try again."}
	case dispatchAmbiguous:
		return actionProblem{Reason: reason, Show: true,
			Title:  "Which browser?",
			Phrase: "More than one browser is connected and BranchKit can't tell which one is in front. Click in the browser window, then try again."}
	case dispatchBusy:
		return actionProblem{Reason: reason, Show: true,
			Title:  "Browser not responding",
			Phrase: fmt.Sprintf("The BranchKit extension in %s isn't keeping up. Try again; if it keeps happening, reload the extension.", browser)}
	default:
		return actionProblem{Reason: reason, Show: true,
			Title:  "Command didn't run",
			Phrase: "The browser didn't receive that command."}
	}
}

// extensionRefusal is a command the extension received and refused, read off
// its /dispatch-result (bridge.go handleDispatchResultPost).
//
// rearm says the refusal must roll back the hints tag: the action was a hint
// activation, whose manifest ClearsTags dropped plugin.browser.hints at MATCH
// time, and it was refused before the extension's badge handoff, so the badges
// are still up and a hint is still what the person will say next. A refusal
// that happens after the handoff is not a non-event: needs_user_gesture
// FOCUSED its element, and in manual mode the badges were already hidden, so
// re-arming there would put the exclusive tag up over no badges. The
// reference refusals never cleared the tag (resolve_reference has no
// ClearsTags), so there is nothing to roll back.
type extensionRefusal struct {
	problem actionProblem
	rearm   bool
}

// detailRestrictedPage is the extension's refusal of a command that needs a
// page the tab doesn't have: a new-tab page, the browser's own pages, the
// extension stores, another extension's page — anywhere the browser runs no
// content script (the extension's background/restricted-page.ts). An older
// extension never sends it; this plugin reads it when it comes.
const detailRestrictedPage = "restricted_page"

// paletteOpenAction is the extension's action id for opening the whole
// palette: the way off a restricted page by voice, which the problem names.
const paletteOpenAction = "toggle_palette"

// paletteOpenFallback is that command's spoken form when no connection has
// contributed one — the extension's catalog default.
const paletteOpenFallback = "palette all"

// refusalFor words the extension's refusal of a command, or ok=false when the
// result is not one. All but one leave the window alone: the extension toasts
// every refusal but no_such_hint in the page, and deliberately stays quiet on
// that one, where a toast per mishear would be noise. The document still
// reaches the renderers that are not windows — speech, for a person who asked
// for it. restricted_page is the exception: there is no page to toast in, so
// the window is the only thing the person can see.
//
// The detail codes are the extension's: no_such_hint (activate/sealed-gate.ts,
// the SW's unroutable pull), reference_not_found and reference_not_visible
// (activate/voice-dispatch.ts), needs_user_gesture (activate/text-entry.ts),
// restricted_page (background/restricted-page.ts), and the codeword-holder
// refusals (activate/holder-report.ts): match_off_screen (a search match past
// the fold), chip_off_screen (a pick chip past it), pick_pending (a stray
// codeword a pending pick swallowed), extra_hint_off_screen.
//
// no_such_hint may carry `unseen`: the hint exists but the sealed gate
// refused it because the person cannot see it ('off_screen' or
// 'not_visible', the gate's whyUnseen), or because the page has turned the
// control off ('disabled': disabled, in a disabled fieldset, aria-disabled,
// or inside an inert region — a mouse couldn't use it either). Telling that person "nothing has the
// hint" sends them hunting for a typo when the fix is to scroll.
//
// An extension that predates the holder codes sent free text there, so those
// are still keyed on their resolution below — remove once no such extension
// can connect.
func (h *Host) refusalFor(r DispatchResultRequest) (extensionRefusal, bool) {
	switch r.Detail {
	case detailRestrictedPage:
		// No rearm: nothing was matched against badges on a page that has
		// none, and a hint mode re-armed over a new-tab page would suppress
		// the very commands that get the person off it. The way off is the
		// palette, named by the phrase the extension contributed for it, so
		// a renamed command is never taught by its old name.
		return extensionRefusal{problem: actionProblem{Reason: r.Detail, Show: true,
			Title: "BranchKit can't use this page",
			Phrase: fmt.Sprintf("This page doesn't allow BranchKit. Say “%s” to open a site or search.",
				h.contributedPhrase(paletteOpenAction, paletteOpenFallback))}}, true
	case "no_such_hint":
		switch r.Unseen {
		case unseenOffScreen:
			return extensionRefusal{rearm: true, problem: actionProblem{Reason: "hint_off_screen",
				Title:  "Hint off screen",
				Phrase: "That hint is off screen. Scroll to it first."}}, true
		case unseenNotVisible:
			return extensionRefusal{rearm: true, problem: actionProblem{Reason: "hint_not_visible",
				Title:  "Hint covered",
				Phrase: "That hint is covered. Close what's over it, or pick another."}}, true
		case unseenDisabled:
			return extensionRefusal{rearm: true, problem: actionProblem{Reason: "hint_disabled",
				Title:  "Control turned off",
				Phrase: "The page has turned that control off right now."}}, true
		}
		return extensionRefusal{rearm: true, problem: actionProblem{Reason: r.Detail,
			Title:  "No such hint",
			Phrase: fmt.Sprintf("Nothing on the page has the hint %s.", r.Codeword)}}, true
	case "reference_not_found":
		phrase := "Say the name of a saved reference."
		if r.Codeword != "" {
			phrase = fmt.Sprintf("There's no reference called %s on this page.", r.Codeword)
		}
		return extensionRefusal{problem: actionProblem{Reason: r.Detail, Title: "No such reference", Phrase: phrase}}, true
	case "reference_not_visible":
		return extensionRefusal{problem: actionProblem{Reason: r.Detail,
			Title:  "Reference not visible",
			Phrase: fmt.Sprintf("%s isn't visible. Scroll to it first.", r.Codeword)}}, true
	case "needs_user_gesture":
		return extensionRefusal{problem: actionProblem{Reason: r.Detail,
			Title:  "Press Enter",
			Phrase: "The browser opens that only for a real key press. It has focus: press Enter to open it."}}, true
	case detailMatchOffScreen, detailChipOffScreen:
		return matchOffScreen, true
	case detailPickPending:
		return pickPending, true
	case detailExtraHintOffScreen:
		return extensionRefusal{rearm: true, problem: actionProblem{Reason: r.Detail,
			Title:  "Off screen",
			Phrase: "That badge is out of sight. Scroll to it first."}}, true
	}
	if r.OK {
		return extensionRefusal{}, false
	}
	// An extension older than the holder codes: free text, keyed on the
	// resolution.
	switch r.Resolution {
	case "search_badge":
		return matchOffScreen, true
	case "range_pick":
		// The old content.ts wrote 'pick pending — codeword is not a chip'
		// when a pick swallowed a stray codeword; otherwise the chip was off
		// screen.
		if strings.HasPrefix(r.Detail, "pick pending") {
			return pickPending, true
		}
		return matchOffScreen, true
	}
	return extensionRefusal{}, false
}

// The codeword-holder refusal codes (the extension's activate/holder-report.ts).
const (
	detailMatchOffScreen     = "match_off_screen"
	detailChipOffScreen      = "chip_off_screen"
	detailPickPending        = "pick_pending"
	detailExtraHintOffScreen = "extra_hint_off_screen"
)

// The sealed gate's reasons for an unseen hint (DispatchResultRequest.Unseen).
const (
	unseenOffScreen  = "off_screen"
	unseenNotVisible = "not_visible"
	unseenDisabled   = "disabled"
)

// The two holder refusals an old and a new extension both report, worded once.
var (
	matchOffScreen = extensionRefusal{rearm: true, problem: actionProblem{Reason: "match_off_screen",
		Title:  "Off screen",
		Phrase: "That match is off screen. Scroll to it first."}}
	pickPending = extensionRefusal{rearm: true, problem: actionProblem{Reason: "pick_pending",
		Title:  "Pick a match",
		Phrase: "Pick one of the highlighted matches, or say escape."}}
)

// actionFailed builds the on_action reply for a command that did not run:
// status "error", the reason and the words in `result`.
func actionFailed(p actionProblem) branchkit.OnActionResponse {
	raw, _ := json.Marshal(map[string]string{"reason": p.Reason, "message": p.Phrase})
	return branchkit.OnActionResponse{Status: branchkit.OnActionStatusError, Result: raw}
}

// problemSurface holds the channel's lifecycle: created once per process, and
// one pending hide timer (a newer problem restarts it).
type problemSurface struct {
	mu        sync.Mutex
	created   bool
	hideTimer *time.Timer
	gen       uint64 // bumped per shown problem; a hide for an older one is void
}

// reportProblemDefault states the problem on the plugin's HUD channel and, when
// asked, shows the window for problemShowFor. Seam: tests record instead.
// Best-effort: a problem that cannot be shown is logged, never fatal.
func (h *Host) reportProblemDefault(p actionProblem) {
	pluginID := h.state.PluginID
	h.problems.mu.Lock()
	defer h.problems.mu.Unlock()
	if !h.problems.created {
		anchor := branchkit.AnchorTopRight
		desc := "Browser command problems"
		width, minHeight, stack := 360, 60, 10
		// An error here is usually "already exists" — the channel outlives a
		// plugin restart — and the push below is the real test of ownership.
		if err := h.plugin.HUDCreateChannel(branchkit.HUDCreateChannelRequest{Channel: problemChannel, Anchor: &anchor, Description: &desc, MinHeight: &minHeight, StackOrder: &stack, Width: &width}); err != nil {
			branchkit.Logf(pluginID, "problem channel create: %v (continuing)", err)
		}
		h.problems.created = true
	}
	doc := branchkit.OutputState{
		Channel: problemChannel,
		Kind:    branchkit.OutputKindProblem,
		Title:   p.Title,
		Phrase:  p.Phrase,
		Urgency: branchkit.OutputUrgencyInterrupt,
		Locale:  "en",
		V:       1,
	}
	if err := h.publishProblemDoc(doc); err != nil {
		branchkit.Logf(pluginID, "problem output.state failed reason=%s: %v", p.Reason, err)
		return
	}
	if !p.Show {
		return
	}
	// Under the dev focus pin the person is in another app: a window popping
	// up over it for a test browser's problem is exactly the interruption the
	// pin exists to avoid. The document is still published (renderers that
	// are not windows, and a check reading it, see it); the show is recorded.
	if h.pin.suppress(pinEffectProblemShow, p.Reason, map[string]any{"title": p.Title, "phrase": p.Phrase}) {
		branchkit.Logf(pluginID, "[FOCUS-PIN] suppressed problem window reason=%s", p.Reason)
		return
	}
	if err := h.showProblemWindow(); err != nil {
		branchkit.Logf(pluginID, "problem show failed: %v", err)
		return
	}
	if h.problems.hideTimer != nil {
		h.problems.hideTimer.Stop()
	}
	h.problems.gen++
	gen := h.problems.gen
	h.problems.hideTimer = time.AfterFunc(problemShowFor, func() {
		// Under the surface lock, RPCs included: a newer problem arriving
		// mid-hide must wait for the hide, not be hidden by it.
		h.problems.mu.Lock()
		defer h.problems.mu.Unlock()
		if h.problems.gen != gen {
			return // a newer problem owns the window now
		}
		h.problems.hideTimer = nil
		if _, err := h.plugin.OutputClear(branchkit.OutputClearRequest{Channel: problemChannel}); err != nil {
			branchkit.Logf(pluginID, "problem clear failed: %v", err)
		}
		if err := h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: problemChannel}); err != nil {
			branchkit.Logf(pluginID, "problem hide failed: %v", err)
		}
	})
}

// publishProblemDocDefault publishes a problem document. Seam.
func (h *Host) publishProblemDocDefault(doc branchkit.OutputState) error {
	_, err := h.plugin.OutputState(branchkit.OutputStateRequest{State: doc})
	return err
}

// showProblemWindowDefault shows the problem channel's window. Seam.
func (h *Host) showProblemWindowDefault() error {
	return h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: problemChannel})
}

// reportActionProblem is the whole report for a command that did not run: the
// log line, the bus event, and the person-facing problem. Off the caller's
// goroutine for the RPCs, so an on_action reply is never held up by them.
func (h *Host) reportActionProblem(action string, p actionProblem) {
	branchkit.Logf(h.state.PluginID, "[PROBLEM] action=%s reason=%s: %s", action, p.Reason, p.Title)
	h.emitEvent("browser.action_failed", map[string]any{
		"action":  action,
		"reason":  p.Reason,
		"message": p.Phrase,
	})
	h.runAsync(func() { h.reportProblem(p) })
}
