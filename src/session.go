package main

import (
	"net/http"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// PUT /session: a connection's whole mode state, as levels.
//
// It replaced six edge-triggered routes (/caret, /find, /video-mode,
// /auto-scroll, /query-field, /media-active), one per mode, each a POST of
// {active}. Edges have to arrive, in order, exactly once: a dropped exit
// stranded an exclusive tag, a reconnect had to replay each mode by hand, and
// a mode entered while another browser was in front was refused and never
// asked again. A level is idempotent and complete: the latest request is the
// truth, a reconnect replays one request, and the plugin keeps it
// (Conn.Session) so a mode refused for focus is applied when focus arrives.
//
// The tags themselves stay single-slot (caret.go, find.go, video.go,
// autoscroll.go, query_field.go): each is global, and its slot names the
// connection it was taken for. What changed is who decides: the slots are
// derived from the connections' sessions here.

// sessionSlot is one single-slot mode as the session sees it.
type sessionSlot struct {
	name string
	// What the session asks for.
	want func(s wire.SessionRequest) bool
	// The connection holding the slot ("" when none).
	owner func() string
	// Take the slot for conn and sync the tag.
	take func(conn string)
	// Release the slot and sync the tag (the slot's drain).
	drain func(reason string)
	// Taking it needs a KNOWN focused connection. The others are modes the
	// user deliberately entered, so taking one with focus unknown — the
	// plugin has not yet heard which app is in front at all — is a
	// single-browser convenience. "No focused connection" is not unknown
	// once an app is known to be in front: then another app has the user,
	// and no browser may take a slot (applySessionLocked).
	// The query field is reported by every content script on load, so a
	// BACKGROUND tab with an autofocused input would claim it — and a gate
	// claimed by an unfocused browser shapes dictation aimed somewhere else
	// entirely. Observed within minutes of shipping the fail-open version: a
	// background Chrome held it while the terminal was frontmost, so prose
	// dictated into the terminal was stamped `query`.
	needsKnownFocus bool
}

// sessionSlots lists the modes a session carries. Must hold mu. Built per
// call so the closures read the current state.
func (h *Host) sessionSlots(pluginID string) []sessionSlot {
	st := h.state
	return []sessionSlot{
		{
			name:  "caret",
			want:  func(s wire.SessionRequest) bool { return s.Modes.Caret },
			owner: func() string { return st.CaretConnID },
			take: func(conn string) {
				st.CaretConnID, st.CaretActive = conn, true
				h.syncCaretLocked(pluginID)
			},
			drain: func(reason string) { h.drainCaretLocked(pluginID, reason) },
		},
		{
			name:  "find",
			want:  func(s wire.SessionRequest) bool { return s.Modes.Find },
			owner: func() string { return st.FindConnID },
			take: func(conn string) {
				st.FindConnID, st.FindActive = conn, true
				h.syncFindLocked(pluginID)
			},
			drain: func(reason string) { h.drainFindLocked(pluginID, reason) },
		},
		{
			name:  "video",
			want:  func(s wire.SessionRequest) bool { return s.Modes.Video },
			owner: func() string { return st.VideoModeConnID },
			take: func(conn string) {
				st.VideoModeConnID, st.VideoModeActive = conn, true
				h.syncVideoModeLocked(pluginID)
			},
			drain: func(reason string) { h.drainVideoModeLocked(pluginID, reason) },
		},
		{
			name:  "auto_scroll",
			want:  func(s wire.SessionRequest) bool { return s.Modes.AutoScroll },
			owner: func() string { return st.AutoScrollConnID },
			take: func(conn string) {
				st.AutoScrollConnID, st.AutoScrollActive = conn, true
				h.syncAutoScrollLocked(pluginID)
			},
			drain: func(reason string) { h.drainAutoScrollLocked(pluginID, reason) },
		},
		{
			name:  "query_field",
			want:  func(s wire.SessionRequest) bool { return s.QueryField },
			owner: func() string { return st.QueryFieldConnID },
			take: func(conn string) {
				st.QueryFieldConnID, st.QueryFieldActive = conn, true
				h.syncQueryFieldLocked(pluginID)
			},
			drain:           func(reason string) { h.drainQueryFieldLocked(pluginID, reason) },
			needsKnownFocus: true,
		},
	}
}

// applySessionLocked derives the slots from connID's stored session. A mode
// it does not want is released when connID holds it, or when nobody does — an
// unowned slot still believed held is a failed Delete waiting for a retry
// (caret.go), and any connection's "off" is a fine moment to retry it.
// Another connection's slot is never released by this one's "off": its
// session says nothing about that browser. A mode it wants is taken when
// connID may take it (the focused connection, or focus unknown where the slot
// allows — never while another app is known to be in front). Releases run
// before takes, so a switch between exclusive modes
// never has the platform holding two exclusive tags at once. Then the media
// tag, the OR over every connection's session. Must hold mu.
func (h *Host) applySessionLocked(pluginID, connID, reason string) {
	s := h.state.peek(connID).Session
	focused := h.state.FocusedConnID
	// Focus is unknown only while no app has been reported in front.
	// FocusedConnID is also "" while a non-browser app is frontmost, and a
	// browser's session re-sent from the background then (its window blur
	// re-sends it) must not re-take the exclusive modes the unfocus drained:
	// they would suppress every command of the app the user is in. The
	// request is kept either way, and focus arriving applies it.
	focusUnknown := focused == "" && h.state.FocusedBundleID == ""
	slots := h.sessionSlots(pluginID)
	for _, slot := range slots {
		if owner := slot.owner(); !slot.want(s) && (owner == connID || owner == "") {
			slot.drain(reason)
		}
	}
	for _, slot := range slots {
		if !slot.want(s) {
			continue
		}
		mayTake := focused == connID || (focusUnknown && !slot.needsKnownFocus)
		if !mayTake {
			// A silent refusal is the failure class that cost a night of log
			// archaeology (2026-07-26): the tag just never appeared. The
			// request is kept; focus arriving applies it.
			if slot.owner() != connID {
				branchkit.Logf(pluginID, "[TAGS] %s set deferred conn=%q focused=%q (applied when it is focused)",
					slot.name, connID, focused)
			}
			continue
		}
		slot.take(connID)
	}
	// The user is in this browser and it has media: it is the one they last
	// touched, for a media verb spoken once they have moved to another app.
	// Runs for its PUTs while focused and on focus reaching it.
	if s.MediaActive && focused == connID {
		h.state.MediaLastConnID = connID
	}
	h.syncMediaActiveLocked(pluginID)
}

// releaseConnSlotsLocked releases every slot connID holds: its connection is
// gone, and a dead publisher can never send its "off". Must hold mu.
func (h *Host) releaseConnSlotsLocked(pluginID, connID, reason string) {
	for _, slot := range h.sessionSlots(pluginID) {
		if slot.owner() == connID {
			slot.drain(reason)
		}
	}
}

// handleSessionPut stores a connection's mode state and derives the tags.
func (h *Host) handleSessionPut(_ *http.Request, req *wire.SessionRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	// Only a connection with a live stream has a session. A Conn is dropped
	// in one place, its stream's disconnect (sse.go), so one created here
	// for a conn_id with no stream — a request racing ahead of its stream, a
	// stale worker's id, any caller at all — would never be dropped: it
	// would hold media_active (the OR over sessions) and the slots it took
	// for as long as the plugin runs. Refused, the extension forgets the
	// state as unacknowledged and its connect replay sends it again once the
	// stream is up (session-state.ts).
	if h.state.clientOf(req.ConnID) == "" {
		h.mu.Unlock()
		branchkit.Logf(pluginID, "[TAGS] session refused conn=%q: no live stream", req.ConnID)
		return wire.Ack{}, statusError(http.StatusConflict, "no live stream for conn_id")
	}
	c := h.state.conn(req.ConnID)
	prev := c.Session
	c.Session = *req
	if req.MediaActive && !prev.MediaActive {
		// The "what you hear" pick (mediaTargetConnsLocked): media just
		// started in this browser. A PUT that only repeats media_active —
		// a connect replay, a mode change, the blur re-send — is no news
		// about media and must not move the pick: a background browser's
		// stream reconnecting used to steal it from the one the user had
		// just been watching. The other way the pick moves is the user
		// being in a browser with media (applySessionLocked).
		h.state.MediaLastConnID = req.ConnID
	}
	// browser.caret_updated is emitted where the tag actually moves
	// (syncCaretLocked), here or on a later focus gain.
	h.applySessionLocked(pluginID, req.ConnID, "extension_session")
	h.mu.Unlock()
	return ack()
}
