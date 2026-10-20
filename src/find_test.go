package main

import (
	"errors"
	"testing"
)

// Find tag lifecycle tests swap the tag seams (find.go vars), mirroring
// caret_test.go. The find tag is a pure imperative mirror of the extension's
// find session — set on active, cleared on inactive/drain — so these assert
// the Put/Delete transitions and that they fire only on change.

func newFindTestState(h *Host) {
	h.state = &PluginState{PluginID: "browser"}
}

type findSeams struct {
	puts    int
	deletes int
}

func captureFindSeams(h *Host, t *testing.T) *findSeams {
	t.Helper()
	s := &findSeams{}
	origPut, origDelete := h.putFindTag, h.deleteFindTag
	h.putFindTag = func(string) error { s.puts++; return nil }
	h.deleteFindTag = func(string) error { s.deletes++; return nil }
	t.Cleanup(func() { h.putFindTag, h.deleteFindTag = origPut, origDelete })
	return s
}

func TestFindSyncPutsTagOnActiveOnce(t *testing.T) {
	h := newTestHost()
	newFindTestState(h)
	s := captureFindSeams(h, t)

	h.state.FindActive = true
	h.syncFindLocked("browser")
	h.syncFindLocked("browser") // second call must be a no-op
	if s.puts != 1 || s.deletes != 0 {
		t.Fatalf("expected 1 put, 0 deletes; got %d/%d", s.puts, s.deletes)
	}
	if !h.state.FindTagSet {
		t.Fatal("FindTagSet should mirror the held tag")
	}
}

func TestFindSyncDeletesTagOnInactive(t *testing.T) {
	h := newTestHost()
	newFindTestState(h)
	s := captureFindSeams(h, t)

	h.state.FindActive = true
	h.syncFindLocked("browser")
	h.state.FindActive = false
	h.syncFindLocked("browser")
	h.syncFindLocked("browser") // second call must be a no-op
	if s.puts != 1 || s.deletes != 1 {
		t.Fatalf("expected 1 put, 1 delete; got %d/%d", s.puts, s.deletes)
	}
	if h.state.FindTagSet {
		t.Fatal("FindTagSet should be false after delete")
	}
}

func TestFindDrainClearsTagAndConn(t *testing.T) {
	h := newTestHost()
	newFindTestState(h)
	s := captureFindSeams(h, t)

	h.state.FindConnID = "conn-1"
	h.state.FindActive = true
	h.syncFindLocked("browser")
	h.drainFindLocked("browser", "test")
	if s.deletes != 1 {
		t.Fatalf("expected 1 delete on drain; got %d", s.deletes)
	}
	if h.state.FindConnID != "" || h.state.FindActive || h.state.FindTagSet {
		t.Fatal("drain must clear conn, active, and tag state")
	}
}

// Same retry invariant as caret (caret_test.go): FindTagSet is what keeps
// drainFindLocked from short-circuiting, so a failed DELETE must leave it true.
// The find tag is non-exclusive, so a leak only strands "next"/"previous" —
// but the ordering rule is one rule, not a per-tag judgement call.
func TestFindDeleteFailureRetriesOnNextDrain(t *testing.T) {
	h := newTestHost()
	newFindTestState(h)
	deletes := 0
	failDelete := true
	origPut, origDelete := h.putFindTag, h.deleteFindTag
	h.putFindTag = func(string) error { return nil }
	h.deleteFindTag = func(string) error {
		deletes++
		if failDelete {
			return errors.New("rpc failed")
		}
		return nil
	}
	t.Cleanup(func() { h.putFindTag, h.deleteFindTag = origPut, origDelete })

	h.state.FindConnID = "conn-1"
	h.state.FindActive = true
	h.syncFindLocked("browser")

	h.drainFindLocked("browser", "first")
	if !h.state.FindTagSet {
		t.Fatal("a failed DELETE must leave FindTagSet true so the next drain retries")
	}

	failDelete = false
	h.drainFindLocked("browser", "retry")
	if deletes != 2 {
		t.Fatalf("second drain must retry the DELETE; deletes = %d, want 2", deletes)
	}
	if h.state.FindTagSet {
		t.Fatal("a successful retry must clear FindTagSet")
	}
}

func TestFindDrainIsNoOpWhenIdle(t *testing.T) {
	h := newTestHost()
	newFindTestState(h)
	s := captureFindSeams(h, t)

	h.drainFindLocked("browser", "test")
	if s.puts != 0 || s.deletes != 0 {
		t.Fatalf("idle drain must not touch the tag; got %d/%d", s.puts, s.deletes)
	}
}
