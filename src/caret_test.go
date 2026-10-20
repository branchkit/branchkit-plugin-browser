package main

import (
	"errors"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// Caret tag lifecycle tests swap the tag seams (caret.go vars), mirroring
// palette_test.go. The caret tag is a pure imperative mirror of the extension's
// mode — set on active, cleared on inactive/drain — so these assert the
// Put/Delete transitions and that they fire only on change.

func newCaretTestState(h *Host) {
	h.state = &PluginState{PluginID: "browser"}
}

type caretSeams struct {
	puts    int
	deletes int
}

func captureCaretSeams(h *Host, t *testing.T) *caretSeams {
	t.Helper()
	s := &caretSeams{}
	origPut, origDelete := h.putCaretTag, h.deleteCaretTag
	h.putCaretTag = func(string) error { s.puts++; return nil }
	h.deleteCaretTag = func(string) error { s.deletes++; return nil }
	t.Cleanup(func() { h.putCaretTag, h.deleteCaretTag = origPut, origDelete })
	return s
}

func TestCaretSyncPutsTagOnActiveOnce(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")
	h.syncCaretLocked("browser") // idempotent — no second Put

	if seams.puts != 1 || seams.deletes != 0 {
		t.Fatalf("tag puts/deletes = %d/%d, want 1/0", seams.puts, seams.deletes)
	}
	if !h.state.CaretTagSet {
		t.Fatal("CaretTagSet should be true after active sync")
	}
}

func TestCaretSyncDeletesTagOnInactive(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")
	h.state.CaretActive = false
	h.syncCaretLocked("browser")
	h.syncCaretLocked("browser") // idempotent — no second Delete

	if seams.puts != 1 || seams.deletes != 1 {
		t.Fatalf("tag puts/deletes = %d/%d, want 1/1", seams.puts, seams.deletes)
	}
	if h.state.CaretTagSet {
		t.Fatal("CaretTagSet should be false after inactive sync")
	}
}

func TestCaretDrainClearsTagAndConn(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")

	h.drainCaretLocked("browser", "test")

	if seams.deletes != 1 {
		t.Fatalf("drain deletes = %d, want 1", seams.deletes)
	}
	if h.state.CaretTagSet || h.state.CaretActive || h.state.CaretConnID != "" {
		t.Fatalf("drain left state: tagSet=%v active=%v conn=%q",
			h.state.CaretTagSet, h.state.CaretActive, h.state.CaretConnID)
	}
}

// --- mode-mirror forwarding (reconcileExternalTagClears' table) ---

// modeMirrorFor looks a mirror up by tag collection, so a test names the mode
// rather than an index into the table.
func modeMirrorFor(h *Host, t *testing.T, collection string) modeMirror {
	t.Helper()
	for _, m := range h.modeMirrors("browser") {
		if m.collection == collection {
			return m
		}
	}
	t.Fatalf("no mode mirror registered for %q", collection)
	return modeMirror{}
}

// captureModeExitEvents wires one SSE client so sendToFocusedClientsLocked has
// somewhere to deliver. FocusedBundleID is "" in test state, so focus is
// unknown and the lone client is the fail-open target (focusedClientLocked).
func captureModeExitEvents(h *Host, t *testing.T) chan ActionEvent {
	t.Helper()
	ch := make(chan ActionEvent, 8)
	h.state.SSEClients = map[string]chan ActionEvent{"client-1": ch}
	return ch
}

func drainedActions(ch chan ActionEvent) []string {
	var out []string
	for {
		select {
		case evt := <-ch:
			out = append(out, evt.Action)
		default:
			return out
		}
	}
}

// The table is exactly the tags this plugin WRITES. video_mode's absence is
// asserted, not merely unlisted: it is a decision (caret.go's modeMirrors
// carries the reasoning — the tag is hold-scoped while the extension's
// keyboard video layer is sticky, so forwarding would end the key layer at
// every hold boundary). If a future change adds it, this test should fail and
// send the reader to that comment before the entry ships.
func TestModeMirrorsCoverEveryPluginWrittenMode(t *testing.T) {
	h := newTestHost()
	// Video joined at Wave 3 C4b, when its tag became a pure mirror of the
	// extension's ONE video-layer lifetime — the sticky-vs-hold-scoped
	// mismatch that used to keep it out (and that an earlier version of this
	// test pinned as load-bearing) no longer exists.
	want := map[string]string{
		caretTagName("browser"):     "select_exit",
		"_surface":                  "palette_dismiss",
		videoModeTagName("browser"): "video_exit",
		// Keep scrolling: the same pure-mirror shape as video,
		// so "over" must stop the page as well as clear the tag.
		autoScrollTagName("browser"): "auto_scroll_stop",
	}
	got := map[string]string{}
	for _, m := range h.modeMirrors("browser") {
		got[m.collection] = m.action
	}
	if len(got) != len(want) {
		t.Fatalf("mode mirrors = %v, want %v", got, want)
	}
	for coll, action := range want {
		if got[coll] != action {
			t.Errorf("mirror %s forwards %q, want %q", coll, got[coll], action)
		}
	}
}

// The video half of the table forwards its exit only while we believe we hold
// the tag — same contract as palette's below.
func TestVideoExternalClearForwardsExitOnlyWhenHeld(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	events := captureModeExitEvents(h, t)
	m := modeMirrorFor(h, t, videoModeTagName("browser"))
	evt := branchkit.CollectionUpdatedEventParams{Collection: m.collection, Writer: "voice"}

	h.forwardModeExit("browser", m)(evt) // no mode held — nothing to exit
	if got := drainedActions(events); len(got) != 0 {
		t.Fatalf("forwarded %v with no video mode held, want nothing", got)
	}

	h.state.VideoModeTagSet = true
	h.forwardModeExit("browser", m)(evt)
	if got := drainedActions(events); len(got) != 1 || got[0] != "video_exit" {
		t.Fatalf("forwarded actions = %v, want [video_exit]", got)
	}
}

// The palette half of the table forwards its dismiss only when rows are
// held AND the `_surface` namespace no longer carries the palette's gate —
// the platform owns the tag since the `@palette` surface migration, so
// "externally cleared" is read from the namespace, not local bookkeeping.
// A detected clear also drains the rows (reconvergence).
func TestPaletteExternalClearForwardsDismissOnlyWhenHeld(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	capturePaletteSeams(h, t)
	events := captureModeExitEvents(h, t)
	m := modeMirrorFor(h, t, "_surface")
	evt := branchkit.CollectionUpdatedEventParams{Collection: m.collection, Writer: "_platform"}

	gates := []string{"browser.palette"}
	orig := h.listPaletteSurfaceGates
	h.listPaletteSurfaceGates = func() ([]string, error) { return gates, nil }
	t.Cleanup(func() { h.listPaletteSurfaceGates = orig })

	h.forwardModeExit("browser", m)(evt) // no palette open — nothing to dismiss
	if got := drainedActions(events); len(got) != 0 {
		t.Fatalf("forwarded %v with no palette open, want nothing", got)
	}

	h.state.PaletteConnID = "conn-1"
	h.state.PaletteEntries = []PaletteEntry{{Title: "Row", RowID: "tab:1"}}
	h.forwardModeExit("browser", m)(evt) // gate still present — an open, not a clear
	if got := drainedActions(events); len(got) != 0 {
		t.Fatalf("forwarded %v while the gate is still held, want nothing", got)
	}

	gates = nil // the platform's gate is gone while rows are held
	h.forwardModeExit("browser", m)(evt)
	if got := drainedActions(events); len(got) != 1 || got[0] != "palette_dismiss" {
		t.Fatalf("forwarded actions = %v, want [palette_dismiss]", got)
	}
	if h.state.PaletteConnID != "" || h.state.PaletteEntries != nil {
		t.Errorf("external clear must drain the rows (reconvergence)")
	}
}

// Our own Put/Delete must not bounce back as an exit imperative — that would
// turn every caret entry and every palette open into an immediate exit. Every
// mirror is checked, with its tag believed held, so the guard can't be right
// for one member and missing on another.
func TestModeExitForwardIgnoresOwnWrites(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	events := captureModeExitEvents(h, t)

	for _, m := range h.modeMirrors("browser") {
		h.state.CaretTagSet = true
		h.state.PaletteConnID = "conn-1"
		h.state.PaletteEntries = []PaletteEntry{{Title: "Row", RowID: "tab:1"}}
		h.forwardModeExit("browser", m)(branchkit.CollectionUpdatedEventParams{
			Collection: m.collection, Writer: "browser",
		})
		if got := drainedActions(events); len(got) != 0 {
			t.Fatalf("%s mirror forwarded %v on our own write, want nothing", m.label, got)
		}
	}
}

// The caret half of the table still forwards, and only while we believe we
// hold the tag — a clear arriving when no caret session is live is not an exit.
func TestCaretExternalClearForwardsSelectExitOnlyWhenHeld(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	events := captureModeExitEvents(h, t)
	m := modeMirrorFor(h, t, caretTagName("browser"))
	evt := branchkit.CollectionUpdatedEventParams{Collection: m.collection, Writer: "voice"}

	h.forwardModeExit("browser", m)(evt) // no caret session — nothing to exit
	if got := drainedActions(events); len(got) != 0 {
		t.Fatalf("forwarded %v with no caret session, want nothing", got)
	}

	h.state.CaretTagSet = true
	h.forwardModeExit("browser", m)(evt)
	if got := drainedActions(events); len(got) != 1 || got[0] != "select_exit" {
		t.Fatalf("forwarded actions = %v, want [select_exit]", got)
	}
}

// Video's sync carries the same invariant as caret's below: a failed DELETE
// leaves VideoModeTagSet TRUE so the next drain retries — an exclusive tag
// the plugin believes gone is a system-wide suppression until restart.
func TestVideoModeDeleteFailureRetriesOnNextDrain(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	deletes := 0
	failDelete := true
	origPut, origDelete := h.putVideoModeTag, h.deleteVideoModeTag
	h.putVideoModeTag = func(string) error { return nil }
	h.deleteVideoModeTag = func(string) error {
		deletes++
		if failDelete {
			return errors.New("rpc failed")
		}
		return nil
	}
	t.Cleanup(func() { h.putVideoModeTag, h.deleteVideoModeTag = origPut, origDelete })

	h.state.VideoModeConnID = "conn-a"
	h.state.VideoModeActive = true
	h.syncVideoModeLocked("browser")
	if !h.state.VideoModeTagSet {
		t.Fatal("tag should be set after a successful PUT")
	}

	h.drainVideoModeLocked("browser", "first")
	if !h.state.VideoModeTagSet {
		t.Fatal("failed DELETE must leave VideoModeTagSet true — it is the retry record")
	}

	failDelete = false
	h.drainVideoModeLocked("browser", "retry")
	if h.state.VideoModeTagSet {
		t.Fatal("retry drain should clear the tag")
	}
	if deletes != 2 {
		t.Fatalf("deletes = %d, want 2 (fail then retry)", deletes)
	}
}

// A failed tag DELETE must leave CaretTagSet TRUE. The flag is the whole retry
// story: drainCaretLocked short-circuits when conn/active/tagSet are all clear,
// so clearing the flag ahead of a Delete that then fails leaves the platform
// holding an EXCLUSIVE tag no later drain will ever retry — every voice command
// in the system suppressed until a restart.
func TestCaretDeleteFailureRetriesOnNextDrain(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	deletes := 0
	failDelete := true
	origPut, origDelete := h.putCaretTag, h.deleteCaretTag
	h.putCaretTag = func(string) error { return nil }
	h.deleteCaretTag = func(string) error {
		deletes++
		if failDelete {
			return errors.New("rpc failed")
		}
		return nil
	}
	t.Cleanup(func() { h.putCaretTag, h.deleteCaretTag = origPut, origDelete })

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")

	h.drainCaretLocked("browser", "first")
	if !h.state.CaretTagSet {
		t.Fatal("a failed DELETE must leave CaretTagSet true so the next drain retries")
	}

	failDelete = false
	h.drainCaretLocked("browser", "retry")
	if deletes != 2 {
		t.Fatalf("second drain must retry the DELETE; deletes = %d, want 2", deletes)
	}
	if h.state.CaretTagSet {
		t.Fatal("a successful retry must clear CaretTagSet")
	}
}

func TestCaretDrainIsNoOpWhenIdle(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)

	h.drainCaretLocked("browser", "test") // nothing held

	if seams.puts != 0 || seams.deletes != 0 {
		t.Fatalf("idle drain touched tags: puts/deletes = %d/%d", seams.puts, seams.deletes)
	}
}

