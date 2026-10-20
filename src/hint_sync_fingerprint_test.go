package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// captureHintRPC counts wire calls through the sync's RPC seam.
func captureHintRPC(h *Host, t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := h.replaceHintRPC
	h.replaceHintRPC = func(
		coll string, _ []branchkit.CollectionPutEntry, _ map[string]branchkit.FieldDisplay,
	) error {
		calls = append(calls, coll)
		return nil
	}
	t.Cleanup(func() { h.replaceHintRPC = orig })
	return &calls
}

func hintState(h *Host) {
	h.state = &PluginState{
		PluginID:            "browser",
		FrameSessions:       make(map[frameKey]*FrameSession),
		LastSyncFingerprint: make(map[string]uint64),
	}
}

// The platform already skips byte-identical RECORDS — but only after the full
// set has been marshaled, written to stdio, parsed, and diffed under the state
// lock. On the scroll path that happens per affected prefix, twice, per
// grammar batch, and the reproject on every focus change re-sends every
// prefix. The skip that costs nothing is the one that never reaches the wire.
func TestUnchangedHintSyncNeverReachesTheWire(t *testing.T) {
	h := newTestHost()
	hintState(h)
	calls := captureHintRPC(h, t)

	data := map[string]any{"bake": map[string]string{"id": "1", "letter": "ab"}}
	for i := 0; i < 3; i++ {
		if err := h.syncPerPrefix("browser", "arch", data); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 wire call for 3 identical syncs, got %d: %v", len(*calls), *calls)
	}

	// A real change still goes out.
	data["cane"] = map[string]string{"id": "2", "letter": "ac"}
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("a changed set must reach the wire, got %d calls", len(*calls))
	}
}

// A failed sync must NOT record its fingerprint, or a transient RPC error
// becomes permanent divergence — the one way this optimization could turn
// into a correctness bug.
func TestFailedHintSyncIsRetriedNotRemembered(t *testing.T) {
	h := newTestHost()
	hintState(h)
	var calls int
	failing := true
	orig := h.replaceHintRPC
	h.replaceHintRPC = func(
		_ string, _ []branchkit.CollectionPutEntry, _ map[string]branchkit.FieldDisplay,
	) error {
		calls++
		if failing {
			return errTestRPCFailed
		}
		return nil
	}
	t.Cleanup(func() { h.replaceHintRPC = orig })

	data := map[string]any{"bake": map[string]string{"id": "1"}}
	if err := h.syncPerPrefix("browser", "arch", data); err == nil {
		t.Fatal("expected the failing sync to surface its error")
	}
	failing = false
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the identical retry must re-send after a failure, got %d calls", calls)
	}
}

// `newData` is a Go map, so its iteration order changes between runs. An
// order-sensitive fingerprint would miss the skip almost every time and the
// optimization would silently do nothing — passing tests, no benefit.
func TestFingerprintIsIndependentOfEntryOrder(t *testing.T) {
	mk := func(ids ...string) []branchkit.CollectionPutEntry {
		out := make([]branchkit.CollectionPutEntry, 0, len(ids))
		for _, id := range ids {
			raw, _ := json.Marshal(map[string]string{"id": id})
			out = append(out, branchkit.CollectionPutEntry{ID: id, Payload: raw})
		}
		return out
	}
	if fingerprintEntries(mk("a", "b", "c")) != fingerprintEntries(mk("c", "a", "b")) {
		t.Fatal("the same set in a different order must fingerprint the same")
	}
	if fingerprintEntries(mk("a", "b")) == fingerprintEntries(mk("a", "b", "c")) {
		t.Fatal("a different set must fingerprint differently")
	}
	// Payload changes count, not just ids.
	changed := mk("a")
	changed[0].Payload = json.RawMessage(`{"id":"a","v":2}`)
	if fingerprintEntries(mk("a")) == fingerprintEntries(changed) {
		t.Fatal("a changed payload must fingerprint differently")
	}
}

// The fingerprint is a claim about what the PLATFORM holds. The disconnect
// path empties those collections directly (clearHintsState deletes records
// without going through the sync), so a surviving claim would make the next
// sync skip against records that no longer exist.
func TestDisconnectResetClearsFingerprints(t *testing.T) {
	h := newTestHost()
	hintState(h)
	calls := captureHintRPC(h, t)

	data := map[string]any{"bake": map[string]string{"id": "1"}}
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	// What the disconnect reset does to the fingerprint map.
	h.state.LastSyncFingerprint = make(map[string]uint64)

	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("after the reset the identical set must re-send, got %d calls", len(*calls))
	}
}

var errTestRPCFailed = rpcTestError("rpc failed")

type rpcTestError string

func (e rpcTestError) Error() string { return string(e) }

