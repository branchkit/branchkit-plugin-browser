package main

import (
	"sort"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
)

// Media-active gating. Each browser connection's background computes
// ONE boolean union — "this browser has controllable media": focused tab has
// a video, OR any tab is audible, OR the resume memory (a recently
// controlled tab still holding its media). The plugin holds the
// NON-exclusive `plugin.<id>.media_active` tag while ANY connection asserts,
// gating the contributed media commands ("pause", "faster", …) and the
// "video" mode-entry word.
//
// Deliberately NOT gated on browser focus, and NOT drained on browser
// unfocus — controlling a background video from an unrelated app is the
// point of this arc. The bound on system-wide exposure is the union itself:
// with nothing playing anywhere, the words aren't hearable.
//
// Per-connection assertion (Conn.Session.MediaActive, PUT /session — not
// caret's single slot): an unfocused browser with playing media must be able
// to assert, and two browsers' reports must not last-writer-clobber each
// other. A connection writes only its own session; the tag mirrors the OR. An
// assertion goes with its connection on SSE disconnect — and the background
// replays its session on its focus/reconnect edges, so a drop can't strand
// the mirror.
//
// The EXCLUSIVE `plugin.<id>.video_mode` tag is caret's shape since Wave 3
// C4b: a pure mirror of the extension's video key layer, carried in the
// session (`modes.video`) the mode stack's SW-derived mirror sends, on the
// layer's ONE lifetime — entered by `w` or
// the spoken "video" (now an action command driving the same entry), exited
// by Escape/q/w/"over". The old matcher-written, hold-scoped tag (sets_tags
// + clear_on_event session_boundary + clear_on_unrelated_command) had a
// DIFFERENT lifetime from the sticky key layer, which is why its clears
// could not be forwarded; with one lifetime the tag joins caret.go's
// modeMirrors and the global "over" reaches the key layer too.

func mediaActiveTagName(pluginID string) string {
	return "plugin." + pluginID + ".media_active"
}

func videoModeTagName(pluginID string) string {
	return "plugin." + pluginID + ".video_mode"
}

// Test seams, same pattern as caret's putCaretTag/deleteCaretTag.
func (h *Host) putVideoModeTagDefault(pluginID string) error {
	return h.plugin.Put(videoModeTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteVideoModeTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(videoModeTagName(pluginID), "singleton")
	return err
}
func (h *Host) putMediaActiveTagDefault(pluginID string) error {
	return h.plugin.Put(mediaActiveTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteMediaActiveTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(mediaActiveTagName(pluginID), "singleton")
	return err
}

func (h *Host) anyMediaActiveLocked() bool {
	return h.mediaActiveConnCountLocked() > 0
}

// mediaActiveConnCountLocked is how many connections' sessions assert media.
func (h *Host) mediaActiveConnCountLocked() int {
	n := 0
	for _, c := range h.state.Conns {
		if c.Session.MediaActive {
			n++
		}
	}
	return n
}

// syncMediaActiveLocked aligns the tag with the OR of all connections'
// assertions, issuing Put/Delete only on transitions. Must be called under
// `mu`.
//
// RPC first, bookkeeping on success — caret.go's syncCaretLocked carries the
// full reasoning. MediaTagSet is the sole record that the platform still holds
// the tag, and the `!want && MediaTagSet` branch here is the ONLY thing that
// re-issues the Delete: clear the flag ahead of a Delete that then fails and
// every later sync becomes a no-op, leaking the tag until the process
// restarts — "pause"/"faster"/"video" stay hearable system-wide (they are not
// focus-gated) with nothing playing anywhere. Retries ride the connections'
// sessions on focus/reconnect edges (PUT /session, session.go).
func (h *Host) syncMediaActiveLocked(pluginID string) {
	want := h.anyMediaActiveLocked()
	if want && !h.state.MediaTagSet {
		if err := h.putMediaActiveTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "media_active tag PUT failed: %v", err)
			return
		}
		h.state.MediaTagSet = true
		branchkit.Logf(pluginID, "[TAGS] media_active tag SET conns=%d", h.mediaActiveConnCountLocked())
	} else if !want && h.state.MediaTagSet {
		if err := h.deleteMediaActiveTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "media_active tag DELETE failed (still believed SET — next sync retries): %v", err)
			return
		}
		h.state.MediaTagSet = false
		branchkit.Logf(pluginID, "[TAGS] media_active tag CLEARED")
	}
}

