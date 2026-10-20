package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// Keep-scrolling's exclusive tag (autoscroll.go). Every drain
// path is pinned, because a stranded exclusive tag suppresses every other
// voice command system-wide.

type autoScrollSeams struct{ puts, deletes int }

func captureAutoScrollSeams(h *Host, t *testing.T) *autoScrollSeams {
	t.Helper()
	s := &autoScrollSeams{}
	origPut, origDelete := h.putAutoScrollTag, h.deleteAutoScrollTag
	h.putAutoScrollTag = func(string) error { s.puts++; return nil }
	h.deleteAutoScrollTag = func(string) error { s.deletes++; return nil }
	t.Cleanup(func() { h.putAutoScrollTag, h.deleteAutoScrollTag = origPut, origDelete })
	return s
}

func postAutoScroll(t *testing.T, h *Host, conn string, active bool) {
	t.Helper()
	putSession(t, h, wire.SessionRequest{ConnID: conn, Modes: wire.SessionModes{AutoScroll: active}})
}

func TestAutoScrollPostSetsAndClearsTagOnEdgesOnly(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	s := captureAutoScrollSeams(h, t)

	postAutoScroll(t, h, "conn-a", true)
	postAutoScroll(t, h, "conn-a", true) // redundant re-assert: no second PUT
	if s.puts != 1 || !h.state.AutoScrollTagSet {
		t.Fatalf("puts=%d set=%v, want one PUT and the tag held", s.puts, h.state.AutoScrollTagSet)
	}
	postAutoScroll(t, h, "conn-a", false)
	if s.deletes != 1 || h.state.AutoScrollTagSet {
		t.Fatalf("deletes=%d set=%v, want one DELETE and the tag gone", s.deletes, h.state.AutoScrollTagSet)
	}
}

// A background browser must not be able to put the system into an exclusive
// mode over the focused one's speech, and its own "not scrolling" says nothing
// about the browser in front: only the slot's holder releases it.
func TestAutoScrollAssertOnlyFromFocusedConnection(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	s := captureAutoScrollSeams(h, t)
	h.state.FocusedConnID = "conn-front"

	postAutoScroll(t, h, "conn-behind", true)
	if s.puts != 0 || h.state.AutoScrollTagSet {
		t.Fatalf("an unfocused browser set the exclusive tag (puts=%d)", s.puts)
	}
	postAutoScroll(t, h, "conn-front", true)
	if !h.state.AutoScrollTagSet {
		t.Fatal("the focused browser's run must hold the tag")
	}
	postAutoScroll(t, h, "conn-behind", false)
	if !h.state.AutoScrollTagSet {
		t.Fatal("another connection's session must not end the focused browser's run")
	}
	postAutoScroll(t, h, "conn-front", false)
	if h.state.AutoScrollTagSet {
		t.Fatal("the holder's session ending the run must release the tag")
	}
}

// Same invariant as caret and video: a failed DELETE leaves the tag believed
// SET, which is the retry record the next drain acts on.
func TestAutoScrollDeleteFailureRetriesOnNextDrain(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	failDelete := true
	deletes := 0
	origPut, origDelete := h.putAutoScrollTag, h.deleteAutoScrollTag
	h.putAutoScrollTag = func(string) error { return nil }
	h.deleteAutoScrollTag = func(string) error {
		deletes++
		if failDelete {
			return errors.New("rpc failed")
		}
		return nil
	}
	t.Cleanup(func() { h.putAutoScrollTag, h.deleteAutoScrollTag = origPut, origDelete })

	h.state.AutoScrollConnID = "conn-a"
	h.state.AutoScrollActive = true
	h.syncAutoScrollLocked("browser")
	h.drainAutoScrollLocked("browser", "first")
	if !h.state.AutoScrollTagSet {
		t.Fatal("failed DELETE must leave AutoScrollTagSet true — it is the retry record")
	}
	failDelete = false
	h.drainAutoScrollLocked("browser", "retry")
	if h.state.AutoScrollTagSet || deletes != 2 {
		t.Fatalf("retry must clear the tag: set=%v deletes=%d", h.state.AutoScrollTagSet, deletes)
	}
}

