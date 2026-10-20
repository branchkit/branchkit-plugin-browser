package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// handleAppFocused processes _platform.app.focused notifications from the
// platform and sets/clears "plugin.browser.active" based on whether the
// newly-focused app is a known browser.
//
// Under the per-batch grammar protocol, the first grammar batch is the
// canonical hints-tag trigger — it lands ~100ms after a page load or
// tab switch and that batch's transitionToGrammarOwned fires the tag.
// What we DO keep here:
//   - Skeleton registration on first browser focus, so the hint
//     commands exist by the time the first batch's per-prefix Puts
//     land. Idempotent via HintCommandSkeletonRegistered.
//   - Tag clear on focus-leaves-browser, so the exclusive hints tag
//     doesn't bleed into other apps and suppress non-hint commands.
//   - Re-arm of the hints tag on browser refocus when the user runs
//     `hint_visibility="always"`. An app-level Chrome→other→Chrome
//     focus cycle doesn't trigger a new content-script scan (no DOM
//     mutation, no tab switch, no URL change), so no new grammar
//     batch arrives — without this re-arm the cleared hints tag
//     stays cleared and codewords don't match until the user
//     forces a scan. HintVisibility is "" until the first batch
//     lands, so this skips first-focus-after-launch (the original
//     batch path covers it) and only fires on real refocus events
//     where prior grammar state still applies.
func (h *Host) handleAppFocused(event branchkit.AppFocusedEventParams) {
	if event.BundleID == "" {
		return
	}

	h.mu.Lock()
	h.state.FocusedBundleID = event.BundleID
	h.state.OSFrontmostBundle = event.BundleID
	// OS truth landed after any pending claim: it supersedes the claim, so
	// the claim's verification has nothing left to check.
	if h.state.ClaimVerifyTimer != nil {
		h.state.ClaimVerifyTimer.Stop()
		h.state.ClaimVerifyTimer = nil
	}
	wasFocused := h.state.BrowserFocused
	// The focused source is derived from OS-asserted identity: the focused conn
	// is the live connection whose asserted bundle equals this frontmost bundle,
	// or none if the frontmost app is not a connected browser. This subsumes the
	// old §5 release (a non-browser frontmost yields no conn → release) and the
	// browser-switch wipe, with no correlation to mispair.
	isBrowser, needsRescan := h.recomputeFocusedSourceLocked("app_focused")
	// Frontmost bundle held a connection this session but has none now: the
	// user is looking at a browser whose extension vanished. Arm the
	// stale-disconnect nudge (fires after a grace period unless it
	// reconnects). No-op for never-connected bundles.
	if !isBrowser {
		h.armDisconnectNudgeLocked(event.BundleID)
	}
	// An open episode may belong to a browser the user has since QUIT —
	// which is "standalone by choice", not a fault. Quitting always moves
	// focus, so this is the moment to check; costs nothing while no episode
	// is open.
	episodeOpen := len(h.state.DisconnectNudged) > 0 || h.state.DisconnectNudgeTimer != nil
	visibility := h.state.HintVisibility
	focusedConn := h.state.FocusedConnID
	focusedTab := h.state.FocusedTabID
	h.mu.Unlock()

	branchkit.Logf(h.state.PluginID, "[FOCUS] app focused bundle=%q is_browser=%v focused_conn=%q focused_tab=%d", event.BundleID, isBrowser, focusedConn, focusedTab)

	if episodeOpen {
		// Goroutine: the running-apps RPC must not sit inside the focus path.
		go h.reapQuitEpisodes()
	}
	if needsRescan {
		h.pushRescanToClients()
	}
	if isBrowser && !wasFocused {
		// Focus-gain edge — same guard the focus-claim and active-tab paths
		// use. Without it, every refocus armed TWICE: the extension's unlagged
		// window-focus claim landed first (edge arm), then this OS event
		// arrived ~0.5-2s later and armed again — the rotation-pair signature
		// in the 2026-07-19 storm logs (two full grammar republishes per app
		// switch). Either ordering still arms exactly once: whichever signal
		// lands first takes the edge.
		h.armBrowserFocusUnlocked(h.state.PluginID, event.BundleID, visibility)
	} else if isBrowser {
		// Already-focused browser (the claim beat us to the edge, or a
		// window-to-window hop within the browser). The tag re-arm this
		// branch historically existed for is covered by the derived
		// recompute — zero extension traffic.
		h.applyDerivedHintsTag("app_refocus bundle=" + event.BundleID)
	} else {
		h.releaseBrowserFocusUnlocked(h.state.PluginID, event.BundleID, "focus_lost bundle="+event.BundleID)
	}
}

// activeTagName is the app-active tag: "a connected browser is the app in
// front". Every browser command that acts on the page is gated on it.
const activeTagName = "plugin.browser.active"

// Test seams for the app-active tag, same pattern as caret's
// putCaretTag/deleteCaretTag.
func (h *Host) putActiveTagDefault(string) error {
	return h.plugin.Put(activeTagName, "singleton", struct{}{})
}
func (h *Host) deleteActiveTagDefault(string) error {
	_, err := h.plugin.Delete(activeTagName, "singleton")
	return err
}

// releaseBrowserFocusUnlocked drops the browser-scoped tags when no connected
// browser is in front any more: the app-active tag and the hints tag. The
// inverse of armBrowserFocusUnlocked. Called when focus moves to another app,
// and when the focused browser's connection goes away while it stays in front
// (its extension crashed or reloaded) — the second case used to leave both
// tags set, so browser commands kept matching and went to whatever browser
// was behind. Must NOT hold mu.
//
// Decided and done under mu, against BrowserFocused as it stands NOW rather
// than as the caller saw it: focus inputs arrive on several goroutines (OS
// events, /focus and /active-tab posts, disconnects), so a release decided
// before a newer focus gain must not land after it.
func (h *Host) releaseBrowserFocusUnlocked(pluginID, bundle, reason string) {
	h.mu.Lock()
	if h.state.BrowserFocused {
		h.mu.Unlock()
		branchkit.Logf(pluginID, "[FOCUS] release skipped reason=%s: a browser is in front again", reason)
		return
	}
	if err := h.deleteActiveTag(pluginID); err != nil {
		branchkit.Logf(pluginID, "[FOCUS] %s tag DELETE failed reason=%s: %v", activeTagName, reason, err)
	}
	h.state.HintGate.transitionToIdle(reason)
	h.mu.Unlock()
	h.emitEvent("browser.focus_changed", map[string]any{"bundle_id": bundle, "active": false})
}

