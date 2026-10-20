package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// Command-palette voice session — since the `@palette` selection-surface
// migration, this file is a thin
// relay. The extension publishes ROWS (not assignments) over POST /palette;
// they land in `browser_palette_rows`, whose contents ARE the surface's
// lifecycle: the PLATFORM holds the exclusive `_surface.browser.palette`
// tag while rows exist, assigns pointing-deck codewords (honoring the tabs
// scope's mark pins), projects the matchable collections — including the
// pair tier's prefix/suffix step boundary that used to live here as ~300
// lines of hand-rolled machinery — and writes assignments back through
// `_select_browser_s_palette_assign`, which this plugin forwards to the
// extension so it can render its chips.
//
// What this plugin still owns is exactly what only it can know:
//   - relaying rows/reserves from the focused connection (a background
//     browser must not flip the system into an exclusive mode), and
//   - the drain triggers: empty POST (every extension close path), focus
//     switch, browser unfocus, and the publisher's SSE disconnect. The
//     rows collection is held_by_writer, so a plugin crash drains it too —
//     the exclusive tag can never outlive its palette.

const (
	// The surface's rows + reserves collections (manifest-declared) and the
	// platform's minted names for the `@palette` surface.
	paletteRowsCollection     = "plugin.browser.palette_rows"
	paletteReservesCollection = "plugin.browser.palette_reserves"
)

// paletteAssignCollection is the platform's assignment write-back for the
// `@palette` surface (`_select_<plugin>_s_<name>_assign`).
func paletteAssignCollection(pluginID string) string {
	return "_select_" + pluginID + "_s_palette_assign"
}

// paletteSurfaceTag is the platform-held exclusive tag for the surface.
func paletteSurfaceTag(pluginID string) string {
	return "_surface." + pluginID + ".palette"
}

// PaletteEntry is one row from the extension (wire.PaletteEntry). `Spoken`
// present = a PINNED codeword (the tabs scope pins rows to their strip marks'
// spoken words; the full palette omits it and the platform assigns).
type PaletteEntry = wire.PaletteEntry

var paletteRowRoles = map[string]branchkit.FieldDisplay{
	"title":    branchkit.FieldDisplayPrimary,
	"group":    branchkit.FieldDisplayPayload,
	"row_id":   branchkit.FieldDisplayPayload,
	"rank":     branchkit.FieldDisplayPayload,
	"codeword": branchkit.FieldDisplayPayload,
}

// Test seams, same pattern as replaceTabsCollection.
func (h *Host) replacePaletteRowsDefault(records []branchkit.CollectionPutEntry) error {
	_, err := h.plugin.Replace(paletteRowsCollection, records, branchkit.ScopeCollection(),
		branchkit.WithReplaceRoles(paletteRowRoles))
	return err
}
func (h *Host) replacePaletteReservesDefault(records []branchkit.CollectionPutEntry) error {
	_, err := h.plugin.Replace(paletteReservesCollection, records, branchkit.ScopeCollection())
	return err
}

// paletteRowRecords maps the wire entries to rows records. Rank is the
// publish order — the platform assigns top-to-bottom so badges align with
// the rendered list. Pure for tests.
func paletteRowRecords(entries []PaletteEntry) ([]branchkit.CollectionPutEntry, error) {
	out := make([]branchkit.CollectionPutEntry, 0, len(entries))
	for i, e := range entries {
		if e.RowID == "" {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"title":    e.Title,
			"group":    e.Group,
			"row_id":   e.RowID,
			"rank":     i,
			"codeword": e.Spoken,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal palette row %q: %w", e.RowID, err)
		}
		out = append(out, branchkit.CollectionPutEntry{ID: e.RowID, Payload: payload})
	}
	return out, nil
}

func reserveRecords(letters []string) []branchkit.CollectionPutEntry {
	out := make([]branchkit.CollectionPutEntry, 0, len(letters))
	seen := map[string]bool{}
	for _, l := range letters {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, branchkit.CollectionPutEntry{ID: l, Payload: json.RawMessage(`{}`)})
	}
	return out
}

// syncPaletteLocked relays the held entries + reserves into the surface's
// collections. Reserves FIRST: the platform assigns on the rows write, so
// the reserve set must already be current. Must be called under `mu`.
func (h *Host) syncPaletteLocked(pluginID string) {
	rows, err := paletteRowRecords(h.state.PaletteEntries)
	if err != nil {
		branchkit.Logf(pluginID, "palette sync conn=%q: %v", h.state.PaletteConnID, err)
		return
	}
	if err := h.replacePaletteReserves(reserveRecords(h.state.PaletteReserves)); err != nil {
		branchkit.Logf(pluginID, "palette reserves sync: %v", err)
		return
	}
	if err := h.replacePaletteRows(rows); err != nil {
		branchkit.Logf(pluginID, "palette rows sync conn=%q: %v", h.state.PaletteConnID, err)
		return
	}
	branchkit.Logf(pluginID, "[TAGS] palette rows relayed conn=%q rows=%d reserves=%d (surface lifecycle is the platform's)",
		h.state.PaletteConnID, len(rows), len(h.state.PaletteReserves))
}

