package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// vocabularyCommitDebounce is the quiet-window length for coalescing
// scroll-burst vocabulary.commit calls. Long enough to absorb a 3-commits-in-200ms burst; short enough that
// a single scroll feels live. Drop to 25ms if codewords-speakable-too-long
// after their elements left the viewport becomes observable.
const vocabularyCommitDebounce = 50 * time.Millisecond

// hintDropSamples bounds how many dropped codewords one summary line names.
const hintDropSamples = 5

// hintDrops collects the elements one grammar frame filtered out of the
// dynamic-collections push, per reason, so they are logged as ONE line per
// reason per frame rather than one line per element. The badge may still
// render (the extension is the source of truth there), but no voice command
// will resolve — a violation of the badge-visible-implies-commandable
// invariant, which is why it is logged at all.
//
// Summarised because a frame refused wholesale (the extension sending letter
// tokens like "f s" when its spoken overlay is not loaded) logged one line
// per element on every resend: 16,660 lines, 2.1 MB of branchkit.log in a day.
// The count and a sample say the same thing.
type hintDrops struct {
	reasons  []string // first-seen order, for a stable line order
	byReason map[string][]string
}

func (d *hintDrops) add(reason, codeword string) {
	if d.byReason == nil {
		d.byReason = make(map[string][]string)
	}
	if _, seen := d.byReason[reason]; !seen {
		d.reasons = append(d.reasons, reason)
	}
	d.byReason[reason] = append(d.byReason[reason], codeword)
}

// log emits one structured stderr line per reason. Lands in branchkit.log and
// as a plugin.stderr_line event in show-all.jsonl.
func (d *hintDrops) log(pluginID string, tabID, frameID int) {
	for _, reason := range d.reasons {
		cws := d.byReason[reason]
		sample := cws
		if len(sample) > hintDropSamples {
			sample = sample[:hintDropSamples]
		}
		branchkit.Logf(pluginID,
			"[STATE_WRITE_DROPPED] target=hints reason=%s tab=%d frame=%d count=%d sample=%q",
			reason, tabID, frameID, len(cws), sample)
	}
}

// scheduleVocabularyCommit defers plugin.VocabularyCommit() until the quiet
// window elapses. Each call cancels any pending timer and restarts it, so a
// burst of codeword-changing pushes coalesces into a single recognizer
// rebuild. Callers (the per-batch handler via HintVocabularyDirty, the
// tabs-collection sync via its new-words check) gate on the spoken-key set
// having actually changed, so the debounce never wraps no-op work.
func (h *Host) scheduleVocabularyCommit(pluginID string) {
	h.mu.Lock()
	h.scheduleVocabularyCommitLocked(pluginID)
	h.mu.Unlock()
}

// scheduleVocabularyCommitLocked is the body of scheduleVocabularyCommit for
// callers already holding `mu` (the focus-recompute tab sync). The deferred
// commit itself runs on a timer goroutine and takes `mu` on its own.
func (h *Host) scheduleVocabularyCommitLocked(pluginID string) {
	if h.state.VocabularyCommitTimer != nil {
		h.state.VocabularyCommitTimer.Stop()
	}
	h.state.VocabularyCommitTimer = time.AfterFunc(vocabularyCommitDebounce, func() {
		h.mu.Lock()
		h.state.VocabularyCommitTimer = nil
		h.mu.Unlock()
		if err := h.commitVocabulary(); err != nil {
			branchkit.Logf(pluginID, "vocabulary.commit (debounced) failed: %v", err)
		}
	})
}