// Video mode tracks its own publisher: when the connection that entered it
// disconnects, the exclusive tag is dropped even though caret mode was never
// on. It used to drain only inside the caret-connection check, so this case
// kept every other voice command suppressed until an app switch.
func TestVideoModeDrainsWhenItsConnectionDisconnectsWithoutCaret(t *testing.T) {
	h, _ := newRoutingHost(t)
	client, _ := connectBrowser(h, "conn-v", chromeBundle)
	deletes := 0
	origPut, origDelete := h.putVideoModeTag, h.deleteVideoModeTag
	h.putVideoModeTag = func(string) error { return nil }
	h.deleteVideoModeTag = func(string) error { deletes++; return nil }
	t.Cleanup(func() { h.putVideoModeTag, h.deleteVideoModeTag = origPut, origDelete })

	h.state.VideoModeConnID = "conn-v"
	h.state.VideoModeActive = true
	h.syncVideoModeLocked("browser")
	if !h.state.VideoModeTagSet || h.state.CaretConnID != "" {
		t.Fatal("setup: video mode on, caret off")
	}

	h.disconnectSSEClient("browser", "conn-v", client)

	if h.state.VideoModeTagSet || deletes != 1 {
		t.Fatalf("video_mode tag must drop with its connection: set=%v deletes=%d",
			h.state.VideoModeTagSet, deletes)
	}
}

