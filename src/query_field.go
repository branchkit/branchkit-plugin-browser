package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// Query-field gate. Held while the focused browser reports focus in a
// single-line text input (`query_field` in its PUT /session); the collection
// declares `dictation_profile: query`, so the platform stamps that on any
// transcript spoken while it's up and the voice plugin strips prosody
// punctuation before the text lands.
//
// Same imperative-mirror shape as caret.go, with one deliberate difference:
// this gate is NON-EXCLUSIVE. It shapes dictation and gates nothing, so a
// stale one costs a period on one utterance — where a stale exclusive tag
// would suppress every command system-wide. That asymmetry is why this one
// needs no external-clear reconciliation: nothing else has reason to clear it,
// and the global escape ("over") leaves non-exclusive gates alone.
//
// Drains, matching caret's:
//   - the session's query_field going false (focus left the field)
//   - focused-source change / browser unfocused (focus.go)
//   - the publisher's SSE disconnect (sse.go)

func queryFieldTagName(pluginID string) string {
	return "plugin." + pluginID + ".query_field"
}

// Test seams, same pattern as caret's putCaretTag/deleteCaretTag.
func (h *Host) putQueryFieldTagDefault(pluginID string) error {
	return h.plugin.Put(queryFieldTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteQueryFieldTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(queryFieldTagName(pluginID), "singleton")
	return err
}

// syncQueryFieldLocked aligns the tag with state.QueryFieldActive, issuing
// Put/Delete only on transitions. Must be called under `mu`.
func (h *Host) syncQueryFieldLocked(pluginID string) {
	if h.state.QueryFieldActive && !h.state.QueryFieldTagSet {
		if err := h.putQueryFieldTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "query_field tag PUT failed: %v", err)
			return
		}
		h.state.QueryFieldTagSet = true
		branchkit.Logf(pluginID, "[TAGS] query_field tag SET conn=%q", h.state.QueryFieldConnID)
	} else if !h.state.QueryFieldActive && h.state.QueryFieldTagSet {
		h.state.QueryFieldTagSet = false
		if err := h.deleteQueryFieldTag(pluginID); err != nil {
			branchkit.Logf(pluginID, "query_field tag DELETE failed: %v", err)
			return
		}
		branchkit.Logf(pluginID, "[TAGS] query_field tag CLEARED")
	}
}

// drainQueryFieldLocked releases the gate. No-op when nothing is held. Must be
// called under `mu`.
func (h *Host) drainQueryFieldLocked(pluginID, reason string) {
	if h.state.QueryFieldConnID == "" && !h.state.QueryFieldActive && !h.state.QueryFieldTagSet {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] query_field drain reason=%s conn=%q", reason, h.state.QueryFieldConnID)
	h.state.QueryFieldConnID = ""
	h.state.QueryFieldActive = false
	h.syncQueryFieldLocked(pluginID)
}