// clearHintsState transitions the hint gate to idle and clears the
// prefix index. Called when the last SSE client disconnects so a
// subsequent voice command can't match against stale prefixes.
// Per-prefix `browser_hints_<X>` collections are cleaned up by session-end
// (handleHintsSessionEndPost → cleanupFrameSessionLocked) under the
// per-batch protocol, so they aren't deleted here.
func (h *Host) clearHintsState(pluginID string) error {
	h.state.HintGate.transitionToIdle("clear_hints_state")
	prefixListName := hintCollection(pluginID, "prefix")
	// Empty the collection record-by-record — the collection-wide
	// `collection.delete` wire op was removed (verb-surface
	// consolidation); what matters here is that no stale prefix records
	// remain matchable, which delete_records guarantees.
	//
	// ListAll, not List: a partial read here leaves the records it never saw
	// matchable forever, which is the failure this function exists to
	// prevent. A dense page can carry more hints than the platform's default
	// list limit, and List cannot tell a complete read from a capped one.
	recs, err := h.plugin.ListAll(prefixListName)
	if err != nil {
		branchkit.Logf(pluginID, "list %s for clear: %v", prefixListName, err)
		return nil
	}
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	if _, _, err := h.plugin.DeleteMany(prefixListName, ids); err != nil {
		branchkit.Logf(pluginID, "clear %s: %v", prefixListName, err)
	}
	return nil
}

