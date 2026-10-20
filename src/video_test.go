package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Media-active tag lifecycle tests swap the tag seams (video.go vars),
// mirroring caret_test.go. The tag is the OR of per-connection assertions —
// set while any connection reports controllable media, cleared when the
// last drops — and these assert the Put/Delete transitions fire only on
// change, plus the contributed "video" context's dual-gated variants.

func newMediaTestState(h *Host) {
	h.state = &PluginState{PluginID: "browser"}
}

type mediaSeams struct {
	puts    int
	deletes int
}

func captureMediaSeams(h *Host, t *testing.T) *mediaSeams {
	t.Helper()
	s := &mediaSeams{}
	origPut, origDelete := h.putMediaActiveTag, h.deleteMediaActiveTag
	h.putMediaActiveTag = func(string) error { s.puts++; return nil }
	h.deleteMediaActiveTag = func(string) error { s.deletes++; return nil }
	t.Cleanup(func() { h.putMediaActiveTag, h.deleteMediaActiveTag = origPut, origDelete })
	return s
}

func TestMediaActiveSyncPutsTagOnFirstAssertOnce(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	seams := captureMediaSeams(h, t)

	h.state.conn("conn-a").Session.MediaActive = true
	h.syncMediaActiveLocked("browser")
	h.state.conn("conn-b").Session.MediaActive = true
	h.syncMediaActiveLocked("browser") // still ORed true — no second Put

	if seams.puts != 1 || seams.deletes != 0 {
		t.Fatalf("tag puts/deletes = %d/%d, want 1/0", seams.puts, seams.deletes)
	}
	if !h.state.MediaTagSet {
		t.Fatal("MediaTagSet should be true after assert sync")
	}
}

func TestMediaActiveTagSurvivesUntilLastConnClears(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	seams := captureMediaSeams(h, t)

	h.state.conn("conn-a").Session.MediaActive = true
	h.state.conn("conn-b").Session.MediaActive = true
	h.syncMediaActiveLocked("browser")

	h.state.conn("conn-a").Session.MediaActive = false
	h.syncMediaActiveLocked("browser") // conn-b still asserts — tag stays
	if seams.deletes != 0 || !h.state.MediaTagSet {
		t.Fatalf("tag dropped while a conn still asserts (deletes=%d)", seams.deletes)
	}

	h.state.conn("conn-b").Session.MediaActive = false
	h.syncMediaActiveLocked("browser")
	if seams.deletes != 1 || h.state.MediaTagSet {
		t.Fatalf("tag should clear when the last conn drops (deletes=%d)", seams.deletes)
	}
}

func TestDroppedConnTakesOnlyItsMediaAssertion(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	seams := captureMediaSeams(h, t)

	h.state.conn("conn-a").Session.MediaActive = true
	h.state.conn("conn-b").Session.MediaActive = true
	h.syncMediaActiveLocked("browser")

	// What the disconnect path does with a dropped connection.
	drop := func(conn string) {
		delete(h.state.Conns, conn)
		h.syncMediaActiveLocked("browser")
	}
	drop("conn-a")
	if !h.state.MediaTagSet {
		t.Fatal("tag must survive: conn-b still asserts")
	}
	drop("conn-a") // no-op — already dropped
	drop("conn-b")
	if h.state.MediaTagSet || seams.deletes != 1 {
		t.Fatalf("tag should clear once after last drop (deletes=%d)", seams.deletes)
	}
}

// Same retry invariant as caret (caret_test.go): MediaTagSet is the only record
// that the tag is still held platform-side, and the `!want && MediaTagSet`
// branch is the only thing that re-issues the DELETE. Clearing the flag ahead
// of a Delete that then fails makes every later sync a no-op, so the tag leaks
// — "pause"/"faster" stay hearable with nothing playing anywhere.
func TestMediaActiveDeleteFailureRetriesOnNextSync(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	deletes := 0
	failDelete := true
	origPut, origDelete := h.putMediaActiveTag, h.deleteMediaActiveTag
	h.putMediaActiveTag = func(string) error { return nil }
	h.deleteMediaActiveTag = func(string) error {
		deletes++
		if failDelete {
			return errors.New("rpc failed")
		}
		return nil
	}
	t.Cleanup(func() { h.putMediaActiveTag, h.deleteMediaActiveTag = origPut, origDelete })

	h.state.conn("conn-a").Session.MediaActive = true
	h.syncMediaActiveLocked("browser")

	h.state.conn("conn-a").Session.MediaActive = false
	h.syncMediaActiveLocked("browser")
	if !h.state.MediaTagSet {
		t.Fatal("a failed DELETE must leave MediaTagSet true so the next sync retries")
	}

	failDelete = false
	h.syncMediaActiveLocked("browser")
	if deletes != 2 {
		t.Fatalf("second sync must retry the DELETE; deletes = %d, want 2", deletes)
	}
	if h.state.MediaTagSet {
		t.Fatal("a successful retry must clear MediaTagSet")
	}
}

// The "video" voice context produces: (a) a media-active-gated bare variant
// with NO app-active requirement (background control from any app), (b) a
// mode-gated variant (no tag writes — the exclusive tag is the mode stack's
// mirror of the extension layer since Wave 3 C4b, sticky until exit), and
// (c) exactly one "video" mode-entry command whose ACTION drives the
// extension's video layer, the same entry the `w` key takes.
func TestContributedVideoContextDualVariants(t *testing.T) {
	h := newTestHost()
	newMediaTestState(h)
	h.storeContributedCommands("", []contributedCommand{
		{Action: "media_play_pause", Pattern: "pause",
			Params: map[string]string{"op": "pause"}, Category: "Media", Context: "video"},
		{Action: "media_speed", Pattern: "faster",
			Params: map[string]string{"op": "faster"}, Category: "Media", Context: "video"},
	})
	t.Cleanup(func() { h.storeContributedCommands("", nil) })

	specs := h.buildContributedCommands("browser")
	mediaTag := mediaActiveTagName("browser")
	modeTag := videoModeTagName("browser")

	var bareVariants, modeVariants, entries int
	for _, s := range specs {
		raw, _ := json.Marshal(s)
		js := string(raw)
		requires := strings.Join(s.RequiresTags, ",")
		switch {
		case strings.Contains(requires, mediaTag) && strings.Contains(js, "browser.video_mode"):
			entries++
			if s.Action == nil {
				t.Fatalf("entry command must carry the browser.video_mode action, got nil")
			}
			if len(s.SetsTags) != 0 {
				t.Fatalf("entry must not write the tag (the mirror is the writer), got %v", s.SetsTags)
			}
		case strings.Contains(requires, mediaTag):
			bareVariants++
			if strings.Contains(requires, "plugin.browser.active") {
				t.Fatalf("bare variant must NOT require app-active (background control), got %v", s.RequiresTags)
			}
			if len(s.SetsTags) != 0 {
				t.Fatalf("bare variant must not set tags, got %v", s.SetsTags)
			}
		case strings.Contains(requires, modeTag):
			modeVariants++
			if len(s.SetsTags) != 0 {
				t.Fatalf("mode variant must not write the sticky mirror tag, got %v", s.SetsTags)
			}
		}
	}
	if bareVariants != 2 || modeVariants != 2 || entries != 1 {
		t.Fatalf("variants bare/mode/entry = %d/%d/%d, want 2/2/1",
			bareVariants, modeVariants, entries)
	}
}