// focusedConnSnapshot reads FocusedConnID under the lock, for log lines
// written after it is released.
func (h *Host) focusedConnSnapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.FocusedConnID
}

// armBrowserFocusUnlocked arms the hint surface for a focused browser: marks
// the browser active, ensures the command skeleton, and in always-mode re-arms
// the hints tag. Called on the idle→browser focus-gain EDGE by the focus
// inputs (handleAppFocused, handleFocusPost, handleActiveTabPost,
// resolveAssertedBundle — whichever lands first). Must NOT hold mu — it
// issues RPCs that take the lock themselves.
//
// The recovery rescan does NOT live here: a DIRECT browser→browser switch has no
// idle→browser edge, so an edge-gated rescan would leave the switched-back
// browser voice-dead. The decision belongs to
// recomputeFocusedSourceLocked's transition branch, which every caller runs
// first — callers fire pushRescanToClients when it reports needsRescan.
//
// The Put is decided and done under mu against BrowserFocused as it stands
// now — the mirror of releaseBrowserFocusUnlocked — so an arm decided before
// focus moved to another app cannot land after the release.
func (h *Host) armBrowserFocusUnlocked(pluginID, bundle, visibility string) {
	h.mu.Lock()
	if !h.state.BrowserFocused {
		h.mu.Unlock()
		branchkit.Logf(pluginID, "[FOCUS] arm skipped bundle=%q: no browser in front any more", bundle)
		return
	}
	if err := h.putActiveTag(pluginID); err != nil {
		branchkit.Logf(pluginID, "[FOCUS] %s tag PUT failed bundle=%q: %v", activeTagName, bundle, err)
	}
	h.mu.Unlock()
	h.ensureSkeleton(pluginID)
	if visibility == "always" {
		// Derived, not unconditional: refocusing onto an empty/chrome:// tab
		// must NOT arm (the old unconditional arm left the tag wrongly up
		// until the next active-tab post).
		h.applyDerivedHintsTag("browser_focus bundle=" + bundle)
	}
	h.emitEvent("browser.focus_changed", map[string]any{"bundle_id": bundle, "active": true})
}

// connForAssertedBundleLocked returns the live connection whose OS-asserted
// bundle equals `bundle`, or "". Because a Conn (and so its AssertedBundle)
// is dropped on disconnect, the conns with one ARE the connected-browser set:
// no KnownBrowsers list, no correlation, no wall-clock window. Must hold mu.
//
// Several connections can assert one bundle — two profiles of one browser
// share it, so the OS's frontmost app names both. The pick is then the one
// with the newest standing focus claim: newest by when the claim was MADE
// (Conn.ClaimAt), then by arrival (Conn.FocusSeq); on a tie (no standing claim,
// or both released) the one already focused, then the lowest conn id. It used
// to be whichever map iteration hit first, so the focused profile was random
// on every recompute and a claim from the other profile could not make it
// win; and then the latest to ARRIVE, so a claim delivered late beat one made
// after it.
func (h *Host) connForAssertedBundleLocked(bundle string) string {
	if bundle == "" {
		return ""
	}
	best := ""
	var bestAt int64
	var bestSeq uint64
	for conn, c := range h.state.Conns {
		if c.AssertedBundle != bundle {
			continue
		}
		at, seq := c.ClaimAt, c.FocusSeq
		switch {
		case best == "", at > bestAt, at == bestAt && seq > bestSeq:
		case at < bestAt, seq < bestSeq:
			continue
		case best == h.state.FocusedConnID:
			continue
		case conn != h.state.FocusedConnID && conn > best:
			continue
		}
		best, bestAt, bestSeq = conn, at, seq
	}
	return best
}

// noteConnFocusClaimLocked stamps a connection's focus claim, made at
// `claimedAt` (epoch ms), so connForAssertedBundleLocked picks it among
// connections sharing its bundle unless one of them claimed later. Must hold
// mu.
func (h *Host) noteConnFocusClaimLocked(connID string, claimedAt int64) {
	h.state.FocusSeq++
	c := h.state.conn(connID)
	c.FocusSeq = h.state.FocusSeq
	c.ClaimAt = claimedAt
}

// claimMoment is when a claim was made, in epoch ms: the extension's
// claimed_at, the moment focus changed, or — from an extension that predates
// it, or one whose clock runs ahead — `arrived`. A claim cannot have been made
// after it arrived, so a later claimed_at is not believed: it would win every
// same-bundle pick until released.
func claimMoment(claimedAt int64, arrived time.Time) int64 {
	now := arrived.UnixMilli()
	if claimedAt <= 0 || claimedAt > now {
		return now
	}
	return claimedAt
}

// admitClaimSeqLocked reports whether a claim from connID is newer than the
// last one applied from it, and if so records its seq. A claim without a seq
// (seq 0, an extension that predates it) is always admitted and recorded
// nowhere: arrival order, the old behaviour. Must hold mu.
func (h *Host) admitClaimSeqLocked(connID string, seq uint64) bool {
	if seq == 0 {
		return true
	}
	if seq <= h.state.peek(connID).ClaimSeq {
		return false
	}
	h.state.conn(connID).ClaimSeq = seq
	return true
}