// clearHintsStateOnDisconnect zeroes out the plugin's hint state on
// last-SSE-client disconnect. The next "show hints" via a reconnected
// extension re-pushes everything.
//
// Recheck: between the defer in handleSSE releasing the mutex and us running
// the destructive RPCs, a new SSE client can connect (SW restart cycles).
// If that happens we defer to the new client's setup; our cleanup would
// otherwise race its registration. A residual race remains in the window
// between this recheck and the RPCs starting — rare enough to accept;
// self-heals on the next user-driven scan.
func (h *Host) clearHintsStateOnDisconnect(pluginID string) {
	h.mu.Lock()
	if len(h.state.SSEClients) > 0 {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	if err := h.clearHintsState(pluginID); err != nil {
		branchkit.Logf(pluginID, "disconnect cleanup: clear hints state: %v", err)
	}
	// Nothing to reset about WHICH collections hold records — that is asked
	// of the platform now (livePrefixes), and clearHintsState just made the
	// answer empty. The live-prefix index that used to be reset here is gone
	// along with the last of this plugin's process-local memory of what it
	// had published.
	h.mu.Lock()
	// The sync fingerprints describe what the platform holds; clearHintsState
	// just emptied those collections behind this plugin's back, so every
	// claim is stale. Keeping them would make the next sync skip as a no-op
	// against records that no longer exist.
	h.state.LastSyncFingerprint = make(map[string]uint64)
	h.state.LastSyncKeyFingerprint = make(map[string]uint64)
	bundle := h.state.FocusedBundleID
	h.mu.Unlock()
	// No connection left, so no browser can be in front: the app-active tag
	// must go too, or "over", "toggle", the tab-desk and reference commands
	// keep matching and do nothing. (The focused-disconnect path usually got
	// here first; this covers a last connection that was never bound as
	// focused. The release re-checks under mu, so a reconnect that already
	// re-armed is left alone.)
	h.releaseBrowserFocusUnlocked(pluginID, bundle, "last_disconnect")
}

// sealedHintFields is the action-param map every hint-targeting command
// carries under pull resolution: the sealed browser_alpha captures bind
// {letter, codeword} objects, letters + words ride the params, and the
// EXTENSION resolves letter-pair → element at dispatch time (live strict
// gate + "no such hint" feedback). Shared by the skeleton's activate
// (buildAllHintCommands) and the contributed {hint} verbs (contribute.go).
func sealedHintFields() map[string]any {
	// Params only — the action type is the builder's first argument, and
	// the one-params-dialect builder nests this whole map under "params".
	return map[string]any{
		"prefix_letter": "{prefix.letter}",
		"suffix_letter": "{suffix.letter}",
		"prefix_word":   "{prefix.codeword}",
		"suffix_word":   "{suffix.codeword}",
	}
}

// buildAllHintCommands generates the voice command surface for hint selection.
// The two-word codeword pattern is a SEALED pair over the browser-owned
// alphabet mirror:
//
//	<prefix:browser_alpha> <suffix:browser_alpha>
//
// Any alphabet pair matches while the hints tag is up, and the EXTENSION
// resolves letter-pair → element at dispatch time. One pattern handles every
// possible (prefix, suffix) combination — no per-prefix command generation, no
// scoped pair tag, no per-prefix cancel duplication.
//
// This comment used to describe the DEPENDENT form
// (`<suffix:browser_hints_${prefix}>`), which was deleted 2026-07-19 when the
// sealed path verified live. The stale version was load-bearing in the wrong
// direction: it made the per-prefix `browser_hints_<X>` collections look like
// the MATCH surface when they are only a DisplaySource, which is the single
// easiest wrong turn in this file (see the block on `activate` below, which
// says so at length). Reworded rather than deleted — the pattern shape is worth
// stating, it just has to be the shape that exists. The palette DOES use the
// dependent form (palette.go), so both shapes are live in this plugin; don't
// read one file's answer onto the other.
//
// `_ words` is retained so callers don't need to change their wiring; the
// codewords are no longer needed at command-generation time (the dynamic
// collections in entity_cache carry the live vocabulary).
func buildAllHintCommands(pluginID string, _ [26]string, appTag string) []branchkit.CommandSpec {
	hintsTag := "plugin." + pluginID + ".hints"
	prefixList := hintCollection(pluginID, "prefix")

	// "toggle" gates on the app-active tag; an empty appTag (the
	// no-context case) leaves it ungated.
	var triggerTags []string
	if appTag != "" {
		triggerTags = []string{appTag}
	}

	commands := []branchkit.CommandSpec{
		// Trigger: "toggle" — shows the badges when hidden, hides them when
		// shown. Whether hints are currently up is extension state (badge
		// visibility / hintsShown), so this is a stateless trigger: it
		// dispatches toggle_hints and lets the extension pick the direction —
		// the same handler Ctrl+S uses, so voice and keyboard stay aligned.
		// No SetsTags/ClearsTags because a toggle can't statically choose
		// between them; the extension owns the resulting visibility.
		branchkit.Command(branchkit.Word("toggle")).
			Action(pluginID + ".toggle_hints").
			Description("Toggle hints").
			Category("Badges").
			RequiresTags(triggerTags...).
			Build(),
	}

	// NOTE: the dependent-capture description that used to sit here described
	// the path DELETED on 2026-07-19, and it sat directly above the code that
	// replaced it. It claimed `_strict` was the match surface, so anyone
	// reading top-down concluded that off-screen codewords can't match. They
	// can — see below. Removed rather than reworded; the accurate version is
	// the block on the live command.
	//
	// `${prefix.codeword}` is the field-reference form: prefix entries are
	// object-shaped ({codeword, letter}) so the discovery HUD can render
	// the letter as a subtitle.
	// Activate: a HYBRID pair — SEALED prefix, STRICT suffix, so match == what the
	// Discovery HUD shows. The FIRST letter is the sealed browser-owned alphabet
	// mirror: any first letter always matches and decodes, so you can always START
	// a hint (no paint→matchable latency, no dead-start). The SECOND letter is a
	// DEPENDENT capture over the per-prefix LIVE set `browser_hints_<prefix>_strict`:
	// a second word that isn't a live option for the spoken prefix is a
	// NON-continuation (silenced; the mid-pair bridge is KEPT), NOT a completion.
	// So a mishear onto a valid-shaped-but-not-shown pair is a pure non-event —
	// your captured first letter and the narrowed HUD stay put; you just say the
	// right second letter. This is the exit-side of visible⇒speakable.
	//
	// This deliberately re-gates ONLY the suffix on `_strict`, reverting that half
	// of the 2026-07-19 sealed migration so the HUD is HONEST (what it lists is
	// exactly what completes). It does NOT revive the deleted grammar-epoch /
	// reservoir / strict-re-push machinery — it only READS the display-grade
	// `_strict` collection at match time. Residual cost: a freshly-scrolled second
	// letter is briefly unmatchable until the next per-scan strict push (silenced,
	// never a dead start; self-heals). The dependent shape compiles to a stable
	// full-alphabet Layer-1 grammar (open states, no per-paint churn) — the same
	// machinery the palette uses in production (palette.go). sealedDispatchSeen
	// (extension) stays as the belt-and-suspenders live-geometry check at dispatch.
	activate := branchkit.Command(
		branchkit.Capture("prefix", "plugin.browser.alpha"),
		branchkit.Capture("suffix", fmt.Sprintf("plugin.%s.hints_${prefix.codeword}_strict", pluginID)),
	).
		Action(pluginID+".activate", sealedHintFields()).
		Description("Activate hint").
		Category("Badges").
		RequiresTags(hintsTag).
		ClearsTags(hintsTag).
		DisplaySource("prefix", prefixList).
		DisplaySource("suffix", fmt.Sprintf("plugin.%s.hints_${prefix.codeword}_strict", pluginID)).
		Build()
	// Caret twin: codeword pairs stay speakable mid-selection — the
	// select_to range-pick chips AND ordinary link activation without
	// exiting the mode. See caretHintTwin.
	commands = append(commands, activate, caretHintTwin(pluginID, activate))

	// Implicit multi-pair stash: two or more bare pairs in one breath open
	// as background tabs — no verb. Min-two is expressed as ONE + ONE-OR-MORE
	// references to the manifest's hint_pair capture macro; a single bare
	// pair leaves this command Partial while the plain activate above
	// Completes and wins, so single-pair clicks are untouched. Lives in the
	// skeleton (not the contribution seam) because it has no spoken
	// vocabulary of its own — pure pattern structure over the pair macro.
	// Reuses the stash action: the extension's per-target fan-out and the
	// non-anchor click fallback give the right degradation everywhere
	// (links background-open; checkboxes/buttons just get clicked).
	// handleOnAction merges `first` into `targets` before the SSE hop.
	// Retains the hints tag like explicit stash — the gather continues.
	implicitStash := branchkit.Command(
		branchkit.Word("<first:hint_pair>"),
		branchkit.Word("<targets:hint_pair>+"),
	).
		Action(pluginID+".activate_hint_background", map[string]any{
			"first":   "{first}",
			"targets": "{targets}",
		}).
		Description("Open several hints in background tabs (no verb — speak the pairs)").
		Category("Badges").
		RequiresTags(hintsTag).
		// Macro-inner slots take the same live display menus as the bare
		// activate (the override threads through the group resume —
		// without it the HUD showed the raw sealed collection after the
		// first pair word, the 2026-07-19 regression).
		DisplaySource("prefix", prefixList).
		DisplaySource("suffix", fmt.Sprintf("plugin.%s.hints_${prefix.codeword}_strict", pluginID)).
		Build()
	commands = append(commands, implicitStash, caretHintTwin(pluginID, implicitStash))

	// There is deliberately NO hint-cancel/mute command here anymore
	// (since 2026-08-05). Its three former
	// roles ("dismiss" pre-2026-08-04, "hide" for a day) each have a proper
	// owner now: mid-utterance cancel is "over"'s job alone (the
	// platform's escape); hiding badges is the extension's toggle_hints (a
	// normal feature verb); and muting the codewords follows visibility
	// automatically — the extension retracts its grammar when badges hide, so
	// there is nothing left for a spoken mute to do.

	// Hover ("hover {hint}") is now contributed by the EXTENSION like every other
	// badge verb (retains_hints:true → doesn't clear the hints tag), via the
	// generic {hint} contribution path (buildContributedHintCommand). It used to
	// be a bespoke command here; consolidating it put every badge verb in one
	// place.

	return commands
}

// caretHintTwin clones a hints-gated CommandSpec with the exclusive caret tag
// added to its gate, keeping the hint system usable INSIDE caret/visual
// selection (user decision 2026-07-25: don't force a mode exit to follow a
// link; the select_to range-pick chips ride the same path — content resolves
// a pending pick before element activation, so a codeword spoken over chips
// picks a range and otherwise activates normally). The [hints, caret]
// conjunction makes the twin modal-eligible — the engine's exclusive filter
// sees the caret tag, so the codeword deck enters the selection-mode grammar
// (the desk-mode `<number>` capture precedent) — and never co-eligible with
// its parent: the parent is exclusively suppressed in-mode, the twin's caret
// requirement is unmet out of mode. The palette deliberately gets NO twin:
// its rows reuse the hint alphabet, so hints there would be ambiguous.
func caretHintTwin(pluginID string, spec branchkit.CommandSpec) branchkit.CommandSpec {
	twin := spec
	twin.RequiresTags = append(append([]string{}, spec.RequiresTags...), caretTagName(pluginID))
	return twin
}

// hintTagSideEffects describes the gate surgery that must follow a hint command
// that clears plugin.browser.hints. The platform applies each command's
// manifest ClearsTags the moment it matches; the gate's cache has to mirror
// that or it drifts — a stuck GrammarOwned state SET-skips the next re-arm,
// leaving codewords painted but un-matchable until a focus reset.
//
// One command group clears the tag (see buildAllHintCommands): activate. It
// must clear the cache, and re-arms only in always-mode — an inline re-put so
// the next utterance lands as a hint. (The spoken muting cancel — "noop",
// words "dismiss"/"hide" — was removed 2026-08-05: muting follows badge
// visibility via the extension's grammar retraction, so no command carries
// that tag clear anymore. Visibility toggling is the extension-owned
// toggle_hints action, which carries no tag ops and so needs no mirror
// here.) The invariant that every clearsTags-of-hints command is mirrored
// here is pinned by TestEveryHintsTagClearingCommandIsGateMirrored.
//
// Pure for testability — handleOnAction calls this under the state mutex.
func hintTagSideEffects(subAction, hintVisibility string) (clearActive bool, rePut bool) {
	switch subAction {
	case "activate":
		return true, hintVisibility == "always"
	default:
		return false, false
	}
}

// mergeImplicitTargets folds the implicit multi-pair stash's split bindings
// (`first` + `targets`, the ONE + ONE-OR-MORE min-two pattern shape) into the
// single ordered `targets` list the extension consumes. No-op for actions
// without a `first` param. Pure — unit-tested.
func mergeImplicitTargets(rawParams map[string]any) {
	first, ok := rawParams["first"]
	if !ok {
		return
	}
	rest, _ := rawParams["targets"].([]any)
	rawParams["targets"] = append([]any{first}, rest...)
	delete(rawParams, "first")
}

// handleOnAction receives every browser.* action from the platform and forwards
// it to connected SSE clients (the browser extension). The action set is fully
// dynamic — the browser extension declares it at runtime — so per-action
// HandleAction registration doesn't fit. We use the low-level Handle("on_action",
// ...) API instead, which is the documented escape hatch for dynamic dispatch.
func (h *Host) handleOnAction(req *branchkit.OnActionRequest) (any, error) {
	subAction := req.Action
	if idx := strings.LastIndex(req.Action, "."); idx >= 0 {
		subAction = req.Action[idx+1:]
	}

	var rawParams map[string]any
	if err := req.UnmarshalParams(&rawParams); err != nil {
		branchkit.Logf("browser", "on_action: bad params for %s: %v", req.Action, err)
	}

	// Tab surgery (tab_to_desk / tab_to_window) is plugin-orchestrated, not a
	// verbatim SSE forward — claim it before the generic path. See tab_desk.go.
	if h.interceptTabSurgeryAction(subAction, rawParams) {
		return branchkit.OnActionResponse{Status: branchkit.OnActionStatusOk}, nil
	}

	// The address bar is browser chrome — no extension API can focus it, so
	// this never forwards: press the browser's own shortcut (Cmd+L on macOS,
	// Ctrl+L elsewhere — keystrokes.go) natively via the keyboard plugin. The
	// app-active gate on the contributed command means the browser is
	// frontmost, so the keystroke lands in it. Async like the surgery flows:
	// the on_action reply must not wait on a nested dispatch round-trip.
	if subAction == "focus_address_bar" {
		// The SDK keys the ambient correlation id by goroutine, so the spawn
		// below would start with none and break the chain. RunWithCorrelation
		// carries it across; the outbound dispatch then stamps it on the
		// envelope like any in-handler call.
		//
		// This used to be done by capturing the id and passing it as the
		// dispatch's trace_id, which correlated nothing: that field held a
		// 32-hex single-use routing credential, so a tr_ in it could only ever
		// be REJECTED, and every address-bar focus earned a warning accusing
		// this plugin of claiming command routing.
		corr := h.plugin.CurrentCorrelation()
		key := shortcutsFor(runtime.GOOS).addressBar
		h.runAsync(func() {
			branchkit.RunWithCorrelation(corr, func() {
				if err := h.pressShortcut(key); err != nil {
					branchkit.Logf("browser", "focus_address_bar: %v", err)
				}
			})
		})
		return branchkit.OnActionResponse{Status: branchkit.OnActionStatusOk}, nil
	}

	mergeImplicitTargets(rawParams)

	// The matched params travel as the JSON object they are — structured
	// values (the multi-target `targets` list) included — instead of being
	// flattened to strings with JSON hidden inside them. `action` names the
	// verb unless the command set its own.
	if rawParams == nil {
		rawParams = map[string]any{}
	}
	if _, set := rawParams["action"]; !set {
		rawParams["action"] = subAction
	}
	event := wire.NewAction(subAction, rawParams)
	event.CorrelationID = h.plugin.CurrentCorrelation()

	h.mu.Lock()
	clientCount := len(h.state.SSEClients)
	tabID := h.state.FocusedTabID
	browserFocused := h.state.BrowserFocused
	clearActive, rePutHintsTag := hintTagSideEffects(subAction, h.state.HintVisibility)
	// Contributed {hint} verbs (blank/stash) aren't in the skeleton's switch;
	// their lifecycle comes from the contribution's retains_hints declaration.
	if ca, rp, ok := h.contributedHintTagEffects(subAction, h.state.HintVisibility); ok {
		clearActive, rePutHintsTag = ca, rp
	}
	// Media verbs follow the media (the tag gating them is an OR over every
	// connection); everything else goes to the browser in front, and only
	// to it.
	var mediaConns []string
	if isMediaAction(subAction) {
		mediaConns = h.mediaTargetConnsLocked(subAction)
	}
	var sent int
	var reason string
	if len(mediaConns) > 0 {
		sent, reason = h.sendToConnsLocked(mediaConns, event)
	} else {
		sent, reason = h.sendToFocusedClientsLocked(event)
	}
	focusedBundle := h.state.FocusedBundleID
	h.mu.Unlock()

	// The activate command's manifest `ClearsTags` already removed the
	// tag platform-side; mirror in the gate without issuing a second
	// Delete. In always-mode we then re-put because the extension's
	// post-activate grammar refresh bails out via lastGrammarHash dedup
	// when the click didn't mutate the DOM — so we can't rely on it to
	// drive the re-put. Skip the re-put if focus already moved off the
	// browser before dispatch landed; the next refocus will re-arm.
	if clearActive {
		h.state.HintGate.markCleared("clearstags_" + subAction)
	}
	if rePutHintsTag && browserFocused {
		h.applyDerivedHintsTag("post_activate_re_put")
	}

	// Reached no browser: an error with the reason, and the person told why
	// (problem.go). It used to answer "ok" — a spoken command that did
	// nothing, indistinguishable from a mishear.
	if sent == 0 {
		branchkit.Logf("browser", "dispatch NOT DELIVERED action=%s reason=%s focused_bundle=%q sse=%d",
			subAction, reason, focusedBundle, clientCount)
		problem := problemForUndelivered(reason, focusedBundle)
		h.reportActionProblem(subAction, problem)
		return actionFailed(problem), nil
	}

	// Surface in branchkit.log so dispatches are visible even if the emitted
	// event stream is unavailable. The codeword/id/elem_type live in
	// params; we extract the common ones for log readability and leave the
	// emitted event as the structured source of truth.
	branchkit.Logf("browser", "dispatch tab=%d action=%s sse=%d codeword=%q id=%s frame_id=%s elem_type=%s",
		tabID, subAction, clientCount,
		event.Param("codeword"), event.Param("id"), event.Param("frame_id"), event.Param("elem_type"))

	h.emitEvent("browser.action_dispatched", map[string]any{
		"action":      subAction,
		"params":      rawParams,
		"sse_clients": clientCount,
		"tab_id":      tabID,
	})

	return branchkit.OnActionResponse{Status: branchkit.OnActionStatusOk}, nil
}