// A replace that moves only payloads (an element re-id'd, re-lettered, a
// strict flag flipped) leaves the words the recognizer can hear unchanged, so
// it must reach the wire but must not mark the vocabulary for a commit.
// A changed ID set must, once.
func TestOnlyASpokenKeyChangeDirtiesTheVocabulary(t *testing.T) {
	h := newTestHost()
	hintState(h)
	calls := captureHintRPC(h, t)

	data := map[string]any{"bake": map[string]string{"id": "1"}}
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if !h.takeHintVocabularyDirty() {
		t.Fatal("the first sync of a collection must mark the vocabulary dirty")
	}
	if h.takeHintVocabularyDirty() {
		t.Fatal("taking the mark must clear it")
	}

	data["bake"] = map[string]string{"id": "2"}
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("a payload change must still reach the wire, got %d calls", len(*calls))
	}
	if h.takeHintVocabularyDirty() {
		t.Error("a payload-only change must not mark the vocabulary dirty")
	}

	data["cane"] = map[string]string{"id": "3"}
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if !h.takeHintVocabularyDirty() {
		t.Error("a new spoken key must mark the vocabulary dirty")
	}

	delete(data, "cane")
	if err := h.syncPerPrefix("browser", "arch", data); err != nil {
		t.Fatal(err)
	}
	if !h.takeHintVocabularyDirty() {
		t.Error("a removed spoken key must mark the vocabulary dirty")
	}
}

// A failed sync changed nothing the platform holds, so it must not claim a
// key change either.
func TestFailedHintSyncDoesNotDirtyTheVocabulary(t *testing.T) {
	h := newTestHost()
	hintState(h)
	orig := h.replaceHintRPC
	h.replaceHintRPC = func(string, []branchkit.CollectionPutEntry, map[string]branchkit.FieldDisplay) error {
		return errTestRPCFailed
	}
	t.Cleanup(func() { h.replaceHintRPC = orig })
	_ = h.syncPerPrefix("browser", "arch", map[string]any{"bake": map[string]string{"id": "1"}})
	if h.takeHintVocabularyDirty() {
		t.Error("a failed sync must not mark the vocabulary dirty")
	}
}

// fakeHintStore stands in for the platform's hint collections: what the
// replace seam last wrote, read back through the seed's list seam. Payloads
// are stored re-indented with their keys reversed, so the seed has to
// canonicalise rather than compare the platform's bytes to ours.
type fakeHintStore map[string][]branchkit.CollectionRecord

func (s fakeHintStore) install(h *Host, t *testing.T) {
	t.Helper()
	origReplace, origList, origLive := h.replaceHintRPC, h.listHintRecords, h.livePrefixes
	h.replaceHintRPC = func(coll string, entries []branchkit.CollectionPutEntry, _ map[string]branchkit.FieldDisplay) error {
		recs := make([]branchkit.CollectionRecord, 0, len(entries))
		for _, e := range entries {
			var m map[string]any
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				t.Fatalf("payload: %v", err)
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Sort(sort.Reverse(sort.StringSlice(keys)))
			var b strings.Builder
			b.WriteString("{\n")
			for i, k := range keys {
				kv, _ := json.Marshal(k)
				vv, _ := json.Marshal(m[k])
				fmt.Fprintf(&b, "  %s: %s", kv, vv)
				if i < len(keys)-1 {
					b.WriteString(",")
				}
				b.WriteString("\n")
			}
			b.WriteString("}")
			recs = append(recs, branchkit.CollectionRecord{ID: e.ID, Payload: json.RawMessage(b.String())})
		}
		if len(recs) == 0 {
			delete(s, coll)
		} else {
			s[coll] = recs
		}
		return nil
	}
	h.listHintRecords = func(coll string) ([]branchkit.CollectionRecord, error) { return s[coll], nil }
	h.livePrefixes = func(pluginID string) (map[string]bool, error) {
		out := map[string]bool{}
		for name := range s {
			if word, ok := hintPrefixOfCollection(pluginID, name); ok {
				out[word] = true
			}
		}
		return out, nil
	}
	t.Cleanup(func() { h.replaceHintRPC, h.listHintRecords, h.livePrefixes = origReplace, origList, origLive })
}