// drainPaletteLocked ends any palette session: rows drained (which closes
// the surface — the platform clears the tag and empties the minted
// collections). No-op when nothing is held. Must be called under `mu`.
func (h *Host) drainPaletteLocked(pluginID, reason string) {
	if h.state.PaletteConnID == "" && len(h.state.PaletteEntries) == 0 {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] palette drain reason=%s conn=%q", reason, h.state.PaletteConnID)
	h.state.PaletteConnID = ""
	h.state.PaletteEntries = nil
	h.state.PaletteReserves = nil
	h.syncPaletteLocked(pluginID)
}

// watchPaletteAssignments forwards the platform's codeword assignments to
// the extension: on every write-back, read the assignment set and send it to
// the focused SSE clients as one event. The extension renders its chips
// (and derives typed labels + holder claims) from exactly this.
func (h *Host) watchPaletteAssignments(pluginID string) {
	h.plugin.Subscribe(paletteAssignCollection(pluginID),
		func(_ branchkit.CollectionUpdatedEventParams) {
			records, err := h.plugin.ListAll(paletteAssignCollection(pluginID))
			if err != nil {
				branchkit.Logf(pluginID, "palette assignments read: %v", err)
				return
			}
			assigns := make(map[string]string, len(records))
			for _, rec := range records {
				var p struct {
					Codeword string `json:"codeword"`
				}
				if json.Unmarshal(rec.Payload, &p) == nil && p.Codeword != "" {
					assigns[rec.ID] = p.Codeword
				}
			}
			h.mu.Lock()
			h.sendToFocusedClientsLocked(wire.NewAction("palette_assign", wire.PaletteAssignParams{Assignments: assigns}))
			h.mu.Unlock()
			branchkit.Logf(pluginID, "palette assignments forwarded: %d row(s)", len(assigns))
		})
}

// listPaletteSurfaceGates reads the `_surface` namespace's live gate names
// ("<plugin>.<surface>"). Seam for tests, like the replace seams above.
func (h *Host) listPaletteSurfaceGatesDefault() ([]string, error) {
	records, err := h.plugin.ListAll("_surface")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(records))
	for _, rec := range records {
		var p struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(rec.Payload, &p) == nil && p.Name != "" {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

// paletteSurfaceClearedLocked answers the mode-mirror's question for the
// platform-held surface tag: did someone OTHER than the rows lifecycle end
// the palette mode? True exactly when this plugin still holds rows but the
// `_surface` namespace no longer carries the palette's gate (escape's
// "over", a simulate unwind). Reconverges by draining the rows so the next
// open starts clean. Must be called under `mu`.
func (h *Host) paletteSurfaceClearedLocked(pluginID string) bool {
	if h.state.PaletteConnID == "" && len(h.state.PaletteEntries) == 0 {
		return false
	}
	names, err := h.listPaletteSurfaceGates()
	if err != nil {
		branchkit.Logf(pluginID, "palette surface-tag read: %v", err)
		return false
	}
	want := pluginID + ".palette"
	for _, name := range names {
		if name == want {
			return false
		}
	}
	h.drainPaletteLocked(pluginID, "external_tag_clear")
	return true
}

// handlePalettePost stores/drains the palette session. Empty entries are the
// close signal and are honored from ANY connection (a close must never be
// refused). Non-empty entries are accepted only from the focused connection
// (or with focus unknown — the single-browser fail-open, mirroring tabs):
// a background browser must not be able to flip the system into an exclusive
// mode over the focused one's speech.
func (h *Host) handlePalettePost(_ *http.Request, req *wire.PaletteRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	accepted := false
	if len(req.Entries) == 0 {
		h.drainPaletteLocked(pluginID, "publisher_clear")
	} else if h.state.FocusedConnID == "" || h.state.FocusedConnID == req.ConnID {
		h.state.PaletteConnID = req.ConnID
		h.state.PaletteEntries = req.Entries
		h.state.PaletteReserves = req.Reserves
		h.syncPaletteLocked(pluginID)
		accepted = true
	}
	h.mu.Unlock()

	// Row words are alphabet words already in the engine union (the platform
	// seeds every `_select_*` collection from the deck), but pinned pair
	// entries are new multi-word STRUCTURE — the debounced commit lets the
	// DAG/never-standalone rebuild propagate (a no-op when the desired-
	// vocabulary hash didn't move).
	h.scheduleVocabularyCommit(pluginID)

	h.emitEvent("browser.palette_updated", map[string]any{
		"conn_id":   req.ConnID,
		"entries":   len(req.Entries),
		"projected": accepted,
	})
	return ack()
}
