package main

import (
	"encoding/json"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// Since the `@palette` surface migration
// this file tests a RELAY: entries in → rows + reserves collections out,
// with the platform owning assignment, projection, and the exclusive tag.
// The old projection/tag tests (~400 lines of atomic-vs-pair splitting,
// prefix drains, tag transitions) moved to the platform with the machinery
// they tested — platform select_mode surface_tests.

func newPaletteTestState(h *Host) {
	h.state = &PluginState{PluginID: "browser"}
}

type paletteRelaySeams struct {
	rows     [][]branchkit.CollectionPutEntry
	reserves [][]branchkit.CollectionPutEntry
}

func capturePaletteSeams(h *Host, t *testing.T) *paletteRelaySeams {
	t.Helper()
	s := &paletteRelaySeams{}
	origRows, origReserves := h.replacePaletteRows, h.replacePaletteReserves
	h.replacePaletteRows = func(records []branchkit.CollectionPutEntry) error {
		s.rows = append(s.rows, records)
		return nil
	}
	h.replacePaletteReserves = func(records []branchkit.CollectionPutEntry) error {
		s.reserves = append(s.reserves, records)
		return nil
	}
	t.Cleanup(func() {
		h.replacePaletteRows, h.replacePaletteReserves = origRows, origReserves
	})
	return s
}

func rowPayload(t *testing.T, rec branchkit.CollectionPutEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Payload, &m); err != nil {
		t.Fatalf("unmarshal row %q: %v", rec.ID, err)
	}
	return m
}

