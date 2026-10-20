package main

import (
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// The tabs projection's RPC half rides collection.replace; tests swap the
// replaceTabsCollection seam and assert on the record sets it receives,
// mirroring how batch_test.go stubs syncPerPrefix.

func newTabsTestState(h *Host) {
	h.state = &PluginState{
		PluginID:          "browser",
		LastPushedTabKeys: make(map[string]bool),
	}
}

func captureTabsReplace(h *Host, t *testing.T) *[][]branchkit.CollectionPutEntry {
	t.Helper()
	var calls [][]branchkit.CollectionPutEntry
	orig := h.replaceTabsCollection
	h.replaceTabsCollection = func(records []branchkit.CollectionPutEntry) error {
		calls = append(calls, records)
		return nil
	}
	t.Cleanup(func() { h.replaceTabsCollection = orig })
	return &calls
}

func recordIDs(records []branchkit.CollectionPutEntry) map[string]bool {
	ids := make(map[string]bool, len(records))
	for _, r := range records {
		ids[r.ID] = true
	}
	return ids
}

func TestTabsProjectionPrefersFocusedConn(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	h.state.FocusedConnID = "conn-a"
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	h.state.setTabs("conn-b", []TabEntry{{Spoken: "gmail", TabID: "9"}})

	conn, ok := h.tabsProjectionConnLocked()
	if !ok || conn != "conn-a" {
		t.Fatalf("projection = %q/%v, want conn-a/true", conn, ok)
	}
}

func TestTabsProjectionFocusedConnWithoutEntriesStillWins(t *testing.T) {
	h := newTestHost()
	// A just-focused browser that hasn't published yet must still be the
	// source — projecting another browser's entries would dispatch tab ids
	// from the wrong browser's numbering.
	newTabsTestState(h)
	h.state.FocusedConnID = "conn-new"
	h.state.setTabs("conn-old", []TabEntry{{Spoken: "github", TabID: "1"}})

	conn, ok := h.tabsProjectionConnLocked()
	if !ok || conn != "conn-new" {
		t.Fatalf("projection = %q/%v, want conn-new/true", conn, ok)
	}
}

func TestTabsProjectionLoneFallbackAndDrain(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	if conn, ok := h.tabsProjectionConnLocked(); !ok || conn != "conn-a" {
		t.Fatalf("lone fallback = %q/%v, want conn-a/true", conn, ok)
	}

	delete(h.state.Conns, "conn-a")
	if conn, ok := h.tabsProjectionConnLocked(); !ok || conn != "" {
		t.Fatalf("drain = %q/%v, want \"\"/true", conn, ok)
	}
}

func TestTabsProjectionUndecidableWithMultipleUnfocused(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	h.state.setTabs("conn-b", []TabEntry{{Spoken: "gmail", TabID: "9"}})
	if _, ok := h.tabsProjectionConnLocked(); ok {
		t.Fatal("two publishers with no focus should be undecidable (keep current state)")
	}
}

func TestSyncTabsReportsNewWordsOnlyOnKeySetGrowth(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	calls := captureTabsReplace(h, t)
	h.state.FocusedConnID = "conn-a"
	h.state.setTabs("conn-a", []TabEntry{
		{Spoken: "github", TabID: "1"},
		{Spoken: "gmail", TabID: "2"},
	})

	if !h.syncTabsCollectionLocked("browser") {
		t.Fatal("first sync should report new words")
	}
	if got := recordIDs((*calls)[0]); !got["github"] || !got["gmail"] || len(got) != 2 {
		t.Fatalf("first sync records = %v", got)
	}

	// Payload-only change (tab_id reassignment on MRU flip): same key set,
	// no vocabulary commit needed.
	h.state.setTabs("conn-a", []TabEntry{
		{Spoken: "github", TabID: "3"},
		{Spoken: "gmail", TabID: "2"},
	})
	if h.syncTabsCollectionLocked("browser") {
		t.Fatal("payload-only change must not report new words")
	}

	// Key removal only: still no new words.
	h.state.setTabs("conn-a", []TabEntry{
		{Spoken: "github", TabID: "3"},
	})
	if h.syncTabsCollectionLocked("browser") {
		t.Fatal("key removal must not report new words")
	}

	// A genuinely new key does.
	h.state.setTabs("conn-a", []TabEntry{
		{Spoken: "github", TabID: "3"},
		{Spoken: "zebra", TabID: "4"},
	})
	if !h.syncTabsCollectionLocked("browser") {
		t.Fatal("new key should report new words")
	}
}

func TestSyncTabsSkipsMalformedEntries(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	calls := captureTabsReplace(h, t)
	h.state.FocusedConnID = "conn-a"
	h.state.setTabs("conn-a", []TabEntry{
		{Spoken: "", TabID: "1"},
		{Spoken: "github", TabID: ""},
		{Spoken: "gmail", TabID: "2"},
	})
	h.syncTabsCollectionLocked("browser")
	if got := recordIDs((*calls)[0]); len(got) != 1 || !got["gmail"] {
		t.Fatalf("records = %v, want only gmail", got)
	}
}

func TestSyncTabsUndecidableLeavesCollectionUntouched(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	calls := captureTabsReplace(h, t)
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	h.state.setTabs("conn-b", []TabEntry{{Spoken: "gmail", TabID: "9"}})
	if h.syncTabsCollectionLocked("browser") {
		t.Fatal("undecidable sync must not report new words")
	}
	if len(*calls) != 0 {
		t.Fatalf("undecidable sync must not touch the collection; got %d calls", len(*calls))
	}
}

func TestDroppedConnTabsFallBackToSurvivor(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	calls := captureTabsReplace(h, t)
	h.state.FocusedConnID = "conn-a"
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	h.state.setTabs("conn-b", []TabEntry{{Spoken: "gmail", TabID: "9"}})
	h.syncTabsCollectionLocked("browser")

	// conn-a disconnects; the SSE defer releases the focused slot first.
	h.state.FocusedConnID = ""
	// What the disconnect path does with the dropped connection.
	delete(h.state.Conns, "conn-a")
	h.syncTabsCollectionLocked("browser")

	last := (*calls)[len(*calls)-1]
	if got := recordIDs(last); len(got) != 1 || !got["gmail"] {
		t.Fatalf("post-drop projection = %v, want only gmail (lone survivor)", got)
	}
	if _, still := h.state.Conns["conn-a"]; still {
		t.Fatal("dropped conn's entries should be removed")
	}
}

func TestDropLastConnDrainsProjection(t *testing.T) {
	h := newTestHost()
	newTabsTestState(h)
	calls := captureTabsReplace(h, t)
	h.state.FocusedConnID = "conn-a"
	h.state.setTabs("conn-a", []TabEntry{{Spoken: "github", TabID: "1"}})
	h.syncTabsCollectionLocked("browser")

	h.state.FocusedConnID = ""
	// What the disconnect path does with the dropped connection.
	delete(h.state.Conns, "conn-a")
	h.syncTabsCollectionLocked("browser")

	last := (*calls)[len(*calls)-1]
	if len(last) != 0 {
		t.Fatalf("last-conn drop should drain the projection; got %d records", len(last))
	}
}