// isMediaAction reports whether an action is a media verb — the ones gated on
// media_active rather than on the browser being in front.
func isMediaAction(subAction string) bool {
	return strings.HasPrefix(subAction, "media_")
}

// isMediaAllAction reports the "everything" forms ("pause everything"), which
// fan out to every browser with media rather than picking one.
func isMediaAllAction(subAction string) bool {
	return subAction == "media_pause_all" || subAction == "media_mute_all"
}

// mediaTargetConnsLocked picks the connection(s) a media verb drives. The tag
// that makes "pause" hearable is the OR over every connection's media-active
// assertion, so the verb must go where the media is — sending it to the
// focused browser, as every other verb goes, paused nothing when the video was
// playing in a browser behind it. Same priority the extension applies across
// its own tabs (background/media.ts): the focused browser's media when it has
// some — control what you see; else the connection that most recently
// asserted — what you hear; else any asserting connection, lowest id first so
// the pick is stable. The "everything" forms go to every asserting
// connection. Nil when none asserts: the caller falls back to the focused
// browser. Must be called under `mu`.
func (h *Host) mediaTargetConnsLocked(subAction string) []string {
	var live []string
	for conn, c := range h.state.Conns {
		if !c.Session.MediaActive || c.Client == "" {
			continue
		}
		live = append(live, conn)
	}
	if len(live) == 0 {
		return nil
	}
	sort.Strings(live)
	if isMediaAllAction(subAction) {
		return live
	}
	if f := h.state.FocusedConnID; h.state.BrowserFocused && f != "" && h.mediaLiveLocked(f) {
		return []string{f}
	}
	if last := h.state.MediaLastConnID; h.mediaLiveLocked(last) {
		return []string{last}
	}
	return live[:1]
}

// mediaLiveLocked: conn asserts media and has a live stream to send a verb on.
func (h *Host) mediaLiveLocked(conn string) bool {
	c := h.state.peek(conn)
	return conn != "" && c.Session.MediaActive && c.Client != ""
}

// syncVideoModeLocked aligns the exclusive tag with state.VideoModeActive.
// RPC first, bookkeeping on success — caret.go's syncCaretLocked carries the
// full reasoning (a clear signal must survive until the platform has taken
// the tag). Must be called under `mu`.
func (h *Host) syncVideoModeLocked(pluginID string) {
	if h.state.VideoModeActive && !h.state.VideoModeTagSet {
		if err := h.putVideoModeTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "video_mode tag PUT failed: %v", err)
			return
		}
		h.state.VideoModeTagSet = true
		branchkit.Logf(pluginID, "[TAGS] video_mode tag SET conn=%q", h.state.VideoModeConnID)
	} else if !h.state.VideoModeActive && h.state.VideoModeTagSet {
		if err := h.deleteVideoModeTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "video_mode tag DELETE failed (still believed SET — next drain retries): %v", err)
			return
		}
		h.state.VideoModeTagSet = false
		branchkit.Logf(pluginID, "[TAGS] video_mode tag CLEARED")
	}
}

// drainVideoModeLocked ends any video voice mode: tag cleared. No-op when
// nothing is held. Must be called under `mu`.
func (h *Host) drainVideoModeLocked(pluginID, reason string) {
	if h.state.VideoModeConnID == "" && !h.state.VideoModeActive && !h.state.VideoModeTagSet {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] video_mode drain reason=%s conn=%q", reason, h.state.VideoModeConnID)
	h.state.VideoModeConnID = ""
	h.state.VideoModeActive = false
	h.syncVideoModeLocked(pluginID)
}