// releaseConnClaimLocked withdraws a connection's standing focus claim — a
// focused:false claim, sent when every window of that browser lost OS focus.
// It stops counting in the same-bundle pick. The focus it installed is not
// undone here: where the user went is the next signal's to say (the OS event
// for another app, or the other profile's own claim), and guessing would
// churn the grammar. The one exception is a claim the OS never confirmed (a
// re-check is pending, see claimVerifyDelay): its author has withdrawn it, so
// ask the OS now instead of after the delay. Returns whether to run that
// check, which the caller does once mu is released. Must hold mu.
func (h *Host) releaseConnClaimLocked(connID string) (verifyNow bool) {
	if c := h.state.Conns[connID]; c != nil {
		c.ClaimAt, c.FocusSeq = 0, 0
	}
	if connID != h.state.FocusedConnID || h.state.ClaimVerifyTimer == nil {
		return false
	}
	h.state.ClaimVerifyTimer.Stop()
	h.state.ClaimVerifyTimer = nil
	return true
}

// focusedSourceFromAsserted returns the connection that OS-asserted identity says
// owns the OS-frontmost bundle — the focused browser — or "" when the frontmost
// app is not a connected browser. Pure read over (FocusedBundleID,
// AssertedBundle). Must hold mu.
func (h *Host) focusedSourceFromAsserted() string {
	return h.connForAssertedBundleLocked(h.state.FocusedBundleID)
}

// recomputeFocusedSourceLocked derives the focused browser source purely from
// OS-asserted identity and applies it. The focused conn is the live connection
// whose AssertedBundle equals the OS-frontmost bundle (FocusedBundleID), or none
// if the frontmost app is not a connected browser. This is the step-3
// replacement for the correlation-driven derivation (reconcile + KnownBrowsers +
// connForBundleLocked + the active-tab backstop): there is nothing to mispair,
// because identity is asserted from the socket, never inferred by pairing a
// claim with a separate app-focus event. Owns the side effects of a
// focused-source change — surrendering the prior browser's grammar on a
// browser→browser switch and reprojecting the new source — so the callers just
// feed an input (OS bundle, claim, or active tab) and recompute. Returns whether
// a browser is now focused, for the caller's tag transitions, and whether the
// extension needs a recovery rescan. Must hold mu.
//
// needsRescan is the recovery decision: true when a
// focused-source TRANSITION just reprojected and came up empty — the kept
// sessions couldn't restore the grammar (browser-switch wipe, plugin restart,
// or a genuinely hintless tab, where the extension's early-out makes the nudge
// a no-op round trip). Keying recovery on the transition rather than on the
// caller's signal is what makes it fire exactly once per real change: two
// signals reporting the same focus gain (the extension's unlagged claim + the
// laggy OS app.focused) produce one transition and one no-op recompute. It is
// also what covers DIRECT browser→browser switches, which have no
// idle→browser edge — gating recovery on the arm edge alone left the
// switched-back browser voice-dead until its next content-script delta.
// Always-mode only; other visibility modes own their grammar lifecycle.
// Callers must fire pushRescanToClients AFTER releasing mu.
func (h *Host) recomputeFocusedSourceLocked(reason string) (isBrowser, needsRescan bool) {
	osBundle := h.state.FocusedBundleID
	newConn := h.connForAssertedBundleLocked(osBundle)
	// The dev focus pin (focus_pin.go) overrides the OS: the pinned
	// connection is the focused source whatever app is in front, and the
	// rest of this function treats it exactly as a frontmost browser. The
	// pin is only ever a live connection — it is refused for any other and
	// cleared when its connection goes.
	if pinned := h.pin.current(); pinned != "" {
		newConn = pinned
		osBundle = h.state.peek(pinned).AssertedBundle
	}
	prevConn := h.state.FocusedConnID
	prevTab := h.state.FocusedTabID

	if newConn == "" {
		// Frontmost app is not a connected browser. Release the focused slot but
		// keep LastFocusedConnID (and FocusedBrowserBundleID) sticky, so a return
		// to the prior browser reads as a refocus (no spurious switch-wipe), and a
		// hop to a DIFFERENT connection still parks the prior one.
		if prevConn != "" {
			h.state.FocusedConnID = ""
			h.state.FocusedTabID = 0
			branchkit.Logf(h.state.PluginID, "[FOCUS] recompute trigger=%s os_bundle=%q released focused_conn=%q",
				reason, osBundle, prevConn)
		}
		h.state.BrowserFocused = false
		// An exclusive palette tag must not survive the browser losing OS
		// focus — it would suppress every other app's commands. The palette
		// page also closes itself on blur; this is the plugin-side backstop.
		h.drainPaletteLocked(h.state.PluginID, "browser_unfocused")
		// Same for the exclusive caret tag (caret.go). The extension exits caret
		// mode on blur too; this is the backstop.
		h.drainCaretLocked(h.state.PluginID, "browser_unfocused")
		h.drainVideoModeLocked(h.state.PluginID, "browser_unfocused")
		h.drainAutoScrollLocked(h.state.PluginID, "browser_unfocused")
		h.drainQueryFieldLocked(h.state.PluginID, "browser_unfocused")
		// media_active deliberately NOT drained here — controlling a
		// background video from an unrelated app is its whole point
		// (video.go); slots drop on their conn's SSE disconnect instead.
		return false, false
	}

	lastConn := h.state.LastFocusedConnID
	h.state.FocusedConnID = newConn
	h.state.LastFocusedConnID = newConn
	h.state.FocusedBrowserBundleID = osBundle
	h.state.BrowserFocused = true
	h.state.FocusedTabID = h.state.peek(newConn).ActiveTab
	// A switch is a change of CONNECTION, not of bundle: two profiles of one
	// browser share a bundle, and a profile switch must park the loser's
	// grammar exactly as a browser switch does. lastConn is sticky across
	// non-browser focus, so Chrome → Terminal → Chrome is still a refocus.
	switched := lastConn != "" && lastConn != newConn

	// Connection → different connection: PARK the losing connection's
	// grammar — its collection entries drain so its
	// codewords stop matching against the one now focused, but its sessions
	// survive for zero-traffic reprojection when focus returns. Runs with the
	// new source already set, so the park's re-sync and the reproject below
	// rebuild from the new browser. Destruction is reserved for SSE
	// disconnect (cleanupConnSessionsLocked), where the conn can never
	// come back.
	if switched {
		h.parkConnSessionsLocked(h.state.PluginID, lastConn)
	}

	// The tab collection follows the focused CONNECTION (it's per-browser,
	// not per-tab): on a source change, project the new browser's published
	// tab words — and, symmetrically, drain the old browser's (its tab ids
	// are another browser's numbering). New words ride the debounced
	// vocabulary commit; the timer takes `mu` on its own goroutine.
	if newConn != prevConn || switched {
		if h.syncTabsCollectionLocked(h.state.PluginID) {
			h.scheduleVocabularyCommitLocked(h.state.PluginID)
		}
		// A palette session belongs to the browser that opened it; a focus
		// handoff to a different connection ends it (row ids are the old
		// browser's numbering, and its overlay is no longer frontmost).
		if h.state.PaletteConnID != "" && h.state.PaletteConnID != newConn {
			h.drainPaletteLocked(h.state.PluginID, "focus_switch")
		}
		// A caret session likewise belongs to the browser that started it,
		// and so do video mode and the query-field flag — each checked against
		// its OWN connection. They used to drain only inside the caret check,
		// so video mode started with caret off kept its exclusive tag while a
		// second browser was in front, blocking that browser's commands.
		if h.state.CaretConnID != "" && h.state.CaretConnID != newConn {
			h.drainCaretLocked(h.state.PluginID, "focus_switch")
		}
		if h.state.VideoModeConnID != "" && h.state.VideoModeConnID != newConn {
			h.drainVideoModeLocked(h.state.PluginID, "focus_switch")
		}
		if h.state.QueryFieldConnID != "" && h.state.QueryFieldConnID != newConn {
			h.drainQueryFieldLocked(h.state.PluginID, "focus_switch")
		}
		// Keep-scrolling checks its OWN publisher, not caret's: a run started
		// with caret off must still drain when another browser takes focus.
		if h.state.AutoScrollConnID != "" && h.state.AutoScrollConnID != newConn {
			h.drainAutoScrollLocked(h.state.PluginID, "focus_switch")
		}
	}
	// The connection now in front gets the modes its session asks for: one it
	// asked for while another browser was in front, or before focus was
	// known, was deferred rather than refused (session.go), and the unfocus
	// drain above released the ones it held when it last lost focus.
	if newConn != prevConn {
		h.applySessionLocked(h.state.PluginID, newConn, "focus_gain")
	}

	// Grammar follows focus: rebuild from the now-focused source whenever the
	// source or its tab changed (or a switch wiped the prior browser), so its
	// codewords are commandable without waiting for a rescan. A pure refocus of
	// the same conn+tab is a no-op.
	if newConn != prevConn || h.state.FocusedTabID != prevTab || switched {
		// Same-browser tab switch: remove the prior tab's contribution from the
		// live projection here, plugin-side. The SW's tab-level session_end no
		// longer drives this — an older SW that still sends it just makes this
		// a no-op.
		if newConn == prevConn && prevTab != 0 && prevTab != h.state.FocusedTabID {
			h.deprojectTabSessionsLocked(h.state.PluginID, newConn, prevTab, "focus_recompute_tab_switch")
		}
		// Revive the newly-focused tab's kept-but-deprojected sessions first:
		// clear SweptStale and refresh LastBatchAt so the dormant-TTL
		// exclusion doesn't skip sessions that merely aged in the background
		// — without this, the reproject below sees an empty source for any
		// tab backgrounded longer than the TTL.
		h.reviveTabSessionsLocked(newConn, h.state.FocusedTabID)
		h.reprojectFocusedSourceLocked(h.state.PluginID)
		// Recovery decision, per the doc comment above: the projection just
		// ran for a real transition — if it restored nothing, the extension
		// must rebuild from its authoritative wrapper store. Do not widen this
		// beyond always-mode: in manual mode a hidden tab is legitimately empty.
		// Arming is handled post-commit by applyDerivedHintsTag, not here.
		needsRescan = h.state.HintVisibility == "always" && !h.focusedTabHasGrammarLocked()
		branchkit.Logf(h.state.PluginID,
			"[FOCUS] recompute trigger=%s os_bundle=%q focused_conn=%q->%q tab=%d switched=%v needs_rescan=%v",
			reason, osBundle, prevConn, newConn, h.state.FocusedTabID, switched, needsRescan)
	}
	return true, needsRescan
}

