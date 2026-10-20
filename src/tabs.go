package main

import (
	"encoding/json"
	"net/http"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// Open-tab voice collection. The extension publishes distinctive spoken
// words for its open tabs over POST /tabs; this plugin projects the focused
// browser's set into the manifest-declared `browser_tabs` collection
// (feeds_matching: as_named_entities, key=spoken, value=tab_id). The
// contributed "switch to {browser_tabs}" command captures from it, and the
// matched tab_id rides the action params back to the extension over SSE.
//
// Scoping mirrors the hint grammar's focused-source projection,
// one level coarser: entries are stored per connection and only the focused
// connection's set is projected. This matters beyond clobber-avoidance —
// tab ids are per-browser numberings, so a background browser's entry would
// dispatch a wrong-tab activation in the focused one.

// tabsCollectionName is the manifest-declared collection the projection
// writes. The extension references the same name in its contributed
// "switch to {browser_tabs}" pattern.
const tabsCollectionName = "plugin.browser.tabs"

// TabEntry is one spoken-word record from the extension (wire.TabEntry).
type TabEntry = wire.TabEntry

// replaceTabsCollection applies the projected record set via collection.replace
// — the platform computes the complement over this plugin's own records and
// skips byte-identical ones. Var for the test seam, same pattern as
// syncPerPrefix.
func (h *Host) replaceTabsCollectionDefault(records []branchkit.CollectionPutEntry) error {
	_, err := h.plugin.Replace(tabsCollectionName, records, branchkit.ScopeCollection())
	return err
}

// tabsProjectionConnLocked returns the connection whose entries should
// populate browser_tabs, and whether the projection is decidable:
//   - the focused connection, always, when one is bound — even if it has
//     published nothing yet (a browser switch must DRAIN the previous
//     browser's entries: their tab_ids belong to another browser's numbering
//     and would dispatch wrong-tab activations);
//   - with no focused conn: the lone publisher (single-browser fail-open,
//     mirroring the grammar batch path) — but only while it could be the
//     browser in front: focus unknown, or its identity unresolved. A lone
//     publisher whose asserted bundle is NOT the frontmost app is a
//     background browser, and projecting it put its tab words up while the
//     frontmost browser's extension was gone (a drain instead);
//   - a drain when none remain;
//   - undecidable (keep current state) when multiple publishers exist and
//     focus is unknown.
//
// Must be called under `mu`.
func (h *Host) tabsProjectionConnLocked() (string, bool) {
	if h.state.FocusedConnID != "" {
		return h.state.FocusedConnID, true
	}
	lone, publishers := "", 0
	for id, c := range h.state.Conns {
		if c.TabsPublished {
			lone, publishers = id, publishers+1
		}
	}
	switch publishers {
	case 0:
		return "", true
	case 1:
		front := h.state.FocusedBundleID
		if asserted := h.state.peek(lone).AssertedBundle; front != "" && asserted != "" && asserted != front {
			return "", true
		}
		return lone, true
	}
	return "", false
}

// syncTabsCollectionLocked projects the current source's entries into the
// browser_tabs collection. Returns whether the projected spoken-key set
// gained words the previous projection lacked — the caller schedules a
// debounced vocabulary commit in that case (key removals and payload-only
// reassignments don't touch the engine grammar, which is a full-union HWM).
// Must be called under `mu`.
func (h *Host) syncTabsCollectionLocked(pluginID string) (newWords bool) {
	conn, ok := h.tabsProjectionConnLocked()
	if !ok {
		return false
	}
	entries := h.state.peek(conn).TabEntries
	records := make([]branchkit.CollectionPutEntry, 0, len(entries))
	keys := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Spoken == "" || e.TabID == "" {
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			branchkit.Logf(pluginID, "tabs sync conn=%q: marshal %q: %v", conn, e.Spoken, err)
			return false
		}
		records = append(records, branchkit.CollectionPutEntry{ID: e.Spoken, Payload: raw})
		keys[e.Spoken] = true
		if !h.state.LastPushedTabKeys[e.Spoken] {
			newWords = true
		}
	}
	if err := h.replaceTabsCollection(records); err != nil {
		branchkit.Logf(pluginID, "tabs sync conn=%q: %v", conn, err)
		return false
	}
	if len(records) > 0 || len(h.state.LastPushedTabKeys) > 0 {
		branchkit.Logf(pluginID, "[INGEST] tabs projected conn=%q entries=%d new_words=%v",
			conn, len(records), newWords)
	}
	h.state.LastPushedTabKeys = keys
	return newWords
}

// handleTabsPost stores one connection's published tab-word entries and, if
// that connection is the projection source, re-syncs the collection.
// Non-focused publishers are stored only (same store-don't-project posture
// as grammar batches) — their set projects when focus lands on them.
func (h *Host) handleTabsPost(_ *http.Request, req *wire.TabsRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	c := h.state.conn(req.ConnID)
	c.TabEntries, c.TabsPublished = req.Entries, true
	projConn, decidable := h.tabsProjectionConnLocked()
	projected := decidable && projConn == req.ConnID
	needCommit := false
	if projected {
		needCommit = h.syncTabsCollectionLocked(pluginID)
	}
	h.mu.Unlock()

	if needCommit {
		h.scheduleVocabularyCommit(pluginID)
	}

	h.emitEvent("browser.tabs_updated", map[string]any{
		"conn_id":   req.ConnID,
		"entries":   len(req.Entries),
		"projected": projected,
	})
	return ack()
}