// "over" clears the tag from outside; the plugin forwards auto_scroll_stop so
// the page stops too — and only while it believes it holds the tag.
func TestAutoScrollExternalClearForwardsStopOnlyWhenHeld(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	events := captureModeExitEvents(h, t)
	m := modeMirrorFor(h, t, autoScrollTagName("browser"))
	evt := branchkit.CollectionUpdatedEventParams{Collection: m.collection, Writer: "voice"}

	h.forwardModeExit("browser", m)(evt)
	if got := drainedActions(events); len(got) != 0 {
		t.Fatalf("forwarded %v with no run held, want nothing", got)
	}
	h.state.AutoScrollTagSet = true
	h.forwardModeExit("browser", m)(evt)
	if got := drainedActions(events); len(got) != 1 || got[0] != "auto_scroll_stop" {
		t.Fatalf("forwarded actions = %v, want [auto_scroll_stop]", got)
	}
}

// The run's connection dying drops the tag even with caret never on (the
// same shape as video's media tag, which once outlived its connection).
func TestAutoScrollDrainsWhenItsConnectionDisconnects(t *testing.T) {
	h, _ := newRoutingHost(t)
	client, _ := connectBrowser(h, "conn-s", chromeBundle)
	s := captureAutoScrollSeams(h, t)

	h.state.AutoScrollConnID = "conn-s"
	h.state.AutoScrollActive = true
	h.syncAutoScrollLocked("browser")

	h.disconnectSSEClient("browser", "conn-s", client)
	if h.state.AutoScrollTagSet || s.deletes != 1 {
		t.Fatalf("auto_scroll tag must drop with its connection: set=%v deletes=%d",
			h.state.AutoScrollTagSet, s.deletes)
	}
}

// Another browser taking focus drains the run's tag — checked against the
// run's OWN connection, not caret's.
func TestAutoScrollDrainsOnFocusSwitchWithoutCaret(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	s := captureAutoScrollSeams(h, t)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")

	postAutoScroll(t, h, "profile-1", true)
	if !h.state.AutoScrollTagSet || h.state.CaretConnID != "" {
		t.Fatal("setup: run on, caret off")
	}
	claimFocus(t, h, "profile-2")
	if h.state.AutoScrollTagSet || s.deletes != 1 {
		t.Fatalf("focus switch must drain the run's tag: set=%v deletes=%d", h.state.AutoScrollTagSet, s.deletes)
	}
}

// The in-mode words are gated on the exclusive tag ALONE: no app-active, no
// clear at match (many "faster"s per run), and no collision with the video
// context's "faster" — they are separate spoken identities.
func TestContributedAutoScrollContextGatesOnItsTag(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	h.storeContributedCommands("", []contributedCommand{
		{Action: "auto_scroll", Pattern: "keep scrolling", Category: "Scroll"},
		{Action: "auto_scroll_speed", Pattern: "faster",
			Params: map[string]string{"op": "faster"}, Category: "Scroll", Context: "auto_scroll"},
		{Action: "auto_scroll_stop", Pattern: "stop", Category: "Scroll", Context: "auto_scroll"},
		{Action: "media_speed", Pattern: "faster",
			Params: map[string]string{"op": "faster"}, Category: "Media", Context: "video"},
	})
	t.Cleanup(func() { h.storeContributedCommands("", nil) })

	tag := autoScrollTagName("browser")
	var inMode, entry int
	for _, s := range h.buildContributedCommands("browser") {
		var act struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(s.Action, &act) != nil {
			continue
		}
		switch act.Type {
		case "browser.auto_scroll_speed", "browser.auto_scroll_stop":
			inMode++
			if len(s.RequiresTags) != 1 || s.RequiresTags[0] != tag {
				t.Errorf("%s requires %v, want exactly [%s]", act.Type, s.RequiresTags, tag)
			}
			if len(s.ClearsTags) != 0 || len(s.SetsTags) != 0 {
				t.Errorf("%s must not write the mirror tag: sets=%v clears=%v", act.Type, s.SetsTags, s.ClearsTags)
			}
		case "browser.auto_scroll":
			entry++
			if len(s.RequiresTags) != 1 || s.RequiresTags[0] != "plugin.browser.active" {
				t.Errorf("entry requires %v, want the app-active gate", s.RequiresTags)
			}
		}
	}
	if inMode != 2 || entry != 1 {
		t.Fatalf("in-mode/entry = %d/%d, want 2/1", inMode, entry)
	}
}