// handleFocusPost processes the extension's focus claim (POST /focus). A
// focused:true claim means "a window of this connection's browser just became
// OS-focused". It rides chrome.windows.onFocusChanged, so it is unlagged — the
// fast path that beats the macOS app-focus event (which can lag >1s on Firefox).
// The claim sets the OS-frontmost bundle to the claiming connection's OWN
// asserted bundle and recomputes; on the idle→browser transition it arms. No
// fork list and no correlation: identity is the socket-asserted bundle, so any
// browser — vanilla or fork — is recognized the instant its claim lands.
//
// Claims are separate requests, so they can arrive out of order. Two optional
// fields order them (an extension that predates them sends neither and is
// applied in arrival order, as before):
//
//   - seq, per connection, increasing in the order focus changed. A claim
//     not newer than the last one applied from its connection is dropped
//     whole — a focus gain delivered after the blur that followed it is
//     exactly this.
//   - claimed_at, epoch ms of the change. Orders claims ACROSS connections
//     where the OS cannot: two profiles of one browser share its app.
//
// A focused:false claim releases the connection's claim
// (releaseConnClaimLocked).
func (h *Host) handleFocusPost(_ *http.Request, body *wire.FocusRequest) (wire.Ack, error) {
	arrived := time.Now()
	if body.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	var armBundle, armVis string
	var needsRescan, verifyNow bool
	h.mu.Lock()
	// Order first: a stale claim carries stale everything, hint_visibility
	// included, so none of it is applied.
	if !h.admitClaimSeqLocked(body.ConnID, body.Seq) {
		last := h.state.peek(body.ConnID).ClaimSeq
		h.mu.Unlock()
		branchkit.Logf(h.state.PluginID, "[FOCUS] stale focus claim dropped conn=%q focused=%v seq=%d (applied %d)",
			body.ConnID, body.Focused, body.Seq, last)
		// Not a refusal: the claim was delivered, and dropping it is the
		// ordering working. The log line above is the record.
		return ack()
	}
	// Refresh visibility from the SW's authoritative value before recompute, so
	// the empty-source recovery-rescan derivation (recomputeFocusedSourceLocked)
	// reads the true mode instead of a value lagging the last grammar batch.
	if body.HintVisibility != "" {
		h.state.HintVisibility = body.HintVisibility
	}
	if body.Focused {
		// Record the OS-focused window for this connection — the window-focus
		// arbitration input batchIsFocusedSourceLocked matches window_id against.
		// The SW reads it straight off chrome.windows.onFocusChanged, so it is the
		// least-lossy focus signal in the system; a window switch within one
		// browser updates it here without depending on the /active-tab query
		// chain. Set even if the asserted bundle hasn't resolved yet (below),
		// since the window is known regardless. window_id==0 means the SW couldn't
		// supply one; leave the prior value rather than blanking it.
		if body.WindowID != 0 {
			h.state.conn(body.ConnID).FocusedWindow = body.WindowID
		}
		// The claimant is the connection now in front — stamp it so every
		// recompute picks IT among connections sharing its bundle (two
		// profiles of one browser), rather than whichever it met first —
		// unless another of them claimed later. Stamped even before its
		// identity resolves: the recompute that resolveAssertedBundle runs
		// then already knows who is in front.
		h.noteConnFocusClaimLocked(body.ConnID, claimMoment(body.ClaimedAt, arrived))
	} else {
		verifyNow = h.releaseConnClaimLocked(body.ConnID)
	}
	if body.Focused {
		// The claiming connection's window just became OS-frontmost — the claim
		// rides chrome.windows.onFocusChanged, so it is unlagged (covers the
		// Firefox app-focus lag the OS event suffers). Use the claiming conn's
		// OWN asserted bundle as the frontmost bundle: no correlation against a
		// separate app-focus event, so nothing to mispair. If the assertion
		// hasn't resolved yet (a just-connected conn), do nothing — the eventual
		// resolveAssertedBundle recomputes when it lands.
		if asserted := h.state.peek(body.ConnID).AssertedBundle; asserted != "" {
			wasFocused := h.state.BrowserFocused
			// A claim that contradicts the OS's latest report is either early
			// (the OS event lags — the reason claims exist) or LATE: sent when
			// this browser gained focus, delivered after the user had already
			// moved to another app. Nothing orders the two, and a late claim
			// applied here stays applied — the OS sent its event already.
			// Check the OS again once any lag has passed (verifyFocusClaim).
			if reported := h.state.OSFrontmostBundle; reported != "" && reported != asserted {
				h.armClaimVerifyLocked()
			}
			h.state.FocusedBundleID = asserted
			var isBrowser bool
			isBrowser, needsRescan = h.recomputeFocusedSourceLocked("focus_claim")
			if isBrowser && !wasFocused {
				armBundle = asserted
				armVis = h.state.HintVisibility
			}
		}
	}
	// A focused:false claim does not recompute: a blur surfaces as an app-focus
	// event for the newly-frontmost app, which recomputes and releases, and a
	// switch to the other profile as that profile's own claim.
	h.mu.Unlock()

	if verifyNow {
		branchkit.Logf(h.state.PluginID, "[FOCUS] focused conn=%q released an unconfirmed claim — asking the OS now", body.ConnID)
		h.runAsync(h.verifyFocusClaim)
	}
	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		branchkit.Logf(h.state.PluginID, "[FOCUS] focus claim conn=%q bundle=%q (arming)", body.ConnID, armBundle)
		h.armBrowserFocusUnlocked(h.state.PluginID, armBundle, armVis)
	}
	return ack()
}