// A caret PUT that landed but whose answer timed out must not strand the
// EXCLUSIVE tag: the exit that follows still sends the Delete. Before the
// uncertain state, CaretTagSet stayed false and the clear branch never ran.
func TestCaretTimedOutPutStillClearsOnExit(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)
	h.putCaretTag = func(string) error {
		seams.puts++
		return &branchkit.CallTimeoutError{Method: "collection.put"}
	}

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")
	if !h.state.CaretTagUncertain {
		t.Fatal("a timed-out PUT leaves the tag uncertain")
	}

	h.drainCaretLocked("browser", "extension_exit")
	if seams.deletes != 1 {
		t.Fatalf("the exit must Delete a tag that may be held; deletes = %d", seams.deletes)
	}
	if h.state.CaretTagSet || h.state.CaretTagUncertain {
		t.Fatal("a confirmed Delete settles the state as clear")
	}
}

// While active and uncertain, the next sync re-asserts the PUT rather than
// treating the tag as absent-and-done or present-and-done.
func TestCaretUncertainPutIsReassertedOnNextSync(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)
	fail := true
	h.putCaretTag = func(string) error {
		seams.puts++
		if fail {
			return &branchkit.CallTimeoutError{Method: "collection.put"}
		}
		return nil
	}

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")
	fail = false
	h.syncCaretLocked("browser")
	if seams.puts != 2 || !h.state.CaretTagSet || h.state.CaretTagUncertain {
		t.Fatalf("puts=%d set=%v uncertain=%v, want 2/true/false",
			seams.puts, h.state.CaretTagSet, h.state.CaretTagUncertain)
	}
}