// After a plugin restart under a live platform, the first syncs of state the
// platform already holds must not reach the wire or cost a vocabulary commit:
// the fingerprints are seeded from the collections (every boot used to
// re-send every hint collection).
func TestSeededFingerprintsSkipWhatThePlatformHolds(t *testing.T) {
	store := fakeHintStore{}
	arch := map[string]any{
		"bake": map[string]string{"id": "1", "letter": "ab", "codeword": "arch bake", "frame_id": "0", "type": "link"},
		"cane": map[string]string{"id": "2", "letter": "ac", "codeword": "arch cane", "frame_id": "0", "type": "button"},
	}
	strict := map[string]any{"bake": arch["bake"]}
	prefixes := map[string]any{"arch": map[string]string{"codeword": "arch", "letter": "a"}}

	before := newTestHost()
	hintState(before)
	store.install(before, t)
	if err := before.syncPerPrefix("browser", "arch", arch); err != nil {
		t.Fatal(err)
	}
	if err := before.syncPerPrefixStrict("browser", "arch", strict); err != nil {
		t.Fatal(err)
	}
	if err := before.syncPrefixList("browser", prefixes); err != nil {
		t.Fatal(err)
	}

	// The restarted process: no fingerprints, the platform's records intact.
	after := newTestHost()
	hintState(after)
	store.install(after, t)
	after.seedHintSyncFingerprints("browser")
	if after.takeHintVocabularyDirty() {
		t.Error("seeding must not mark the vocabulary dirty: the platform already has those words")
	}
	var wire []string
	inner := after.replaceHintRPC
	after.replaceHintRPC = func(c string, e []branchkit.CollectionPutEntry, r map[string]branchkit.FieldDisplay) error {
		wire = append(wire, c)
		return inner(c, e, r)
	}
	if err := after.syncPerPrefix("browser", "arch", arch); err != nil {
		t.Fatal(err)
	}
	if err := after.syncPerPrefixStrict("browser", "arch", strict); err != nil {
		t.Fatal(err)
	}
	if err := after.syncPrefixList("browser", prefixes); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 0 {
		t.Errorf("syncs of what the platform already holds reached the wire: %v", wire)
	}
	if after.takeHintVocabularyDirty() {
		t.Error("an unchanged key set after the seed must not mark the vocabulary dirty")
	}

	// A genuine change still goes out.
	arch["bake"] = map[string]string{"id": "9", "letter": "ab", "codeword": "arch bake", "frame_id": "0", "type": "link"}
	if err := after.syncPerPrefix("browser", "arch", arch); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 {
		t.Errorf("a changed payload after the seed must reach the wire, got %v", wire)
	}
}

// The seed only fills names no real sync has claimed: a sync that landed
// while the seed was reading describes the platform more recently than the
// seed's read, and overwriting it could make a later sync skip against
// records the platform no longer holds.
func TestSeedNeverOverwritesASyncClaim(t *testing.T) {
	store := fakeHintStore{}
	h := newTestHost()
	hintState(h)
	store.install(h, t)
	if err := h.syncPerPrefix("browser", "arch", map[string]any{"bake": map[string]string{"id": "1"}}); err != nil {
		t.Fatal(err)
	}
	name := hintCollection("browser", "arch")
	claimed := h.state.LastSyncFingerprint[name]
	store[name] = []branchkit.CollectionRecord{{ID: "zzz", Payload: json.RawMessage(`{"id":"7"}`)}}
	h.seedHintSyncFingerprints("browser")
	if got := h.state.LastSyncFingerprint[name]; got != claimed {
		t.Error("the seed overwrote a fingerprint a real sync had recorded")
	}
}

// End to end through the handler: the commit is scheduled for a frame that
// changes the codeword set and not for one that changes only payloads.
func TestGrammarFrameCommitsVocabularyOnlyWhenCodewordsChange(t *testing.T) {
	h, _, _ := newDriftHost(t)
	// The drift host stubs the syncs; put the real replace path back, with
	// only the wire stubbed.
	h.syncPerPrefix = h.syncPerPrefixDefault
	h.syncPerPrefixStrict = h.syncPerPrefixStrictDefault
	h.syncPrefixList = h.syncPrefixListDefault
	calls := captureHintRPC(h, t)

	pending := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		timer := h.state.VocabularyCommitTimer
		if timer == nil {
			return false
		}
		timer.Stop()
		h.state.VocabularyCommitTimer = nil
		return true
	}
	_ = h.takeHintVocabularyDirty() // whatever the setup synced

	putFrame(t, h, frameReq(0, "arch bake", "zoo check"))
	if !pending() {
		t.Fatal("a frame introducing codewords must schedule a vocabulary commit")
	}

	wrote := len(*calls)
	req := frameReq(0, "arch bake", "zoo check")
	for i := range req.Elements {
		req.Elements[i].ID += 100 // same codewords, re-id'd elements
	}
	putFrame(t, h, req)
	if len(*calls) == wrote {
		t.Fatal("precondition: the payload change must reach the wire")
	}
	if pending() {
		t.Error("a frame that changed only payloads scheduled a vocabulary commit")
	}

	putFrame(t, h, frameReq(0, "arch bake", "zoo check", "arch deck"))
	if !pending() {
		t.Error("a frame adding a codeword must schedule a vocabulary commit")
	}
}

// A frame refused element by element logs one summary line per reason, not
// one line per element (it was 16,660 lines a day from resends of
// letter-token frames).
func TestHintDropsSummariseByReason(t *testing.T) {
	var d hintDrops
	for _, cw := range []string{"f s", "f d", "f f", "d f", "s f", "a f", "d a"} {
		d.add("codeword_not_in_alphabet", cw)
	}
	d.add("invalid_codeword_parts", "arch")
	if len(d.reasons) != 2 || d.reasons[0] != "codeword_not_in_alphabet" {
		t.Fatalf("reasons = %v, want first-seen order of 2", d.reasons)
	}
	if n := len(d.byReason["codeword_not_in_alphabet"]); n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
	d.log("browser", 1, 0) // must not panic on a sample longer than the cap
}