// claimVerifyDelay is how long after a claim that contradicts the OS's latest
// report the plugin asks the OS again. Past the OS app-focus lag the claim
// exists to beat (over a second on Firefox): by then the OS either confirmed
// the claim with its own event (which cancels the check) or still names
// another app, and the claim was late.
//
// Kept once claims gained an order (seq, claimed_at), as the backstop for
// what the order cannot see. Ordered claims mostly settle a late gain
// themselves: it is dropped when the blur that followed it arrived first, and
// withdrawn — this check then runs at once — when that blur arrives after it.
// What is left is this check's: an extension that predates seq, whose late
// gain nothing drops, and a blur that never arrives, since claims are
// best-effort POSTs. It costs one OS query, only after a claim the OS
// contradicted.
const claimVerifyDelay = 2 * time.Second

// armClaimVerifyLocked (re)starts the claim verification timer. Must hold mu.
func (h *Host) armClaimVerifyLocked() {
	if h.state.ClaimVerifyTimer != nil {
		h.state.ClaimVerifyTimer.Stop()
	}
	h.state.ClaimVerifyTimer = time.AfterFunc(claimVerifyDelay, h.verifyFocusClaim)
}

// frontmostBundleDefault asks the OS which app is in front now. Seam.
func (h *Host) frontmostBundleDefault() (string, error) {
	res, err := h.plugin.NativeFrontmostApp()
	if err != nil {
		return "", err
	}
	if res.App == nil || res.App.BundleID == nil {
		return "", nil
	}
	return *res.App.BundleID, nil
}

// verifyFocusClaim re-reads the OS frontmost app after a contradicting claim
// and, when the OS disagrees with the focus the claim installed, applies the
// OS answer as an app-focus event would. A late claim thus arms the browser
// tags for at most claimVerifyDelay while another app is in front, instead of
// until the next app switch.
func (h *Host) verifyFocusClaim() {
	h.mu.Lock()
	h.state.ClaimVerifyTimer = nil
	h.mu.Unlock()
	front, err := h.frontmostBundle()
	if err != nil || front == "" {
		return
	}
	h.mu.Lock()
	current := h.state.FocusedBundleID
	h.mu.Unlock()
	if front == current {
		return
	}
	branchkit.Logf(h.state.PluginID, "[FOCUS] claim for %q was late — the OS has %q in front; applying the OS answer", current, front)
	h.handleAppFocused(branchkit.AppFocusedEventParams{BundleID: front})
}

