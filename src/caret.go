package main

import (
	"errors"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// Caret/visual selection voice mode. The extension's caret mode (a content-side
// DOM-selection state, entered by keyboard `v` or voice "select <hint>") holds
// the EXCLUSIVE `plugin.<id>.caret` tag while active, so the contributed voice
// selection commands ("select word", "copy that", …) are eligible ONLY while
// selecting — and, being exclusive, they're the only things that match, mirroring
// the keyboard's modal capture.
//
// This is the palette tag's shape (palette.go), minus the row collection: the
// tag is a pure mirror of the extension's mode — held while the connection's
// session (PUT /session, session.go) says `modes.caret`, cleared when it stops
// saying so or on any drain path. Because it's
// content-driven and never command-cleared (no clear_on_unrelated_command), the
// tag can't drift from the extension's mode. The drain paths are the safety
// story — a stuck exclusive tag would suppress every command system-wide:
//   - the session's caret going false (the extension's exit: yank / Escape / blur)
//   - focused-source change / browser unfocused (focus.go)
//   - the publisher's SSE disconnect (sse.go)

func caretTagName(pluginID string) string {
	return "plugin." + pluginID + ".caret"
}

// Test seams, same pattern as palette's putPaletteTag/deletePaletteTag.
func (h *Host) putCaretTagDefault(pluginID string) error {
	return h.plugin.Put(caretTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteCaretTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(caretTagName(pluginID), "singleton")
	return err
}

// syncCaretLocked aligns the exclusive tag with state.CaretActive, issuing
// Put/Delete only on transitions. Must be called under `mu`.
//
// Ordering rule: the RPC goes first and the local bookkeeping flag only
// follows a SUCCESS, on both edges. The invariant being protected is not
// statement order for its own sake — it is that a clear signal always survives
// until the platform has actually taken the tag. CaretTagSet is not a note
// about what we did; it is the sole record that the platform still holds the
// tag, and it is what keeps the retry paths live: drainCaretLocked
// short-circuits when conn/active/tagSet are all clear, and this function's
// clear branch is guarded on it. Clear it ahead of a Delete that then fails and
// the platform keeps an EXCLUSIVE tag the plugin believes is gone, with every
// later drain a no-op — nothing ever retries and every voice command in the
// system stays suppressed until a restart. Leaving the flag set on failure
// risks only a redundant Delete later, which is idempotent platform-side: the
// error can only cost us a tag we already meant to lose.
//
// hintGate.transitionToIdle reaches the same invariant from the other side: it
// sets its state field first but has NO early-out, so every subsequent clear
// signal re-issues the Delete regardless. These mirrors have no such second
// chance — the drain guards are the early-out — so here the order is what
// carries it.
//
// A write that TIMED OUT is neither outcome: the platform may have carried it
// out and only the answer is missing. The flag alone could not say that, and
// a PUT that landed but timed out left CaretTagSet false over a tag the
// platform held, so the exit's clear branch never sent the Delete and the
// exclusive tag stranded. A timeout now marks CaretTagUncertain, and while it
// is set every sync issues the write the current state wants, whichever
// edge. Both writes are idempotent and silent when nothing changes. A
// definite refusal keeps its meaning: nothing was written.
func (h *Host) syncCaretLocked(pluginID string) {
	uncertain := h.state.CaretTagUncertain
	if h.state.CaretActive && (!h.state.CaretTagSet || uncertain) {
		if err := h.putCaretTag(pluginID); err != nil {
			h.state.CaretTagUncertain = uncertain || isCallTimeout(err)
			branchkit.Logf(pluginID, "caret tag PUT failed (uncertain=%v): %v", h.state.CaretTagUncertain, err)
			return
		}
		h.state.CaretTagSet = true
		h.state.CaretTagUncertain = false
		h.state.CaretTagConnID = h.state.CaretConnID
		branchkit.Logf(pluginID, "[TAGS] caret tag SET conn=%q", h.state.CaretConnID)
		h.emitCaretUpdatedLocked(h.state.CaretConnID, true)
	} else if !h.state.CaretActive && (h.state.CaretTagSet || uncertain) {
		if err := h.deleteCaretTag(pluginID); err != nil {
			h.state.CaretTagUncertain = uncertain || isCallTimeout(err)
			branchkit.Logf(pluginID, "caret tag DELETE failed (still believed SET — next drain retries): %v", err)
			return
		}
		h.state.CaretTagSet = false
		h.state.CaretTagUncertain = false
		branchkit.Logf(pluginID, "[TAGS] caret tag CLEARED")
		h.emitCaretUpdatedLocked(h.state.CaretTagConnID, false)
		h.state.CaretTagConnID = ""
	}
}

// emitCaretUpdatedLocked announces that the caret tag was just taken or
// released, from the one place either happens: a session asking for it, focus
// reaching a connection whose session was deferred (focus.go), every drain
// (unfocus, focus switch, disconnect). It used to be emitted by the session
// route alone, and for the request rather than the tag, so a caret taken on
// focus gain or drained by focus was never announced, and a deferred request
// was announced as a change that had not happened. A notification, not a
// call, so it is fine under mu, as the tag write itself is. Must hold mu.
func (h *Host) emitCaretUpdatedLocked(connID string, active bool) {
	h.emitEvent("browser.caret_updated", map[string]any{
		"conn_id": connID,
		"active":  active,
	})
}

// isCallTimeout reports whether err is a call that got no answer in time, so
// may have been carried out.
func isCallTimeout(err error) bool {
	var timeout *branchkit.CallTimeoutError
	return errors.As(err, &timeout)
}

// modeMirror is one exclusive-mode tag whose external clears this plugin
// forwards to the extension as an exit imperative. The set is a table rather
// than hand-written Subscribe calls so that "which modes does the global
// escape reach" is one list to read — the Nth-participant miss is the whole
// bug class here (the mode stack eventually DERIVES this list instead of
// declaring it).
type modeMirror struct {
	// Short name for logs.
	label string
	// Tag collection to subscribe to.
	collection string
	// Extension action dispatched on an external clear.
	action string
	// wasCleared runs under `mu` after a write by someone other than this
	// plugin and answers "did the mode we were mirroring just end?" — the
	// event payload carries only (collection, writer), never the operation,
	// so the mirror has to answer from what it knows. Both members answer from
	// their own bookkeeping flag, which is sound only because they are
	// plugin-written; see modeMirrors on why a matcher-written tag can't join
	// without a different kind of predicate.
	wasCleared func() bool
}

// modeMirrors lists the tags this plugin both WRITES and mirrors outward.
//
// Video joined at Wave 3 C4b, and the CONDITION its old absence-note demanded
// was met first, not discriminated around: the sticky-vs-hold-scoped lifetime
// mismatch is gone because the tag became a pure mirror of the extension's
// ONE video lifetime (`w` and the spoken "video" drive the same mode-stack
// entry; the matcher-written sets_tags and the hold-scoped clear_on_event
// lifecycle are deleted from the manifest). With one lifetime, a forwarded
// exit imperative can only ever end a mode the user is actually in — the
// hold-boundary teardown hazard the absence guarded against is
// unrepresentable.
func (h *Host) modeMirrors(pluginID string) []modeMirror {
	return []modeMirror{
		// All are plugin-written: WE hold the tag, so our own bookkeeping flag
		// is the answer — a foreign write to a tag we believe we hold can only
		// be a clear, because nobody else sets these.
		{label: "caret", collection: caretTagName(pluginID), action: "select_exit",
			wasCleared: func() bool { return h.state.CaretTagSet }},
		// The palette's tag is PLATFORM-held since the `@palette` surface
		// migration, in the shared `_surface` namespace. A foreign clear
		// while rows are held (escape's "over", a simulate unwind) is the
		// dismiss signal; the rows-drain close path zeroes local state
		// first, so it answers false here.
		{label: "palette", collection: "_surface", action: "palette_dismiss",
			wasCleared: func() bool { return h.paletteSurfaceClearedLocked(pluginID) }},
		{label: "video", collection: videoModeTagName(pluginID), action: "video_exit",
			wasCleared: func() bool { return h.state.VideoModeTagSet }},
		// Keep scrolling (autoscroll.go): "over" clearing the tag must stop
		// the page, or it keeps moving with its "stop" no longer hearable.
		{label: "auto_scroll", collection: autoScrollTagName(pluginID), action: "auto_scroll_stop",
			wasCleared: func() bool { return h.state.AutoScrollTagSet }},
	}
}

// reconcileExternalTagClears subscribes to this plugin's mode-mirror tags and,
// when one is cleared by a writer other than this plugin, forwards the exit
// imperative to the extension. The one external writer today is the platform's
// global escape: the voice plugin's mode-global "over" deletes every non-exempt
// exclusive gate directly, and the tags are one-way mirrors (extension →
// plugin → platform) — so without this, the page stays visibly in the mode
// while its voice gating is gone, and even "stop selecting" is dead (its gate
// just vanished): the state-lying wedge. The forwarded action drives the
// extension's NORMAL exit path (caret exit → a session with caret false;
// palette close → drain), so the mirror reconciles through the same one-way
// flow as a user-initiated exit — the redundant Delete it triggers is
// idempotent platform-side.
func (h *Host) reconcileExternalTagClears(pluginID string) {
	for _, m := range h.modeMirrors(pluginID) {
		h.plugin.Subscribe(m.collection, h.forwardModeExit(pluginID, m))
	}
}

func (h *Host) forwardModeExit(pluginID string, m modeMirror) branchkit.CollectionChangedHandler {
	return func(evt branchkit.CollectionUpdatedEventParams) {
		if evt.Writer == pluginID {
			return
		}
		h.mu.Lock()
		cleared := m.wasCleared()
		if cleared {
			h.sendToFocusedClientsLocked(wire.NewAction(m.action, wire.ModeExitParams{Action: m.action}))
		}
		h.mu.Unlock()
		if cleared {
			branchkit.Logf(pluginID, "[TAGS] %s tag cleared externally (writer=%q) — forwarding %s", m.label, evt.Writer, m.action)
		}
	}
}

// drainCaretLocked ends any caret voice session: tag cleared. No-op when nothing
// is held. Must be called under `mu`.
func (h *Host) drainCaretLocked(pluginID, reason string) {
	if h.state.CaretConnID == "" && !h.state.CaretActive && !h.state.CaretTagSet && !h.state.CaretTagUncertain {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] caret drain reason=%s conn=%q", reason, h.state.CaretConnID)
	h.state.CaretConnID = ""
	h.state.CaretActive = false
	h.syncCaretLocked(pluginID)
}
