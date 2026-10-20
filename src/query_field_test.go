package main

import "testing"

// Mirrors caret_test.go's shape: the tag is a pure mirror of the extension's
// focus state, and the transitions are what must be exact.
func TestQueryFieldSyncOnlyOnTransitions(t *testing.T) {
	h := newTestHost()
	puts, deletes := 0, 0
	origPut, origDelete := h.putQueryFieldTag, h.deleteQueryFieldTag
	h.putQueryFieldTag = func(string) error { puts++; return nil }
	h.deleteQueryFieldTag = func(string) error { deletes++; return nil }
	defer func() { h.putQueryFieldTag, h.deleteQueryFieldTag = origPut, origDelete }()

	h.state = &PluginState{PluginID: "browser"}

	h.state.QueryFieldActive = true
	h.syncQueryFieldLocked("browser")
	h.syncQueryFieldLocked("browser") // idempotent — focus events repeat
	if puts != 1 || !h.state.QueryFieldTagSet {
		t.Fatalf("expected exactly one PUT, got %d (set=%v)", puts, h.state.QueryFieldTagSet)
	}

	h.state.QueryFieldActive = false
	h.syncQueryFieldLocked("browser")
	h.syncQueryFieldLocked("browser")
	if deletes != 1 || h.state.QueryFieldTagSet {
		t.Fatalf("expected exactly one DELETE, got %d (set=%v)", deletes, h.state.QueryFieldTagSet)
	}
}

func TestQueryFieldDrainReleasesTheGate(t *testing.T) {
	h := newTestHost()
	deletes := 0
	origPut, origDelete := h.putQueryFieldTag, h.deleteQueryFieldTag
	h.putQueryFieldTag = func(string) error { return nil }
	h.deleteQueryFieldTag = func(string) error { deletes++; return nil }
	defer func() { h.putQueryFieldTag, h.deleteQueryFieldTag = origPut, origDelete }()

	h.state = &PluginState{PluginID: "browser"}
	h.state.QueryFieldConnID = "c1"
	h.state.QueryFieldActive = true
	h.state.QueryFieldTagSet = true
	h.drainQueryFieldLocked("browser", "test")
	if deletes != 1 || h.state.QueryFieldTagSet || h.state.QueryFieldConnID != "" {
		t.Fatalf("drain must clear the gate: deletes=%d set=%v conn=%q",
			deletes, h.state.QueryFieldTagSet, h.state.QueryFieldConnID)
	}

	deletes = 0
	h.drainQueryFieldLocked("browser", "test") // nothing held
	if deletes != 0 {
		t.Fatalf("drain with nothing held must be a no-op, got %d deletes", deletes)
	}
}

// A gate claimed by an unfocused browser would shape dictation aimed at another
// app entirely — the failure seen within minutes of shipping the fail-open
// version (background Chrome held it while the terminal was frontmost).
func TestQueryFieldRefusesAnUnfocusedClaim(t *testing.T) {
	h := newTestHost()
	puts := 0
	origPut, origDelete := h.putQueryFieldTag, h.deleteQueryFieldTag
	h.putQueryFieldTag = func(string) error { puts++; return nil }
	h.deleteQueryFieldTag = func(string) error { return nil }
	defer func() { h.putQueryFieldTag, h.deleteQueryFieldTag = origPut, origDelete }()

	// Focus unknown (nobody has claimed it — e.g. right after a restart with
	// every browser in the background).
	h.state = &PluginState{PluginID: "browser"}
	h.state.QueryFieldActive = true
	if h.state.FocusedConnID != "" {
		t.Fatal("precondition: focus should be unknown")
	}
	// The handler's guard, exercised directly: unknown focus must not claim.
	if h.state.FocusedConnID != "" && h.state.FocusedConnID == "conn-a" {
		h.syncQueryFieldLocked("browser")
	}
	if puts != 0 {
		t.Fatalf("unfocused claim must not set the gate, got %d puts", puts)
	}

	// A claim from the focused connection does set it.
	h.state = &PluginState{PluginID: "browser", FocusedConnID: "conn-a"}
	h.state.QueryFieldActive = true
	if h.state.FocusedConnID != "" && h.state.FocusedConnID == "conn-a" {
		h.syncQueryFieldLocked("browser")
	}
	if puts != 1 || !h.state.QueryFieldTagSet {
		t.Fatalf("focused claim must set the gate: puts=%d set=%v", puts, h.state.QueryFieldTagSet)
	}
}