// focusedTabHasGrammarLocked reports whether the focused source's focused tab
// has any live codeword sessions — the input for the derived hints tag:
// the tag is a function of the
// focused projection, not an edge-triggered write. Must hold mu.
func (h *Host) focusedTabHasGrammarLocked() bool {
	if h.state.FocusedConnID == "" || h.state.FocusedTabID == 0 {
		return false
	}
	for key, sess := range h.state.FrameSessions {
		if key.ConnID == h.state.FocusedConnID && key.TabID == h.state.FocusedTabID && len(sess.Codewords) > 0 {
			return true
		}
	}
	return false
}

// applyDerivedHintsTag recomputes the hints tag from the focused projection
// — the single arm/drop policy point: the tag is a function of "does the focused tab hold live grammar
// while a known browser is OS-focused", never an edge-triggered write.
//
// Mode-agnostic: visible ⇒ speakable holds
// in manual mode too, so the derivation must NOT gate on `always`. The former
// `vis != "always"` early-out left manual-mode `F`/"show" projecting grammar
// with no armed tag (voice-dead badges). Visibility is already carried by the
// session's emptiness — hidden ⇒ the extension drops puts / retracts, so the
// focused session is empty and this idles; shown ⇒ codewords present and this
// arms — exactly the ratified emptiness-aware arm. The `BrowserFocused` guard
// (kept) is the real defense against a background app's batches arming the tag;
// the `always` check was only a visibility proxy. Callers must NOT hold mu.
//
// Decides AND acts under one mu hold. It used to decide, release the lock,
// then act — so an arm decided while the browser was in front could land
// after the app-focus handler had cleared the tag, leaving the exclusive hints
// tag up in another app. Lock order is mu then the gate's own lock; the gate
// never takes mu.
func (h *Host) applyDerivedHintsTag(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	arm := h.state.BrowserFocused && h.focusedTabHasGrammarLocked()
	tab := h.state.FocusedTabID
	if arm {
		h.state.HintGate.transitionToGrammarOwned(fmt.Sprintf("%s tab=%d derived", reason, tab))
	} else {
		h.state.HintGate.transitionToIdle(fmt.Sprintf("%s tab=%d derived_empty", reason, tab))
	}
}

// handleActiveTabPost processes the extension's active-tab signal (POST
// /active-tab). The extension reports which tab is active in its window
// whenever that changes (tab switch, window focus gain).
//
// We ALWAYS record the reporting connection's active tab (Conn.ActiveTab), even
// when it isn't the focused browser. That way, when OS focus later flips to it,
// the recompute derives FocusedTabID from the already-known value with no
// ordering race. The recompute then advances FocusedTabID only for the focused
// connection (a background connection's report is recorded but doesn't move the
// focused tab).
func (h *Host) handleActiveTabPost(_ *http.Request, body *wire.ActiveTabRequest) (wire.Ack, error) {
	if body.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	var armBundle, armVis string
	h.mu.Lock()
	// Fresh visibility before recompute — same reason as handleFocusPost.
	if body.HintVisibility != "" {
		h.state.HintVisibility = body.HintVisibility
	}
	// Always record the reporting connection's active tab, even when it isn't the
	// focused browser, so a later focus flip to it picks up the right tab with no
	// ordering race. The recompute then advances FocusedTabID + reprojects iff
	// this report is for the focused source (its active tab actually changed);
	// for a background connection it's a no-op. It can also be the signal that
	// first focuses a browser whose asserted bundle just resolved.
	h.state.conn(body.ConnID).ActiveTab = body.TabID
	wasFocused := h.state.BrowserFocused
	isBrowser, needsRescan := h.recomputeFocusedSourceLocked("active_tab")
	if isBrowser && !wasFocused {
		armBundle = h.state.FocusedBrowserBundleID
		armVis = h.state.HintVisibility
	}
	focusedConn := h.state.FocusedConnID
	h.mu.Unlock()

	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		branchkit.Logf(h.state.PluginID, "[FOCUS] active-tab armed conn=%q bundle=%q", body.ConnID, armBundle)
		h.armBrowserFocusUnlocked(h.state.PluginID, armBundle, armVis)
	}
	// Derived hints tag: on every focused recompute the tag follows the
	// focused tab's projection. A tab with revived grammar arms without any
	// republish; a tab with none (fresh tab, chrome:// page) drops the tag —
	// the first grammar batch re-arms via the post-commit
	// applyDerivedHintsTag("batch_commit") when the scan lands.
	if isBrowser {
		h.applyDerivedHintsTag("active_tab")
	}
	branchkit.Logf(h.state.PluginID, "[FOCUS] active tab conn=%q tab=%d focused_conn=%q",
		body.ConnID, body.TabID, focusedConn)
	return ack()
}