// A definite refusal is not uncertain: nothing was written, so an exit has
// nothing to delete.
func TestCaretRefusedPutIsNotUncertain(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	seams := captureCaretSeams(h, t)
	h.putCaretTag = func(string) error { seams.puts++; return errors.New("refused") }

	h.state.CaretConnID = "conn-a"
	h.state.CaretActive = true
	h.syncCaretLocked("browser")
	h.drainCaretLocked("browser", "extension_exit")
	if h.state.CaretTagUncertain || seams.deletes != 0 {
		t.Fatalf("uncertain=%v deletes=%d, want false/0", h.state.CaretTagUncertain, seams.deletes)
	}
}

// Another browser taking focus drains video mode against video mode's OWN
// connection. The drain used to sit inside the caret check, so video mode
// started with caret off kept its exclusive tag while the second browser was
// in front, and that browser's commands stayed suppressed.
func TestVideoModeDrainsOnFocusSwitchWithoutCaret(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	deletes := 0
	origPut, origDelete := h.putVideoModeTag, h.deleteVideoModeTag
	h.putVideoModeTag = func(string) error { return nil }
	h.deleteVideoModeTag = func(string) error { deletes++; return nil }
	t.Cleanup(func() { h.putVideoModeTag, h.deleteVideoModeTag = origPut, origDelete })
	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")

	h.mu.Lock()
	h.state.VideoModeConnID = "profile-1"
	h.state.VideoModeActive = true
	h.syncVideoModeLocked("browser")
	h.mu.Unlock()
	if !h.state.VideoModeTagSet || h.state.CaretConnID != "" {
		t.Fatal("setup: video mode on, caret off")
	}
	claimFocus(t, h, "profile-2")
	if h.state.VideoModeTagSet || deletes != 1 {
		t.Fatalf("focus switch must drain video mode: set=%v deletes=%d", h.state.VideoModeTagSet, deletes)
	}
}
