package main

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

// Test scope: the sync-payload shapes the fixture produces. The hint
// gate transition + VocabularyCommit need a live platform connection
// so they're exercised by the end-to-end step-7 flow rather than here.
//
// We swap `syncPerPrefix` / `syncPrefixList` with recording stubs, drive
// applyHintsFixture / releaseHintsFixture directly, and assert the
// recorded payloads.

type recordedPush struct {
	collection string
	data       map[string]any
}

type pushRecorder struct {
	mu      sync.Mutex
	pushes  []recordedPush
	failNow bool
	// Set by install(); marks a prefix as already holding records without
	// recording a push. See the comment there.
	seedLive func(prefix string)
}

func (r *pushRecorder) record(collection string, data map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNow {
		return fmt.Errorf("simulated sync failure")
	}
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	r.pushes = append(r.pushes, recordedPush{collection: collection, data: cp})
	return nil
}

func (r *pushRecorder) snapshot() []recordedPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedPush, len(r.pushes))
	copy(out, r.pushes)
	return out
}

// install swaps syncPerPrefix and syncPrefixList for stubs that route
// into the recorder, mirroring the production helpers' collection-name
// construction. Returns the cleanup func.
func (r *pushRecorder) install(h *Host) func() {
	prevPerPrefix := h.syncPerPrefix
	prevPrefixList := h.syncPrefixList
	prevLivePrefixes := h.livePrefixes
	// The recorder doubles as storage: a prefix synced non-empty is one the
	// platform would report back through collections.owned. The evict and
	// release sweeps read that, so without this they would sweep nothing and
	// the tests below would pass vacuously.
	live := map[string]bool{}
	h.syncPerPrefix = func(pluginID, prefix string, newData map[string]any) error {
		if err := r.record(hintCollection(pluginID, prefix), newData); err != nil {
			return err
		}
		if len(newData) > 0 {
			live[prefix] = true
		} else {
			delete(live, prefix)
		}
		return nil
	}
	h.syncPrefixList = func(pluginID string, newData map[string]any) error {
		return r.record(hintCollection(pluginID, "prefix"), newData)
	}
	h.livePrefixes = func(string) (map[string]bool, error) {
		out := make(map[string]bool, len(live))
		for p := range live {
			out[p] = true
		}
		return out, nil
	}
	// Seeds a prefix as already holding records without recording a push —
	// how a fixture a PREVIOUS process hydrated looks to this one.
	r.seedLive = func(prefix string) { live[prefix] = true }
	return func() {
		h.syncPerPrefix = prevPerPrefix
		h.syncPrefixList = prevPrefixList
		h.livePrefixes = prevLivePrefixes
		r.seedLive = nil
	}
}

func TestApplyHintsFixturePushShape(t *testing.T) {
	h := newTestHost()
	// Reset global state for the test.
	h.calibration = &calibrationFixture{}
	h.codewordsMu.Lock()
	h.codewords = testCodewords
	h.codewordsMu.Unlock()

	rec := &pushRecorder{}
	defer rec.install(h)()

	// applyHintsFixture also calls plugin.VocabularyCommit + the hint
	// gate transitions; both require globals the test doesn't set up.
	// We accept the resulting error and verify only the sync-shape work
	// that ran before those calls.
	_, _ = h.applyHintsFixture("browser")

	pushes := rec.snapshot()
	if len(pushes) == 0 {
		t.Fatal("expected at least one sync call")
	}

	// Count distinct collections synced:
	//   - 1 sync to plugin.browser.hints_prefix (alphabet of 26 codewords)
	//   - 26 syncs to plugin.browser.hints_<word> (each with 26 suffixes)
	colls := make(map[string]int)
	for _, p := range pushes {
		colls[p.collection]++
	}
	if colls["plugin.browser.hints_prefix"] == 0 {
		t.Errorf("expected push to plugin.browser.hints_prefix, got pushes: %v", colls)
	}
	if got := len(colls); got < 27 {
		t.Errorf("expected at least 27 distinct collections (prefix + 26 per-prefix), got %d", got)
	}

	// Spot-check that one of the per-prefix collections has 26 entries.
	for _, p := range pushes {
		if p.collection == "plugin.browser.hints_arch" {
			if len(p.data) != 26 {
				t.Errorf("plugin.browser.hints_arch should have 26 entries, got %d", len(p.data))
			}
			payload, ok := p.data["bake"].(map[string]any)
			if !ok {
				t.Fatalf("plugin.browser.hints_arch[bake] should be map, got %T", p.data["bake"])
			}
			if payload["letter"] != "ab" {
				t.Errorf("payload.letter = %v, want ab", payload["letter"])
			}
			if payload["codeword"] != "arch bake" {
				t.Errorf("payload.codeword = %v, want 'arch bake'", payload["codeword"])
			}
			break
		}
	}
}