// primeFocusedBundle reads the currently-focused app at startup and seeds
// FocusedBundleID / BrowserFocused so the plugin knows the focused source
// before the first _platform.app.focused notification arrives.
//
// The platform only sends focus notifications on a focus *change* — a freshly
// (re)started plugin gets no event until the user next switches apps. Querying
// native.frontmost_app once at boot seeds that state immediately, so the
// hints-tag arm (gated on BrowserFocused) and focus-conn resolution are
// correct from the first batch rather than waiting for a switch.
//
// No longer load-bearing for clobber-safety — the plugin
// projects only the focused source's grammar, so an unknown focused bundle at
// boot just means "project last-known / the lone batch source" rather than
// "accept and clobber." Best-effort: on error or a nil app we leave the seed
// empty until the first focus event lands.
func (h *Host) primeFocusedBundle(pluginID string) {
	res, err := h.plugin.NativeFrontmostApp()
	if err != nil {
		branchkit.Logf(pluginID, "[FOCUS] prime frontmost_app failed: %v", err)
		return
	}
	if res.App == nil || res.App.BundleID == nil || *res.App.BundleID == "" {
		branchkit.Logf(pluginID, "[FOCUS] prime frontmost_app: no focused app")
		return
	}
	bundle := *res.App.BundleID

	h.mu.Lock()
	h.state.FocusedBundleID = bundle
	h.state.OSFrontmostBundle = bundle
	// Derive BrowserFocused from asserted identity. Usually a no-op at boot (no
	// extension has connected yet) — resolveAssertedBundle arms once the browser's
	// SSE connect resolves its bundle. But if the connect+assertion already landed
	// (it raced ahead of this prime), the browser is frontmost now and we arm
	// here. Either ordering arms exactly once. No KnownBrowsers seed needed.
	wasFocused := h.state.BrowserFocused
	isBrowser, needsRescan := h.recomputeFocusedSourceLocked("prime")
	var armBundle, armVis string
	if isBrowser && !wasFocused {
		armBundle = h.state.FocusedBrowserBundleID
		armVis = h.state.HintVisibility
	}
	// The cold-start half of the tripwire: a browser already frontmost —
	// and already wedged — at launch produces no focus event to arm on, so
	// the persisted seen-set alone closes nothing (seen_browsers.go). The
	// prime is the one place that knows the frontmost bundle at boot. Arm
	// via a startup allowance rather than immediately: at boot a healthy
	// extension may take its full 30s service-worker alarm to reconnect,
	// and the per-focus grace alone would false-alarm on every restart.
	primeNudgeCandidate := ""
	if !isBrowser && h.state.SeenBundles[bundle] {
		primeNudgeCandidate = bundle
	}
	h.mu.Unlock()

	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		h.armBrowserFocusUnlocked(pluginID, armBundle, armVis)
	}
	if primeNudgeCandidate != "" {
		time.AfterFunc(coldStartNudgeAllowance, func() {
			h.mu.Lock()
			// Still what the user is looking at? A nudge is actionable at
			// the moment of focus and noise otherwise; if focus moved, the
			// ordinary focus-event arm owns the case now.
			if h.state.FocusedBundleID == primeNudgeCandidate {
				h.armDisconnectNudgeLocked(primeNudgeCandidate)
			}
			h.mu.Unlock()
		})
	}
	branchkit.Logf(pluginID, "[FOCUS] primed focused bundle=%q is_browser=%v", bundle, isBrowser)
}

// pushRescanToClients fans a "reactivate" ActionEvent out to the focused
// browser's SSE clients. RECOVERY path only: fired
// by recompute callers when a focused-source transition reprojected and came
// up empty — a browser-switch wipe or plugin restart left the plugin with no
// state for the tab the user is now looking at, so the extension must
// rebuild from its authoritative wrapper store (a forced re-send — an
// ordinary send would skip content the plugin last accepted, which is exactly
// what it wiped). The common refocus case never gets
// here: the recompute's reprojection restores kept sessions with zero
// extension traffic. Non-blocking per channel — clients with a full inbox
// drop the event; the next transition re-prompts.
//
// target=active routes to the active tab only (a broadcast would add
// hundreds of ms on a multi-tab session for no benefit — only the active
// tab's state matters for what the user can speak).
func (h *Host) pushRescanToClients() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sendToFocusedClientsLocked(wire.NewAction("reactivate", wire.ReactivateParams{Target: "active", Reason: "app_refocus"}))
}

