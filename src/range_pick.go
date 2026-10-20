package main

import (
	"net/http"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// Range-pick chip window (branchkit-extension/src/activate/range-disambiguation.ts).
// While "highlight <phrase>" has a pending disambiguation pick, the chips OWN the
// hint codewords: the extension hides the regular badges and swallows every
// non-chip codeword at dispatch (refusePickWindowCodeword). That narrowing used
// to be invisible to the platform — the chip codewords were published as an
// ordinary additive grammar batch, so `browser_hints_prefix` and the per-prefix
// `_strict` companions still held every element codeword on the page, and the
// Discovery HUD (which is a straight enumeration of those collections — see
// platform completions_inner) advertised second words that were about to be
// refused.
//
// The fix is a filter on the PROJECTION, not a second copy of the truth: the
// frame sessions stay the single source, and while a pick is armed
// buildTabPrefixState emits only the chip codewords for that (conn, tab). The
// prefix list and every suffix menu narrow together, with no republish and no
// extension round-trip.
//
// Deliberately NOT a tag. Unlike caret/palette this is not an exclusive mode —
// ordinary commands ("scroll down", "over") genuinely still work during a pick,
// and only the hint codewords are owned. A tag would both overstate the narrow
// and cost an HLG recompile at each end of a seconds-long window.
//
// Scoping is by codeword membership (see rangePickFilterLocked): a source that
// doesn't hold the chips projects unfiltered, so a tab switch needs no drain.
// The drain paths cover the cases where the extension can't send its own
// release — SSE disconnect, connection cleanup, tab close.

// rangePickFilterLocked returns the armed chip set when the source being
// projected actually holds those chips, or nil to project unfiltered.
//
// Membership-based, NOT (conn, tab)-keyed. The conn id is minted per
// service-worker instance — an extension reload or an idle-SW restart mints a
// new one — and the projection always runs for the FOCUSED source, which can
// differ from, or lag behind, the source that armed. Keying on identity meant a
// pick armed as conn=A/tab=42 never narrowed a projection built for conn=B, and
// silently did nothing (field bug 2026-07-25). Asking "does this source hold the
// chips?" is the same question without the fragile identity hop, and it can
// never narrow a tab holding none of them — which would filter everything out
// and leave the stale projection frozen behind the empty-source guard.
//
// Caller holds mu.
func (h *Host) rangePickFilterLocked(connID string, tabID int) map[string]bool {
	if len(h.state.RangePickCodewords) == 0 {
		return nil
	}
	sessions, held := 0, 0
	var sample []string
	for key, sess := range h.state.FrameSessions {
		if key.ConnID != connID || key.TabID != tabID {
			continue
		}
		sessions++
		held += len(sess.Codewords)
		for cw := range sess.Codewords {
			if h.state.RangePickCodewords[cw] {
				return h.state.RangePickCodewords
			}
			if len(sample) < 5 {
				sample = append(sample, cw)
			}
		}
	}
	// Armed but nothing matched: the narrow silently won't apply, which reads
	// exactly like the feature not working. Say so, with enough to tell "chips
	// went to another source" from "the codewords don't compare equal".
	wanted := make([]string, 0, len(h.state.RangePickCodewords))
	for cw := range h.state.RangePickCodewords {
		wanted = append(wanted, cw)
	}
	branchkit.Logf(h.state.PluginID,
		"[TAGS] range pick filter MISS conn=%q tab=%d sessions=%d source_codewords=%d sample=%q armed=%q",
		connID, tabID, sessions, held, sample, wanted)
	return nil
}

// drainRangePickLocked releases any armed pick and reprojects so the full hint
// set comes back. No-op when nothing is armed. Caller holds mu.
func (h *Host) drainRangePickLocked(pluginID, reason string) {
	if h.state.RangePickConnID == "" && len(h.state.RangePickCodewords) == 0 {
		return
	}
	branchkit.Logf(pluginID, "[TAGS] range pick drain reason=%s conn=%q tab=%d codewords=%d",
		reason, h.state.RangePickConnID, h.state.RangePickTabID, len(h.state.RangePickCodewords))
	h.state.RangePickConnID = ""
	h.state.RangePickTabID = 0
	h.state.RangePickCodewords = nil
	h.reprojectFocusedSourceLocked(pluginID)
}

// handleRangePickPost arms or releases the chip window. An empty codeword list
// is the release; anything else arms.
//
// Unlike palette and caret, an arm is accepted from ANY connection rather than
// only the focused one. Those two set an exclusive tag — a system-wide effect a
// background browser must not reach — whereas this only ever narrows a source
// that holds the chip codewords (rangePickFilterLocked), so a background
// browser's arm can't touch the focused browser's projection. The focus check
// was also actively harmful: focus is unbound across a service-worker restart,
// which is exactly when a reload-then-retry lands.
func (h *Host) handleRangePickPost(_ *http.Request, req *wire.RangePickRequest) (wire.Ack, error) {
	if req.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}

	h.mu.Lock()
	pluginID := h.state.PluginID
	armed := 0
	// Every POST logs its outcome — a silent no-op made "the extension never
	// sent it" indistinguishable from "the plugin turned it down".
	if len(req.Codewords) == 0 {
		branchkit.Logf(pluginID, "[TAGS] range pick RELEASE conn=%q tab=%d (was_armed=%v)",
			req.ConnID, req.TabID, len(h.state.RangePickCodewords) > 0)
		h.drainRangePickLocked(pluginID, "extension_release")
	} else {
		set := make(map[string]bool, len(req.Codewords))
		for _, cw := range req.Codewords {
			set[cw] = true
		}
		h.state.RangePickConnID = req.ConnID
		h.state.RangePickTabID = req.TabID
		h.state.RangePickCodewords = set
		armed = len(set)
		branchkit.Logf(pluginID, "[TAGS] range pick ARMED conn=%q tab=%d codewords=%v",
			req.ConnID, req.TabID, req.Codewords)
		// Fall back to the arming source: focus is routinely unbound right here
		// (a fresh service worker hasn't completed its focus handshake), and
		// without a fallback the arm reprojects nothing at all.
		h.reprojectSourceLocked(pluginID, req.ConnID, req.TabID)
	}
	h.mu.Unlock()

	h.emitEvent("browser.range_pick_updated", map[string]any{
		"conn_id": req.ConnID,
		"tab_id":  req.TabID,
		"armed":   armed,
	})
	return ack()
}
