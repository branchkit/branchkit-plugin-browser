package main

import "testing"

// newPickTestState builds a tab holding one chip codeword plus two ordinary
// page hints, all sharing the "arch" prefix so a narrow is visible inside one
// prefix bucket rather than just by prefix count.
func newPickTestState(h *Host) {
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
	}
	sess := h.ensureFrameSession(frameKey{ConnID: "conn-a", TabID: 7, FrameID: 0})
	for _, cw := range []string{"arch bake", "arch cane", "arch dune"} {
		sess.Codewords[cw] = map[string]string{"id": "1"}
		sess.StrictCodewords[cw] = true
	}
}

// While a pick is armed, the projection carries ONLY the chip codewords — this
// is what makes the Discovery HUD stop advertising second words the content
// script is about to refuse (refusePickWindowCodeword).
func TestRangePickNarrowsProjectionToChips(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	h.state.RangePickConnID = "conn-a"
	h.state.RangePickTabID = 7
	h.state.RangePickCodewords = map[string]bool{"arch bake": true}

	full, strict := h.buildTabPrefixState("conn-a", 7)
	if _, ok := full["arch"]["bake"]; !ok {
		t.Error("the chip codeword must survive the narrow")
	}
	if _, ok := full["arch"]["cane"]; ok {
		t.Error("a non-chip codeword leaked into the projection during a pick")
	}
	if len(full["arch"]) != 1 {
		t.Errorf("expected exactly the chip in the prefix bucket, got %v", full["arch"])
	}
	// The strict companion drives the HUD's suffix menu — it must narrow too.
	if len(strict["arch"]) != 1 {
		t.Errorf("strict companion should narrow with the full map, got %v", strict["arch"])
	}
}

// Field regression (2026-07-25): the conn id is minted per service-worker
// instance, so an extension reload made the arm carry conn=B while the
// projection still ran for conn=A — and an identity-keyed filter silently did
// nothing. The narrow must follow the CODEWORDS, not the connection that
// announced them.
func TestRangePickNarrowsDespiteStaleArmIdentity(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	// Armed under a connection id that matches no session in the plugin.
	h.state.RangePickConnID = "conn-from-a-restarted-service-worker"
	h.state.RangePickTabID = 999
	h.state.RangePickCodewords = map[string]bool{"arch bake": true}

	full, _ := h.buildTabPrefixState("conn-a", 7)
	if len(full["arch"]) != 1 {
		t.Errorf("narrow must apply wherever the chips live, not where the arm claimed to be; got %v", full["arch"])
	}
	if _, ok := full["arch"]["bake"]; !ok {
		t.Error("the chip codeword must survive")
	}
}

// The narrow follows the chips: a tab holding none of them projects its full
// hint set. This is why a tab switch needs no drain — and it's what keeps a
// membership filter from blanking an unrelated tab.
func TestRangePickNarrowIsScopedToItsSource(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	other := h.ensureFrameSession(frameKey{ConnID: "conn-a", TabID: 8, FrameID: 0})
	other.Codewords["arch echo"] = map[string]string{"id": "2"}
	other.StrictCodewords["arch echo"] = true

	h.state.RangePickConnID = "conn-a"
	h.state.RangePickTabID = 7
	h.state.RangePickCodewords = map[string]bool{"arch bake": true}

	full, _ := h.buildTabPrefixState("conn-a", 8)
	if _, ok := full["arch"]["echo"]; !ok {
		t.Error("a pick in tab 7 must not narrow tab 8's projection")
	}
}

// An unarmed plugin projects everything — the filter must be inert by default,
// not a permanent gate.
func TestRangePickUnarmedProjectsEverything(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	full, _ := h.buildTabPrefixState("conn-a", 7)
	if len(full["arch"]) != 3 {
		t.Errorf("expected all 3 codewords with no pick armed, got %v", full["arch"])
	}
}

// Releasing restores the full set. The extension sends this on every teardown,
// and the plugin's drains (SSE disconnect, tab close) repeat it for the cases
// where the extension can't.
func TestRangePickDrainRestoresFullProjection(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)
	h.state.RangePickConnID = "conn-a"
	h.state.RangePickTabID = 7
	h.state.RangePickCodewords = map[string]bool{"arch bake": true}

	h.drainRangePickLocked("browser", "test")

	if h.state.RangePickConnID != "" || len(h.state.RangePickCodewords) != 0 {
		t.Error("drain must clear the armed pick")
	}
	full, _ := h.buildTabPrefixState("conn-a", 7)
	if len(full["arch"]) != 3 {
		t.Errorf("expected the full set back after drain, got %v", full["arch"])
	}
}

// A closing tab can't send its own release, so the tab-wide session end drains
// the pick — otherwise the narrow would survive into the next occupant of the
// tab slot and hide its hints from the HUD.
func TestRangePickDrainsOnTabSessionEnd(t *testing.T) {
	h := newTestHost()
	newPickTestState(h)
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)
	h.state.RangePickConnID = "conn-a"
	h.state.RangePickTabID = 7
	h.state.RangePickCodewords = map[string]bool{"arch bake": true}

	h.cleanupTabSessionsLocked("browser", "conn-a", 7, "tab_close")

	if h.state.RangePickConnID != "" || len(h.state.RangePickCodewords) != 0 {
		t.Error("a tab-wide session end must drain that tab's armed pick")
	}
}