// ensureSkeleton registers the static hint-command skeleton (trigger
// / hide / activate / cancel) once per plugin lifetime; idempotent.
//
// No vocabulary is seeded here: the 26 hint codewords are already
// permanently in the union grammar via the voice plugin's `alphabet`
// collection (key_field "codeword", command-referenced by the press/key
// commands). browser_hints_prefix isn't seeded either — the per-batch
// handler pushes the full cumulative prefix state, and seeding it would
// violate the badge-implies-functional contract.
//
// Called by handleAppFocused (first browser focus),
// handleHintsSessionStartPost (tab switch into always-mode), and the
// per-batch handler (first batch of a session).
//
// Not claimed while the alphabet is still absent: the skeleton builder returns
// nothing then, the EMPTY group pushes successfully, and a claim taken on that
// push stuck for the plugin's lifetime — a first-run boot race that left no
// `activate` command at all. Unclaimed, the next caller (every grammar batch
// calls this) registers it once the alphabet has arrived.
func (h *Host) ensureSkeleton(pluginID string) {
	h.mu.Lock()
	if h.state.HintCommandSkeletonRegistered {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.refreshCodewords()
	if h.snapshotCodewords() == nil {
		branchkit.Logf(pluginID, "hint command skeleton: alphabet not loaded yet — not registering (the next batch retries)")
		return
	}
	h.mu.Lock()
	if h.state.HintCommandSkeletonRegistered {
		h.mu.Unlock()
		return
	}
	h.state.HintCommandSkeletonRegistered = true
	h.mu.Unlock()
	h.registerHintCommandSkeleton(pluginID)
}

// handleSessionEnded resets the extension's hint visuals when a voice session
// ends. The pair tag is cleared by lifecycle, but the extension needs an
// explicit signal to un-narrow its display. In "always" mode, re-sets the
// hints tag so prefix commands are available on the next hold — but only
// while a known browser is the focused app. Without the browser-active
// gate, the exclusive hints tag would suppress focus/switch commands
// system-wide every time the user released the command key in another app.
func (h *Host) handleSessionEnded() {
	h.mu.Lock()
	visibility := h.state.HintVisibility
	browserFocused := h.state.BrowserFocused
	clientCount := len(h.state.SSEClients)
	// A reset, not an action: every browser un-narrows. Focus-scoping it
	// would strand a display narrowed by a prefix the user spoke before
	// switching apps.
	h.broadcastToClientsLocked(wire.NewAction("noop", wire.NoopParams{}))
	h.mu.Unlock()

	// Only re-put the hints tag if a known browser is actually focused.
	// Read directly from BrowserFocused (tracked from app.focused events)
	// rather than HintGate.IsActive() — the gate can be Active from a
	// stale background batch even when the user has moved off the
	// browser, and using it as a proxy would re-put the tag into
	// whatever app the user is currently in.
	resetHints := visibility == "always" && browserFocused
	h.emitEvent("browser.session_reset", map[string]any{
		"hint_visibility": visibility,
		"reset_hints_tag": resetHints,
		"sse_clients":     clientCount,
	})

	if resetHints {
		h.applyDerivedHintsTag("voice_session_reset")
	}
}

// registerHintCommandSkeleton pushes the static hint-command surface
// (trigger / hide / activate / cancel) the first time a browser is
// focused. Per-batch grammar Puts populate the per-prefix collections
// these commands bind against; the skeleton itself is constant across
// sessions. Idempotent via `HintCommandSkeletonRegistered`.
//
// `appTag` is hardcoded to `plugin.browser.active`, which gates the
// trigger / hide commands to "browser is focused."
func (h *Host) registerHintCommandSkeleton(pluginID string) {
	// Pushing just the hint skeleton is what this always wanted to do. It
	// used to REPLACE the plugin's whole command set, wiping the
	// scroll/find/reference commands other goroutines had pushed, so it was
	// routed through a union of all five sources instead. With a group of its
	// own it can go back to pushing only its own commands.
	if err := h.pushCommandGroup(pluginID, groupHints); err != nil {
		branchkit.Logf(pluginID, "hint command skeleton: push failed: %v", err)
		// Roll back the claim so the next caller can retry.
		h.mu.Lock()
		h.state.HintCommandSkeletonRegistered = false
		h.mu.Unlock()
		return
	}
	branchkit.Logf(pluginID, "hint command skeleton: registered")
}

// buildHintSkeletonCommands returns the hint trigger/hide/activate/cancel
// command set, or nil when codewords haven't loaded yet. Pure — no RPC.
// Called from pushAllCommands.
func (h *Host) buildHintSkeletonCommands(pluginID string) []branchkit.CommandSpec {
	h.refreshCodewords()
	wordsSlice := h.snapshotCodewords()
	if wordsSlice == nil {
		return nil
	}
	var words [26]string
	for i, w := range wordsSlice {
		if i >= 26 {
			break
		}
		words[i] = w
	}
	return buildAllHintCommands(pluginID, words, "plugin.browser.active")
}

// handleCaptureProgress receives the platform's matcher signal that a
// partial command match bound one step of a multi-step capture and is
// waiting for the next.
// For the hint pattern this fires the moment the user speaks the prefix
// codeword ("arch"); we forward a synthetic noop+prefix ActionEvent to SSE
// clients so the extension can gray out codewords that can't finish it.
//
// Keyed on the STRUCTURAL property — this plugin owns the command, and a
// further capture step is still pending — NOT on the bound step being named
// `prefix`. Keying on the name meant every other multi-step capture, on
// every other surface, silently forwarded nothing; the palette's row badges
// are the first surface to need it.
//
// Which surface a forwarded narrow applies to is decided extension-side by
// the holder registry (an exclusive holder swallows it), not here — the
// plugin reports that a prefix was bound; it does not route it.
//
// The platform ADDRESSES this event to the plugin that owns the command, so
// it only ever arrives for our own commands and needs no speech grant; the
// owner check below is a guard, not the filter.
//
// `captured` holds words the user said. They are forwarded to the extension
// (that is the point) but never logged: the lines below carry capture NAMES,
// counts and client counts only, never the payload or a bound value.
func (h *Host) handleCaptureProgress(event branchkit.CaptureProgressEventParams) {
	if event.OwnerPlugin != h.state.PluginID {
		branchkit.Logf("browser", "capture.progress: owner_plugin=%q != %q, skipping", event.OwnerPlugin, h.state.PluginID)
		return
	}
	// A still-pending next step is what makes this progress rather than a
	// completed match. Without one there is nothing left to narrow toward.
	if event.NextCapture == "" {
		branchkit.Logf("browser", "capture.progress: no pending next_capture, skipping")
		return
	}
	prefix, ok := boundCodeword(event.Captured)
	if !ok {
		branchkit.Logf("browser", "capture.progress: no single bound codeword (%d captures bound), skipping",
			len(event.Captured))
		return
	}

	h.mu.Lock()
	clients := len(h.state.SSEClients)
	h.sendToFocusedClientsLocked(wire.NewAction("noop", wire.NoopParams{Prefix: prefix}))
	h.mu.Unlock()
	branchkit.Logf("browser", "capture.progress: forwarded the bound prefix (next=%q) to %d SSE clients",
		event.NextCapture, clients)
}

// boundCodeword pulls the spoken codeword out of the one bound step of an
// in-progress multi-step capture. Entries are object-shaped
// ({codeword, letter}) because the matcher binds the whole collection row to
// the capture.
//
// `captured` is a map and carries no step ordering, so a capture with two or
// more steps already bound has no well-defined "prefix so far". Refuse rather
// than guess: every pattern today is two-step, so that case is unreachable,
// and a three-step capture must bring its own ordering rather than inherit a
// coin flip from map iteration order.
func boundCodeword(captured map[string]json.RawMessage) (string, bool) {
	if len(captured) != 1 {
		return "", false
	}
	for _, raw := range captured {
		// Two shapes bind at a step boundary: the hints family's record
		// values ({codeword, letter}) and — since the `@palette` surface
		// migration — the platform's prefix-index entries, whose captured
		// value is the bare prefix WORD (a named-entities scalar).
		var word string
		if err := json.Unmarshal(raw, &word); err == nil && word != "" {
			return word, true
		}
		var row struct {
			Codeword string `json:"codeword"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.Codeword == "" {
			return "", false
		}
		return row.Codeword, true
	}
	return "", false
}
