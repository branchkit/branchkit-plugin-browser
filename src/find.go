package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// Find-session voice gating. The extension's find (a content-side session:
// bar open OR committed highlights with n/N live — it outlives the bar and
// survives blur) holds the NON-exclusive `plugin.<id>.find` tag while active,
// gating the contributed find-jump commands (voice "next"/"previous"). Without
// the gate those bare words were eligible any time a browser was focused —
// no-ops that swallowed decodes, a mishear magnet, and a fluid prefix shadowing
// "next tab"/"next page" (terminal-command rule).
//
// Caret's shape (caret.go) minus two things:
//   - NOT exclusive: find-jump augments browsing; everything else stays live.
//     A stuck tag has no system-wide blast radius, so the drain story is
//     lighter.
//   - NOT drained on browser unfocus / focus switch: unlike caret, the find
//     session does not end on blur (highlights persist), so an unfocus drain
//     would desync tag from content state — "next" dead over visible
//     highlights after a refocus. Focus is handled orthogonally: the
//     contributed commands require app-active AND this tag (contribute.go),
//     so an unfocused browser's find never makes "next" eligible. Drains are
//     only the truth-preserving ones: the extension's own exit (its
//     session's find going false) and the publisher's SSE disconnect.
//
// Multi-browser edge (accepted): find open in browser A, focus on browser B —
// the tag (A's) plus app-active (B's) leaves "next" eligible and it no-ops in
// B. Rare, harmless, and the single-slot shape matches caret/palette.

func findTagName(pluginID string) string {
	return "plugin." + pluginID + ".find"
}

// Test seams, same pattern as caret's putCaretTag/deleteCaretTag.
func (h *Host) putFindTagDefault(pluginID string) error {
	return h.plugin.Put(findTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteFindTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(findTagName(pluginID), "singleton")
	return err
}

// syncFindLocked aligns the tag with state.FindActive, issuing Put/Delete only
// on transitions. Must be called under `mu`.
//
// RPC first, bookkeeping on success — caret.go's syncCaretLocked carries the
// full reasoning. FindTagSet is the sole record that the platform still holds
// the tag and the thing that keeps drainFindLocked from short-circuiting, so
// clearing it ahead of a Delete that then fails strands the tag with no retry
// path. This tag is non-exclusive, so a leak costs only stray "next"/"previous"
// eligibility rather than system-wide suppression — but the ordering is one
// rule for all four mirrors, not a per-tag judgement about blast radius.
func (h *Host) syncFindLocked(pluginID string) {
	if h.state.FindActive && !h.state.FindTagSet {
		if err := h.putFindTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "find tag PUT failed: %v", err)
			return
		}
		h.state.FindTagSet = true
		branchkit.Logf(pluginID, "[TAGS] find tag SET conn=%q", h.state.FindConnID)
	} else if !h.state.FindActive && h.state.FindTagSet {
		if err := h.deleteFindTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "find tag DELETE failed (still believed SET — next drain retries): %v", err)
			return
		}
		h.state.FindTagSet = false
		branchkit.Logf(pluginID, "[TAGS] find tag CLEARED")
	}
}

// drainFindLocked ends any find voice gating: tag cleared. No-op when nothing
// is held. Must be called under `mu`.
func (h *Host) drainFindLocked(pluginID, reason string) {
	if h.state.FindConnID == "" && !h.state.FindActive && !h.state.FindTagSet {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] find drain reason=%s conn=%q", reason, h.state.FindConnID)
	h.state.FindConnID = ""
	h.state.FindActive = false
	h.syncFindLocked(pluginID)
}