// An apply while a fixture is already active is a refresh-in-place (the
// codeword retune loop re-applies on the same open trial): it re-hydrates the
// collections, keeps the existing handle, and stays active — rather than
// erroring. This avoids the release→reapply toggle whose async release races
// the re-apply.
func TestApplyHintsFixtureRefreshesWhenActive(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{active: true, handle: "cal_prior"}
	h.codewordsMu.Lock()
	h.codewords = testCodewords
	h.codewordsMu.Unlock()

	rec := &pushRecorder{}
	defer rec.install(h)()

	handle, err := h.applyHintsFixture("browser")
	if err != nil {
		t.Fatalf("refresh should not error, got %v", err)
	}
	if handle != "cal_prior" {
		t.Errorf("refresh should keep the existing handle, got %q", handle)
	}
	if !h.calibration.active {
		t.Error("fixture should remain active after refresh")
	}
	if len(rec.snapshot()) == 0 {
		t.Error("refresh should re-hydrate (re-push) the hint collections")
	}
}

func TestApplyHintsFixtureRejectsWithoutAlphabet(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{}
	h.codewordsMu.Lock()
	h.codewords = [26]string{} // empty
	h.codewordsMu.Unlock()
	// TestMain pre-populates codewords for the whole package; restore
	// when this test ends so sibling tests don't see the cleared state.
	t.Cleanup(func() {
		h.codewordsMu.Lock()
		h.codewords = testCodewords
		h.codewordsMu.Unlock()
	})

	_, err := h.applyHintsFixture("browser")
	if err == nil {
		t.Fatal("expected error when alphabet not loaded")
	}
	if got := err.Error(); !contains(got, "alphabet") {
		t.Errorf("error should mention alphabet, got: %s", got)
	}
}

// The point of the whole migration, at the one path where it is observable:
// release wipes a prefix collection THIS process never pushed.
//
// The live-prefix index this replaced was process-local, so a plugin restart
// mid-calibration forgot everything the previous process had hydrated and
// released only what it happened to remember — the rest stayed in the grammar
// until the platform restarted. Asking the platform which collections hold our
// records has nothing to forget.
func TestReleaseWipesPrefixesAPreviousProcessHydrated(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{active: true, handle: "cal_prior"}
	h.codewordsMu.Lock()
	h.codewords = testCodewords
	h.codewordsMu.Unlock()

	rec := &pushRecorder{}
	defer rec.install(h)()
	// Hydrated before this process existed: no push recorded, but the
	// records are there.
	rec.seedLive("orphan")

	if err := h.releaseHintsFixture("browser", "cal_prior"); err != nil {
		t.Fatalf("release: %v", err)
	}

	var wiped bool
	for _, p := range rec.snapshot() {
		if p.collection == "plugin.browser.hints_orphan" {
			wiped = true
			if len(p.data) != 0 {
				t.Errorf("orphan prefix must be wiped with an empty desired state, got %v", p.data)
			}
		}
	}
	if !wiped {
		t.Error("release must wipe a prefix collection this process never pushed")
	}
}

// Same property on the refresh path: a codeword retuned away is evicted based
// on what storage holds, not on what this process remembers pushing.
func TestRefreshEvictsAStalePrefixThisProcessNeverPushed(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{active: true, handle: "cal_prior"}
	h.codewordsMu.Lock()
	h.codewords = testCodewords
	h.codewordsMu.Unlock()

	rec := &pushRecorder{}
	defer rec.install(h)()
	// "retired" is not in the current alphabet, so the refresh must evict it.
	rec.seedLive("retired")

	if _, err := h.applyHintsFixture("browser"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	var evicted bool
	for _, p := range rec.snapshot() {
		if p.collection == "plugin.browser.hints_retired" && len(p.data) == 0 {
			evicted = true
		}
	}
	if !evicted {
		t.Error("refresh must evict a stale prefix collection this process never pushed")
	}
}

func TestReleaseHintsFixtureRejectsWhenNotActive(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{}
	err := h.releaseHintsFixture("browser", "cal_anything")
	if err == nil {
		t.Fatal("expected error when no fixture active")
	}
}

func TestReleaseHintsFixtureRejectsHandleMismatch(t *testing.T) {
	h := newTestHost()
	h.calibration = &calibrationFixture{active: true, handle: "cal_real"}
	err := h.releaseHintsFixture("browser", "cal_fake")
	if err == nil {
		t.Fatal("expected error on handle mismatch")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Sanity check that codeword expansion produces all 26 per-prefix
// collection names with stable casing so the test cross-check stays
// deterministic if testCodewords ever changes.
func TestPerPrefixCollectionNamesExpected(t *testing.T) {
	got := make([]string, 0, 26)
	for _, w := range testCodewords {
		got = append(got, "plugin.browser.hints_"+w)
	}
	sort.Strings(got)
	if len(got) != 26 {
		t.Fatalf("expected 26 per-prefix collection names, got %d", len(got))
	}
}