// The relay's core mapping: publish order becomes `rank` (the platform
// assigns top-to-bottom so badges align with the rendered list), a present
// `spoken` rides as the `codeword` PIN (the tabs scope's mark convergence),
// and a blank row_id is skipped rather than minted into an unaddressable row.
func TestPaletteRowRecordsCarryRankAndPins(t *testing.T) {
	rows, err := paletteRowRecords([]PaletteEntry{
		{Spoken: "arch bay", Title: "GitHub", Group: "Tabs", RowID: "tab:1"},
		{Title: "Pin Tab", Group: "Commands", RowID: "cmd:pin"},
		{Title: "no id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (blank row_id skipped)", len(rows))
	}
	first := rowPayload(t, rows[0])
	if first["rank"] != float64(0) || first["codeword"] != "arch bay" || first["title"] != "GitHub" {
		t.Errorf("pinned row payload = %v", first)
	}
	if rows[0].ID != "tab:1" {
		t.Errorf("row keyed by row_id, got %q", rows[0].ID)
	}
	second := rowPayload(t, rows[1])
	if second["rank"] != float64(1) || second["codeword"] != "" {
		t.Errorf("unpinned row payload = %v", second)
	}
}

// Sync writes reserves BEFORE rows: the platform assigns on the rows write,
// so the reserve set must already be current when it fires.
func TestPaletteSyncWritesReservesThenRows(t *testing.T) {
	h := newTestHost()
	newPaletteTestState(h)
	s := capturePaletteSeams(h, t)
	order := []string{}
	origRows, origReserves := h.replacePaletteRows, h.replacePaletteReserves
	h.replacePaletteRows = func(records []branchkit.CollectionPutEntry) error {
		order = append(order, "rows")
		return origRows(records)
	}
	h.replacePaletteReserves = func(records []branchkit.CollectionPutEntry) error {
		order = append(order, "reserves")
		return origReserves(records)
	}
	h.state.PaletteConnID = "conn-1"
	h.state.PaletteEntries = []PaletteEntry{{Title: "Row", RowID: "tab:1"}}
	h.state.PaletteReserves = []string{"g", "k", "g"}

	h.syncPaletteLocked("browser")

	if len(order) != 2 || order[0] != "reserves" || order[1] != "rows" {
		t.Fatalf("write order = %v, want [reserves rows]", order)
	}
	if len(s.reserves[0]) != 2 {
		t.Errorf("reserves = %d records, want 2 (deduped)", len(s.reserves[0]))
	}
	if len(s.rows[0]) != 1 {
		t.Errorf("rows = %d records, want 1", len(s.rows[0]))
	}
}

// Drain replaces both collections with nothing — that emptiness IS the
// close signal the platform's lifecycle hook acts on (tag cleared, minted
// collections drained). No tag bookkeeping remains plugin-side.
func TestPaletteDrainEmptiesRowsAndReserves(t *testing.T) {
	h := newTestHost()
	newPaletteTestState(h)
	s := capturePaletteSeams(h, t)
	h.state.PaletteConnID = "conn-1"
	h.state.PaletteEntries = []PaletteEntry{{Title: "Row", RowID: "tab:1"}}
	h.state.PaletteReserves = []string{"g"}

	h.drainPaletteLocked("browser", "test")

	if h.state.PaletteConnID != "" || h.state.PaletteEntries != nil || h.state.PaletteReserves != nil {
		t.Errorf("local state not zeroed: conn=%q entries=%v", h.state.PaletteConnID, h.state.PaletteEntries)
	}
	last := s.rows[len(s.rows)-1]
	if len(last) != 0 {
		t.Errorf("final rows write holds %d records, want 0", len(last))
	}
	lastReserves := s.reserves[len(s.reserves)-1]
	if len(lastReserves) != 0 {
		t.Errorf("final reserves write holds %d records, want 0", len(lastReserves))
	}

	// Idempotent: a second drain with nothing held writes nothing.
	writes := len(s.rows)
	h.drainPaletteLocked("browser", "test-again")
	if len(s.rows) != writes {
		t.Errorf("empty drain wrote rows again")
	}
}

// Context-gated contributed commands require the PLATFORM's surface tag and
// never clear it — the surface's lifecycle is its rows, not a match.
func TestContributedPaletteContextGating(t *testing.T) {
	h := newTestHost()
	h.storeContributedCommands("", []contributedCommand{
		{Action: "palette_dismiss", Pattern: "hide", Category: "Help", Context: "palette"},
	})
	defer h.storeContributedCommands("", nil)

	cmds := h.buildContributedCommands("browser")
	if len(cmds) != 1 {
		t.Fatalf("built %d commands, want 1", len(cmds))
	}
	for _, cmd := range cmds {
		typ, _ := actionMap(cmd)["type"].(string)
		if len(cmd.RequiresTags) != 1 || cmd.RequiresTags[0] != "_surface.browser.palette" {
			t.Errorf("%s RequiresTags = %v, want exactly [_surface.browser.palette]", typ, cmd.RequiresTags)
		}
		if len(cmd.ClearsTags) != 0 {
			t.Errorf("%s ClearsTags = %v, want none (rows-driven lifecycle)", typ, cmd.ClearsTags)
		}
	}
}

// A {palette} contribution builds ONE surface-bound spec; the platform
// desugars it into the atomic + pair variants (with the capture.progress
// step boundary the pair tier exists for).
func TestContributedPaletteSlotBuildsSurfaceCapture(t *testing.T) {
	h := newTestHost()
	h.storeContributedCommands("", []contributedCommand{
		{Action: "palette_select", Pattern: "{palette}", Category: "Palette", Context: "palette"},
	})
	defer h.storeContributedCommands("", nil)

	cmds := h.buildContributedCommands("browser")
	if len(cmds) != 1 {
		t.Fatalf("built %d commands, want 1 (the platform expands variants)", len(cmds))
	}
	cmd := cmds[0]
	if got := patternSlot(cmd, 0); got != "<row:@palette>" {
		t.Errorf("slot 0 = %q, want the surface capture", got)
	}
	if got := actionParams(cmd)["row_id"]; got != "{row.row_id}" {
		t.Errorf("row_id = %v, want {row.row_id} (the record rides the capture)", got)
	}
	if len(cmd.ClearsTags) != 0 {
		t.Errorf("ClearsTags = %v, want none", cmd.ClearsTags)
	}
}

// Literal verbs lead the slot ("blank {palette}").
func TestContributedPaletteSlotKeepsLeadingVerb(t *testing.T) {
	h := newTestHost()
	h.storeContributedCommands("", []contributedCommand{
		{Action: "palette_select_newtab", Pattern: "blank {palette}", Context: "palette"},
	})
	defer h.storeContributedCommands("", nil)

	cmds := h.buildContributedCommands("browser")
	if len(cmds) != 1 {
		t.Fatalf("built %d commands, want 1", len(cmds))
	}
	if got := patternSlot(cmds[0], 0); got != "blank" {
		t.Errorf("slot 0 = %q, want the literal verb", got)
	}
	if got := patternSlot(cmds[0], 1); got != "<row:@palette>" {
		t.Errorf("slot 1 = %q, want the surface capture", got)
	}
}

// Same rule as {hint}: the slot is open, so anything after it can never be
// reached. Dropped whole with a log line rather than half-built.
func TestContributedPaletteSlotMustBeLast(t *testing.T) {
	h := newTestHost()
	h.storeContributedCommands("", []contributedCommand{
		{Action: "palette_select", Pattern: "{palette} now", Context: "palette"},
	})
	defer h.storeContributedCommands("", nil)

	if cmds := h.buildContributedCommands("browser"); len(cmds) != 0 {
		t.Fatalf("built %d commands from a non-tail palette slot, want 0", len(cmds))
	}
}
