package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// Keep-scrolling voice mode. The extension's auto-scroll run —
// "keep scrolling" starts it, any key/click/wheel, the page's end or "stop"
// ends it — holds the EXCLUSIVE `plugin.<id>.auto_scroll` tag while the page
// is moving, so the contributed in-mode words ("faster", "slower", "stop") are
// the only things that match. Exclusive is the deliberate choice: two of those
// words are also the media verbs, hearable system-wide whenever any tab plays
// sound (media_active), and the run should be the only meaning they have
// while it lasts. The extension's mode-stack spec carries the full reasoning.
//
// Video's shape exactly (video.go): a pure mirror of the extension's one
// lifetime, carried in the session (`modes.auto_scroll`, session.go) the mode
// stack's SW-derived mirror sends. Set while the session asks for it, cleared
// when it stops asking or on any drain — the drains are the safety story,
// because a stranded exclusive tag would suppress every other command
// system-wide:
//   - the session's auto_scroll going false (the extension's stop, whatever
//     ended it)
//   - browser unfocused / focus moved to another browser (focus.go) — the
//     page also stops itself on blur
//   - the publisher's SSE disconnect (sse.go)
//   - the global "over" clears the tag; caret.go's modeMirrors forwards that
//     to the extension as auto_scroll_stop so the page stops too.

func autoScrollTagName(pluginID string) string {
	return "plugin." + pluginID + ".auto_scroll"
}

// Test seams, same pattern as video's putVideoModeTag/deleteVideoModeTag.
func (h *Host) putAutoScrollTagDefault(pluginID string) error {
	return h.plugin.Put(autoScrollTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteAutoScrollTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(autoScrollTagName(pluginID), "singleton")
	return err
}

// syncAutoScrollLocked aligns the exclusive tag with state.AutoScrollActive.
// RPC first, bookkeeping on success — caret.go's syncCaretLocked carries the
// full reasoning (a clear signal must survive until the platform has taken
// the tag). Must be called under `mu`.
func (h *Host) syncAutoScrollLocked(pluginID string) {
	if h.state.AutoScrollActive && !h.state.AutoScrollTagSet {
		if err := h.putAutoScrollTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "auto_scroll tag PUT failed: %v", err)
			return
		}
		h.state.AutoScrollTagSet = true
		branchkit.Logf(pluginID, "[TAGS] auto_scroll tag SET conn=%q", h.state.AutoScrollConnID)
	} else if !h.state.AutoScrollActive && h.state.AutoScrollTagSet {
		if err := h.deleteAutoScrollTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "auto_scroll tag DELETE failed (still believed SET — next drain retries): %v", err)
			return
		}
		h.state.AutoScrollTagSet = false
		branchkit.Logf(pluginID, "[TAGS] auto_scroll tag CLEARED")
	}
}

// drainAutoScrollLocked ends any keep-scrolling voice mode: tag cleared. No-op
// when nothing is held. Must be called under `mu`.
func (h *Host) drainAutoScrollLocked(pluginID, reason string) {
	if h.state.AutoScrollConnID == "" && !h.state.AutoScrollActive && !h.state.AutoScrollTagSet {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] auto_scroll drain reason=%s conn=%q", reason, h.state.AutoScrollConnID)
	h.state.AutoScrollConnID = ""
	h.state.AutoScrollActive = false
	h.syncAutoScrollLocked(pluginID)
}
