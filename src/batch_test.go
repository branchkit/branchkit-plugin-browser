package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// validateGrammarElement is the pure half of the grammar frame handler;
// the RPC half (plugin.Put) needs a platform connection and is exercised
// by integration tests, not here.

func TestValidateGrammarElementValid(t *testing.T) {
	e := GrammarElement{ID: 7, Type: "button", Codeword: "arch bake"}
	prefix, suffix, payload, reason := validateGrammarElement(e, 0, testCodewords)
	if reason != "" {
		t.Fatalf("unexpected failure: %q", reason)
	}
	if prefix != "arch" || suffix != "bake" {
		t.Errorf("prefix/suffix = %q/%q, want arch/bake", prefix, suffix)
	}
	if payload["letter"] != "ab" {
		t.Errorf("payload.letter = %q, want ab", payload["letter"])
	}
	if payload["id"] != "7" || payload["frame_id"] != "0" {
		t.Errorf("payload.id/frame_id = %q/%q, want 7/0", payload["id"], payload["frame_id"])
	}
	if payload["codeword"] != "arch bake" || payload["type"] != "button" {
		t.Errorf("payload.codeword/type = %q/%q", payload["codeword"], payload["type"])
	}
}

func TestValidateGrammarElementEmptyCodeword(t *testing.T) {
	e := GrammarElement{Codeword: ""}
	_, _, _, reason := validateGrammarElement(e, 0, testCodewords)
	if reason != "empty_codeword" {
		t.Errorf("reason = %q, want empty_codeword", reason)
	}
	// Whitespace-only is also empty.
	e.Codeword = "   "
	_, _, _, reason = validateGrammarElement(e, 0, testCodewords)
	if reason != "empty_codeword" {
		t.Errorf("whitespace reason = %q, want empty_codeword", reason)
	}
}

func TestValidateGrammarElementSingleWordCodeword(t *testing.T) {
	e := GrammarElement{Codeword: "arch"}
	_, _, _, reason := validateGrammarElement(e, 0, testCodewords)
	if reason != "invalid_codeword_parts" {
		t.Errorf("reason = %q, want invalid_codeword_parts", reason)
	}
}

func TestValidateGrammarElementThreeWordCodeword(t *testing.T) {
	e := GrammarElement{Codeword: "arch bake check"}
	_, _, _, reason := validateGrammarElement(e, 0, testCodewords)
	if reason != "invalid_codeword_parts" {
		t.Errorf("reason = %q, want invalid_codeword_parts", reason)
	}
}

func TestValidateGrammarElementUnknownWord(t *testing.T) {
	e := GrammarElement{Codeword: "nonsense bake"}
	_, _, _, reason := validateGrammarElement(e, 0, testCodewords)
	if reason != "codeword_not_in_alphabet" {
		t.Errorf("reason = %q, want codeword_not_in_alphabet", reason)
	}
	e.Codeword = "arch nonsense"
	_, _, _, reason = validateGrammarElement(e, 0, testCodewords)
	if reason != "codeword_not_in_alphabet" {
		t.Errorf("reason = %q, want codeword_not_in_alphabet", reason)
	}
}

// ensureFrameSession allocates on a frame's first request and returns the
// same session after. The state mutex is held by the handler; these tests
// assume the caller holds it.

func TestEnsureFrameSessionFreshAllocates(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{FrameSessions: make(map[frameKey]*FrameSession)}
	key := frameKey{TabID: 1, FrameID: 0}
	s := h.ensureFrameSession(key)
	if s == nil {
		t.Fatal("expected non-nil session")
	}
	if s.Codewords == nil {
		t.Error("Codewords map should be allocated")
	}
	if h.state.FrameSessions[key] != s {
		t.Error("session should be stored in state.FrameSessions")
	}
}

func TestEnsureFrameSessionReusesExisting(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{FrameSessions: make(map[frameKey]*FrameSession)}
	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}
	first := h.ensureFrameSession(key)
	first.Codewords["arch bake"] = map[string]string{"letter": "ab"}

	second := h.ensureFrameSession(key)
	if second != first {
		t.Error("the same frame key must return the same FrameSession instance")
	}
	if len(second.Codewords) != 1 {
		t.Errorf("codewords should persist across requests, got %d", len(second.Codewords))
	}
}

// commitFrame drives commitGrammarFrame for one focused-source request.
func commitFrame(h *Host, key frameKey, elements ...GrammarElement) frameCommit {
	req := &GrammarFrameRequest{ConnID: key.ConnID, TabID: key.TabID, FrameID: key.FrameID, Elements: elements}
	adm := frameAdmission{
		session: h.ensureFrameSession(key), pluginID: "browser",
		projConn: key.ConnID, projTab: key.TabID, isFocusedSource: true,
	}
	return h.commitGrammarFrame(req, adm, testCodewords)
}

// A request IS the frame's state: what it omits is gone, and the prefixes the
// omitted entries lived under are flagged so the sync declares them without
// them. This is the whole delete path — there is no delete list to send.
func TestCommitGrammarFrameReplacesTheFrame(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)
	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}

	commitFrame(h, key,
		GrammarElement{ID: 7, Type: "button", Codeword: "arch bake", InStrictViewport: true},
		GrammarElement{ID: 8, Type: "link", Codeword: "zoo check"})
	commit := commitFrame(h, key, GrammarElement{ID: 7, Type: "button", Codeword: "arch bake"})

	sess := h.state.FrameSessions[key]
	if _, ok := sess.Codewords["arch bake"]; !ok {
		t.Error("a codeword the request carries must stay")
	}
	if _, ok := sess.Codewords["zoo check"]; ok {
		t.Error("a codeword the request omits must be gone")
	}
	if sess.StrictCodewords["arch bake"] {
		t.Error("strict must follow the latest request, not accumulate")
	}
	if !commit.affectedPrefixes["zoo"] || !commit.affectedPrefixes["arch"] {
		t.Errorf("old and new prefixes must both be affected, got %v", commit.affectedPrefixes)
	}

	_ = h.syncAffectedPrefixes("browser", commit)
	if arch := pushes["plugin.browser.hints_arch"]; len(arch) != 1 || arch["bake"] == nil {
		t.Errorf("arch desired state = %v, want exactly {bake}", arch)
	}
	if zoo, ok := pushes["plugin.browser.hints_zoo"]; !ok || len(zoo) != 0 {
		t.Errorf("zoo must be declared empty, got ok=%v %v", ok, zoo)
	}
	if strict, ok := pushes["plugin.browser.hints_arch_strict"]; !ok || len(strict) != 0 {
		t.Errorf("arch's strict companion must be declared empty, got ok=%v %v", ok, strict)
	}
}

// Re-sending the same state — after a service-worker restart, a reconnect, a
// focus return — must not write anything to the platform. The fingerprint skip
// in replaceHintCollection is what makes a full-state protocol as cheap as the
// delta one in the steady state; this pins it at the wire seam.
func TestCommitGrammarFrameResendWritesNothing(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession),
		LastSyncFingerprint: make(map[string]uint64)}
	calls := 0
	prev := h.replaceHintRPC
	h.replaceHintRPC = func(string, []branchkit.CollectionPutEntry, map[string]branchkit.FieldDisplay) error {
		calls++
		return nil
	}
	t.Cleanup(func() { h.replaceHintRPC = prev })
	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}
	elements := []GrammarElement{
		{ID: 7, Type: "button", Codeword: "arch bake", InStrictViewport: true},
		{ID: 8, Type: "link", Codeword: "zoo check"},
	}

	_ = h.syncAffectedPrefixes("browser", commitFrame(h, key, elements...))
	first := calls
	if first == 0 {
		t.Fatal("precondition: the first request must write")
	}
	_ = h.syncAffectedPrefixes("browser", commitFrame(h, key, elements...))
	if calls != first {
		t.Errorf("an identical request wrote %d more time(s), want 0", calls-first)
	}
}

// One frame's request never touches a sibling frame's entries, even under a
// shared prefix: the projection is rebuilt from every frame's session.
func TestCommitGrammarFrameLeavesSiblingsAlone(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)
	top := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}
	iframe := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 3}

	commitFrame(h, iframe, GrammarElement{ID: 1, Type: "link", Codeword: "arch check"})
	commit := commitFrame(h, top, GrammarElement{ID: 2, Type: "link", Codeword: "arch bake"})
	_ = h.syncAffectedPrefixes("browser", commit)
	arch := pushes["plugin.browser.hints_arch"]
	if arch["bake"] == nil || arch["check"] == nil {
		t.Fatalf("arch must carry both frames' suffixes, got %v", arch)
	}

	// The top frame empties: only its own entry leaves.
	_ = h.syncAffectedPrefixes("browser", commitFrame(h, top))
	arch = pushes["plugin.browser.hints_arch"]
	if arch["bake"] != nil || arch["check"] == nil {
		t.Errorf("after the top frame empties, arch = %v, want exactly {check}", arch)
	}
}

// Invalid elements are reported and left out; the rest of the request lands.
func TestCommitGrammarFrameReportsInvalidElements(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}
	commit := commitFrame(h, key,
		GrammarElement{ID: 1, Type: "link", Codeword: "arch bake"},
		GrammarElement{ID: 2, Type: "link", Codeword: "nonsense bake"})
	if len(commit.failed) != 1 || commit.failed[0].Reason != "codeword_not_in_alphabet" {
		t.Errorf("failed = %v, want one codeword_not_in_alphabet", commit.failed)
	}
	if got := len(h.state.FrameSessions[key].Codewords); got != 1 {
		t.Errorf("session holds %d codewords, want 1", got)
	}
}

// testLiveCollections stands in for the platform's storage while the sync
// helpers are stubbed: the set of collection names last synced NON-empty.
// The livePrefixes double derives its answer from this through the same
// hintPrefixOfCollection the production path uses, so the double cannot claim
// a footprint production wouldn't — in particular a base collection with
// records keeps its prefix live even when the strict companion is empty,
// which a per-prefix bool would have gotten wrong (it did, once).
//
// Installed and torn down by withTestPushFn; seed via seedLivePrefix for a
// test that needs a prefix already live before the first sync.
var testLiveCollections map[string]bool

func recordLiveCollection(name string, live bool) {
	if testLiveCollections == nil {
		return
	}
	if live {
		testLiveCollections[name] = true
	} else {
		delete(testLiveCollections, name)
	}
}

func seedLivePrefix(pluginID, prefix string) {
	recordLiveCollection(hintCollection(pluginID, prefix), true)
}

// recordPushes captures sync calls keyed by collection name. Use with
// withTestPushFn to stub syncPerPrefix, syncPerPrefixStrict and
// syncPrefixList. The recorder snapshots the desired-state map each helper
// receives. These tests assert sync shape — the underlying SDK call's role
// propagation is covered separately.
func recordPushes() (map[string]map[string]any, func(string, map[string]any, map[string]string) error) {
	recorded := make(map[string]map[string]any)
	stub := func(name string, data map[string]any, _ map[string]string) error {
		// Take a copy so later mutations to data don't change the
		// recorded snapshot (matters since the handler reuses maps).
		snapshot := make(map[string]any, len(data))
		for k, v := range data {
			snapshot[k] = v
		}
		recorded[name] = snapshot
		return nil
	}
	return recorded, stub
}

// The collision the conn-scoped key fixes: two
// browsers sharing a tab id — Firefox numbers from 1, a second profile
// or fork does too — used to alias into ONE (tab, frame) key, and each
// browser's batches destructively thrashed the other's session via the
// old conn-change branch. With the conn in the key they simply coexist.
func TestSameTabIDAcrossConnsCoexists(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	a := h.ensureFrameSession(frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0})
	a.Codewords["arch bake"] = map[string]string{"id": "1"}
	b := h.ensureFrameSession(frameKey{ConnID: "conn-2", TabID: 1, FrameID: 0})
	b.Codewords["zoo cat"] = map[string]string{"id": "2"}

	if len(pushes) != 0 {
		t.Errorf("coexisting sessions must not trigger any cleanup sync, got %v", pushes)
	}
	if len(a.Codewords) != 1 || len(b.Codewords) != 1 {
		t.Errorf("both browsers' sessions must survive intact, got %d/%d", len(a.Codewords), len(b.Codewords))
	}
	// Projection stays conn-scoped: each browser sees only its own grammar.
	conn1State, _ := h.buildTabPrefixState("conn-1", 1)
	if _, ok := conn1State["zoo"]; ok {
		t.Error("conn-2's codeword leaked into conn-1's projection")
	}
	conn2State, _ := h.buildTabPrefixState("conn-2", 1)
	if _, ok := conn2State["arch"]; ok {
		t.Error("conn-1's codeword leaked into conn-2's projection")
	}
}

func TestConnCleanupPushFailureLogsButContinues(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	calls := 0
	withTestPushFn(h, t, func(_ string, _ map[string]any, _ map[string]string) error {
		calls++
		return errors.New("transient")
	})

	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}
	first := h.ensureFrameSession(key)
	first.Codewords["arch bake"] = map[string]string{}
	first.Codewords["zoo cat"] = map[string]string{}

	// Conn cleanup (SSE disconnect path) pushes one per affected prefix
	// (arch, zoo) plus the prefix list. All fail but must not panic.
	_ = h.cleanupConnSessionsLocked("browser", "conn-1", "test")
	if calls < 2 {
		t.Errorf("expected at least 2 push attempts (one per prefix), got %d", calls)
	}
}

func TestCleanupTabSessionsLockedRebuildsEveryAffectedPrefix(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	// Two frames in tab 1, one frame in tab 2.
	s1f0 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 0})
	s1f0.Codewords["arch bake"] = map[string]string{}
	s1f1 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 1})
	s1f1.Codewords["zoo cat"] = map[string]string{}
	s2 := h.ensureFrameSession(frameKey{TabID: 2, FrameID: 0})
	s2.Codewords["jake dog"] = map[string]string{}

	n := h.cleanupTabSessionsLocked("browser", "", 1, "test")
	if n != 2 {
		t.Errorf("expected 2 codewords cleared from tab 1, got %d", n)
	}

	// Tab 1's frames removed; tab 2 untouched.
	if _, exists := h.state.FrameSessions[frameKey{TabID: 1, FrameID: 0}]; exists {
		t.Error("tab 1 frame 0 session should be removed")
	}
	if _, exists := h.state.FrameSessions[frameKey{TabID: 1, FrameID: 1}]; exists {
		t.Error("tab 1 frame 1 session should be removed")
	}
	if _, exists := h.state.FrameSessions[frameKey{TabID: 2, FrameID: 0}]; !exists {
		t.Error("tab 2 frame 0 session must be preserved")
	}

	// arch and zoo should have been pushed empty (only tab-1 frames
	// had them). jake should NOT have been pushed because tab 2 is
	// untouched.
	if _, ok := pushes["plugin.browser.hints_arch"]; !ok {
		t.Error("expected empty push to plugin.browser.hints_arch")
	}
	if _, ok := pushes["plugin.browser.hints_zoo"]; !ok {
		t.Error("expected empty push to plugin.browser.hints_zoo")
	}
	if _, ok := pushes["plugin.browser.hints_jake"]; ok {
		t.Error("plugin.browser.hints_jake should not be pushed (tab 2 untouched)")
	}
}

func TestCleanupFrameSessionLockedOnEmptySession(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	calls := 0
	withTestPushFn(h, t, func(_ string, _ map[string]any, _ map[string]string) error {
		calls++
		return nil
	})

	key := frameKey{TabID: 1, FrameID: 0}
	session := h.ensureFrameSession(key)
	n := h.cleanupFrameSessionLocked("browser", key, session, "empty")
	if n != 0 {
		t.Errorf("expected 0 codewords cleared for empty session, got %d", n)
	}
	if calls != 0 {
		t.Errorf("expected 0 push calls for empty session, got %d", calls)
	}
}

// Cross-frame timing tests.
//
// Verifies per-(tab, frame) session state isolation: frames in the
// same tab maintain independent sessions, frame-scoped cleanup
// doesn't bleed across siblings, and prior-session implicit-reset
// for one frame leaves siblings' tracked codewords intact.
//
// Concurrent-batch ordering (frame A's Put landing after frame B's
// even when claimed first) is acceptable per the investigation —
// codeword collisions are prevented by the per-tab label pool's
// withTabLock, so the worst case is per-batch latency variance,
// not corruption.

func TestCrossFrameSessionsAreIsolated(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	withTestPushFn(h, t, func(_ string, _ map[string]any, _ map[string]string) error { return nil })

	// Three frames in tab 1 with different session_ids — each gets
	// its own FrameSession that doesn't see the others' codewords.
	f0 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 0})
	f1 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 1})
	f2 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 2})

	f0.Codewords["arch bake"] = map[string]string{"id": "10"}
	f1.Codewords["zoo cat"] = map[string]string{"id": "20"}
	f2.Codewords["jake dog"] = map[string]string{"id": "30"}

	if _, ok := f0.Codewords["zoo cat"]; ok {
		t.Error("frame 0 must not see frame 1's codeword")
	}
	if _, ok := f1.Codewords["arch bake"]; ok {
		t.Error("frame 1 must not see frame 0's codeword")
	}
	if _, ok := f2.Codewords["arch bake"]; ok {
		t.Error("frame 2 must not see frame 0's codeword")
	}
}

func TestCleanupRebuildsPrefixWithRemainingSiblingState(t *testing.T) {
	h := newTestHost()
	// Two iframes in tab 1 both contribute "arch _" codewords. When
	// one frame is cleaned up, the per-prefix collection's push must
	// carry the OTHER frame's contribution — otherwise the dynamic-
	// collection REPLACE semantic would wipe the surviving entry.
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	f0 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 0})
	f0.Codewords["arch bake"] = map[string]string{"id": "10"}
	f1 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 1})
	f1.Codewords["arch check"] = map[string]string{"id": "20"}

	h.cleanupFrameSessionLocked("browser", frameKey{TabID: 1, FrameID: 1}, f1, "test")

	// plugin.browser.hints_arch should be pushed with f0's surviving "bake"
	// entry but NOT f1's cleaned "check" entry.
	arch, ok := pushes["plugin.browser.hints_arch"]
	if !ok {
		t.Fatalf("expected push to plugin.browser.hints_arch after cleanup, got %v", pushes)
	}
	if _, hasBake := arch["bake"]; !hasBake {
		t.Errorf("plugin.browser.hints_arch must keep f0's 'bake' after f1 cleanup, got %v", arch)
	}
	if _, hasCheck := arch["check"]; hasCheck {
		t.Errorf("plugin.browser.hints_arch must drop f1's 'check' after cleanup, got %v", arch)
	}
}

func TestImplicitResetRebuildsPrefixFromRemainingFrames(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	// Top frame + iframe both under session-A.
	top := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 0})
	top.Codewords["arch bake"] = map[string]string{}
	iframe := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 1})
	iframe.Codewords["zoo cat"] = map[string]string{}

	// The top frame's next request holds nothing. It must NOT wipe the
	// iframe's codeword from entity_cache.
	commit := commitFrame(h, frameKey{TabID: 1, FrameID: 0})
	_ = h.syncAffectedPrefixes("browser", commit)

	// arch was only in top frame → push should be empty.
	if archPush, ok := pushes["plugin.browser.hints_arch"]; !ok {
		t.Error("expected push to plugin.browser.hints_arch after top-frame reset")
	} else if len(archPush) != 0 {
		t.Errorf("plugin.browser.hints_arch should be empty after top-frame reset, got %v", archPush)
	}

	// zoo was in iframe only. Top-frame reset doesn't touch zoo's
	// prefix, so no push to plugin.browser.hints_zoo expected.
	if _, ok := pushes["plugin.browser.hints_zoo"]; ok {
		t.Error("plugin.browser.hints_zoo should NOT be pushed during top-frame reset (iframe untouched)")
	}

	// iframe session still intact.
	iframeAfter := h.state.FrameSessions[frameKey{TabID: 1, FrameID: 1}]
	if iframeAfter == nil {
		t.Errorf("iframe session must survive top-frame rescan, got %+v", iframeAfter)
	}
	if _, ok := iframeAfter.Codewords["zoo cat"]; !ok {
		t.Error("iframe's codeword must persist after top-frame rescan")
	}
}

func TestCrossFrameCodewordsCoexistInPerPrefixCollection(t *testing.T) {
	h := newTestHost()
	// Two frames contribute "arch _" codewords with different suffixes.
	// One frame's reset should rebuild plugin.browser.hints_arch carrying only
	// the surviving frame's suffix.
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	f0 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 0})
	f0.Codewords["arch bake"] = map[string]string{"id": "10"}
	f1 := h.ensureFrameSession(frameKey{TabID: 1, FrameID: 1})
	f1.Codewords["arch check"] = map[string]string{"id": "20"}

	// f0's next request no longer holds "arch bake".
	commit := commitFrame(h, frameKey{TabID: 1, FrameID: 0})
	_ = h.syncAffectedPrefixes("browser", commit)

	arch, ok := pushes["plugin.browser.hints_arch"]
	if !ok {
		t.Fatalf("expected push to plugin.browser.hints_arch after f0 reset, got pushes=%v", pushes)
	}
	if _, hasBake := arch["bake"]; hasBake {
		t.Errorf("f0's 'bake' should be gone after f0 reset, got %v", arch)
	}
	if _, hasCheck := arch["check"]; !hasCheck {
		t.Errorf("f1's 'check' should survive in plugin.browser.hints_arch, got %v", arch)
	}
}

// withTestPushFn swaps the syncPerPrefix and syncPrefixList helpers for a
// shared test stub and restores them after the test. The stub receives
// the collection name (per-prefix: `${pluginID}_hints_${prefix}`, list:
// `${pluginID}_hints_prefix`) and the desired-state map the production
// code computed before the diff/apply step. The third argument is left
// for signature compatibility with prior tests that asserted role
// propagation through the old pushFn; with the explicit Put/Delete
// primitive, roles are attached SDK-side on first sync only, so the
// existing tests pass nil and we keep the parameter unused.
//
// The stub mirrors the production helper's live-prefix index advance so
// callers that loop through multiple syncs see a coherent baseline (e.g.
// reprojectFocusedSourceLocked unions the prior index on a focus switch).
// The payload shadows the production helpers used to keep are gone — the
// platform diffs now — so the index (prefix → live?) is all there is to
// mirror.
func withTestPushFn(h *Host, t *testing.T, stub func(string, map[string]any, map[string]string) error) {
	t.Helper()
	prevSyncPerPrefix := h.syncPerPrefix
	prevSyncPerPrefixStrict := h.syncPerPrefixStrict
	prevSyncPrefixList := h.syncPrefixList
	prevLivePrefixes := h.livePrefixes
	testLiveCollections = make(map[string]bool)
	h.syncPerPrefix = func(pluginID, prefix string, newData map[string]any) error {
		name := hintCollection(pluginID, prefix)
		if err := stub(name, newData, nil); err != nil {
			return err
		}
		recordLiveCollection(name, len(newData) > 0)
		return nil
	}
	h.syncPerPrefixStrict = func(pluginID, prefix string, newData map[string]any) error {
		name := hintCollection(pluginID, prefix) + "_strict"
		if err := stub(name, newData, nil); err != nil {
			return err
		}
		recordLiveCollection(name, len(newData) > 0)
		return nil
	}
	h.syncPrefixList = func(pluginID string, newData map[string]any) error {
		return stub(hintCollection(pluginID, "prefix"), newData, nil)
	}
	h.livePrefixes = func(pluginID string) (map[string]bool, error) {
		out := make(map[string]bool, len(testLiveCollections))
		for name := range testLiveCollections {
			if word, ok := hintPrefixOfCollection(pluginID, name); ok {
				out[word] = true
			}
		}
		return out, nil
	}
	t.Cleanup(func() {
		h.syncPerPrefix = prevSyncPerPrefix
		h.syncPerPrefixStrict = prevSyncPerPrefixStrict
		h.syncPrefixList = prevSyncPrefixList
		h.livePrefixes = prevLivePrefixes
		testLiveCollections = nil
	})
}

func TestEnsureFrameSessionDistinctFramesDontCollide(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{FrameSessions: make(map[frameKey]*FrameSession)}
	keyA := frameKey{TabID: 1, FrameID: 0}
	keyB := frameKey{TabID: 1, FrameID: 1}
	keyC := frameKey{TabID: 2, FrameID: 0}

	a := h.ensureFrameSession(keyA)
	b := h.ensureFrameSession(keyB)
	c := h.ensureFrameSession(keyC)
	if a == b || b == c || a == c {
		t.Error("distinct frameKeys must yield distinct sessions")
	}
	if len(h.state.FrameSessions) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(h.state.FrameSessions))
	}
}

// buildTabPrefixState must scope to the queried connection so a stale session
// from another browser (e.g. a transient overlap during a focus switch)
// never bleeds into the global per-prefix namespace.
func TestBuildTabPrefixStateScopesByConn(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{FrameSessions: make(map[frameKey]*FrameSession)}

	chrome := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	chrome.Codewords["arch bake"] = map[string]string{"id": "1"}
	firefox := h.ensureFrameSession(frameKey{ConnID: "conn-firefox", TabID: 99, FrameID: 0})
	firefox.Codewords["arch cane"] = map[string]string{"id": "2"}

	chromeState, _ := h.buildTabPrefixState("conn-chrome", 1)
	if _, ok := chromeState["arch"]["bake"]; !ok {
		t.Error("Chrome's own codeword should be present")
	}
	if _, ok := chromeState["arch"]["cane"]; ok {
		t.Error("Firefox's codeword leaked into Chrome's prefix state")
	}

	firefoxState, _ := h.buildTabPrefixState("conn-firefox", 99)
	if _, ok := firefoxState["arch"]["cane"]; !ok {
		t.Error("Firefox's own codeword should be present")
	}
	if _, ok := firefoxState["arch"]["bake"]; ok {
		t.Error("Chrome's codeword leaked into Firefox's prefix state")
	}
}

// cleanupConnSessionsLocked must wipe only the losing browser's sessions
// on a browser→browser switch, leaving the other browser's grammar intact.
func TestCleanupConnSessionsLockedWipesOneBrowser(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	chrome := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	chrome.Codewords["arch bake"] = map[string]string{"id": "1"}
	firefox := h.ensureFrameSession(frameKey{ConnID: "conn-firefox", TabID: 99, FrameID: 0})
	firefox.Codewords["zoo cane"] = map[string]string{"id": "2"}

	cleared := h.cleanupConnSessionsLocked("browser", "conn-chrome", "browser_switch")
	if cleared != 1 {
		t.Errorf("expected 1 codeword cleared, got %d", cleared)
	}
	if _, ok := h.state.FrameSessions[frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0}]; ok {
		t.Error("Chrome's session should be removed after cleanup")
	}
	if _, ok := h.state.FrameSessions[frameKey{ConnID: "conn-firefox", TabID: 99, FrameID: 0}]; !ok {
		t.Error("Firefox's session must survive a Chrome-only cleanup")
	}
	// Chrome's prefix is pushed empty (wiped); Firefox's is untouched.
	if archPush, ok := pushes["plugin.browser.hints_arch"]; !ok || len(archPush) != 0 {
		t.Errorf("plugin.browser.hints_arch should be pushed empty after wipe, got ok=%v val=%v", ok, archPush)
	}
}

// Focused-source projection: the canonical per-prefix collections are built from the
// OS-focused source only. These lock in the read-time selection that replaced
// the write-time cross-browser gate.

// projectionSourceLocked returns the focused (conn, tab) when it's known, and
// falls back to the batch's own source only while the focus binding is still
// pending (lone-browser cold start).
func TestProjectsFocusedSourceOnly(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{FrameSessions: make(map[frameKey]*FrameSession)}

	// Focus bound: a background batch's own (conn, tab) is ignored in favour of
	// the focused source.
	h.state.FocusedConnID = "conn-fox"
	h.state.FocusedTabID = 99
	if conn, tab := h.projectionSourceLocked("conn-chrome", 1); conn != "conn-fox" || tab != 99 {
		t.Errorf("with focus bound, projection source = %q/%d, want conn-fox/99", conn, tab)
	}

	// Focus unbound (cold start): fall back to the supplied source so a lone
	// browser keeps projecting before the handshake binds.
	h.state.FocusedConnID = ""
	h.state.FocusedTabID = 0
	if conn, tab := h.projectionSourceLocked("conn-chrome", 1); conn != "conn-chrome" || tab != 1 {
		t.Errorf("with focus unbound, projection source = %q/%d, want conn-chrome/1 (fallback)", conn, tab)
	}
}

// A background browser's codewords sit in their own FrameSession (stored) but
// never reach the canonical collections — only the focused source projects.
func TestBackgroundBatchStoresButDoesNotProject(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	chrome := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	chrome.Codewords["arch bake"] = map[string]string{"id": "1"}
	fox := h.ensureFrameSession(frameKey{ConnID: "conn-fox", TabID: 99, FrameID: 0})
	fox.Codewords["arch cane"] = map[string]string{"id": "2"}

	// Firefox is focused; project.
	h.state.FocusedConnID = "conn-fox"
	h.state.FocusedTabID = 99
	h.reprojectFocusedSourceLocked("browser")

	arch, ok := pushes["plugin.browser.hints_arch"]
	if !ok {
		t.Fatalf("expected push to plugin.browser.hints_arch, got %v", pushes)
	}
	if _, hasCane := arch["cane"]; !hasCane {
		t.Errorf("focused Firefox's 'cane' must be projected, got %v", arch)
	}
	if _, hasBake := arch["bake"]; hasBake {
		t.Errorf("background Chrome's 'bake' must NOT be projected, got %v", arch)
	}
	// Chrome's batch is still stored — focus switching back to it would reproject.
	if _, stored := h.state.FrameSessions[frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0}]; !stored {
		t.Error("background Chrome's session must remain stored (not projected, not dropped)")
	}
}

// Switching focus rebuilds the canonical collections from the newly-focused
// source and wipes the prior source's stale prefixes (union with the prior
// push snapshot), so a tab/browser switch follows focus without a rescan.
func TestFocusChangeReprojects(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	chrome := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	chrome.Codewords["arch bake"] = map[string]string{"id": "1"}
	fox := h.ensureFrameSession(frameKey{ConnID: "conn-fox", TabID: 99, FrameID: 0})
	fox.Codewords["zoo cane"] = map[string]string{"id": "2"}

	// Chrome focused first.
	h.state.FocusedConnID = "conn-chrome"
	h.state.FocusedTabID = 1
	h.reprojectFocusedSourceLocked("browser")
	if arch := pushes["plugin.browser.hints_arch"]; len(arch) != 1 || arch["bake"] == nil {
		t.Fatalf("Chrome focus should project arch={bake}, got %v", arch)
	}

	// Focus moves to Firefox.
	h.state.FocusedConnID = "conn-fox"
	h.state.FocusedTabID = 99
	h.reprojectFocusedSourceLocked("browser")

	if zoo := pushes["plugin.browser.hints_zoo"]; len(zoo) != 1 || zoo["cane"] == nil {
		t.Errorf("after switch, Firefox should project zoo={cane}, got %v", zoo)
	}
	// Chrome's stale prefix is wiped, not stranded.
	if arch, ok := pushes["plugin.browser.hints_arch"]; !ok || len(arch) != 0 {
		t.Errorf("after switch, Chrome's arch prefix must be pushed empty, got ok=%v val=%v", ok, arch)
	}
	if live, _ := h.livePrefixes("browser"); live["arch"] {
		t.Errorf("arch should no longer hold records after the wipe")
	}
}

// A focus/tab change to a source with no stored scan data (a freshly
// focused/opened tab, or sessions a prior switch cleared) must NOT wipe the live
// per-prefix grammar — that drain-to-prefixes=0 blanks the hints until a rescan,
// the "say it again" window. The reproject only rebuilds when it has something
// to rebuild from.
func TestReprojectSkipsEmptySourceNoDrain(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
	}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)
	// "air" is already live when the reproject runs — the state the guard
	// below must not drain.
	seedLivePrefix("browser", "air")

	// Focused source has no FrameSessions for its tab → buildTabPrefixState empty.
	h.state.FocusedConnID = "conn-chrome"
	h.state.FocusedTabID = 7
	h.reprojectFocusedSourceLocked("browser")

	// Guard fires before any sync — the live entries are not wiped.
	if len(pushes) != 0 {
		t.Errorf("empty-source reproject must touch no collections, got pushes=%v", pushes)
	}
	if live, _ := h.livePrefixes("browser"); !live["air"] {
		t.Error("empty-source reproject drained the live per-prefix grammar")
	}
}

// The byte-identical-skip property these helpers used to implement locally
// (computePrefixDiff: "unchanged payloads emit no RPC") moved platform-side
// with the migration to collection.replace — it is the verb's `skipped`
// semantics now, pinned by the platform's
// `replace_skips_unchanged_and_writes_only_the_difference`. The tests that
// pinned the local copy went with it.

// Callers pass an empty map to mean "wipe every prior suffix". The sync must
// forward that as an explicit empty desired-state — an empty replace is how a
// scope is CLEARED, and short-circuiting it would strand the collection's
// records.
func TestSyncPerPrefixEmptyNewDataIsAnExplicitWipe(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser"}
	recorded, stub := recordPushes()
	withTestPushFn(h, t, stub)
	seedLivePrefix("browser", "each")
	if err := h.syncPerPrefix("browser", "each", map[string]any{}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got, ok := recorded["plugin.browser.hints_each"]
	if !ok {
		t.Fatalf("expected the wipe to reach plugin.browser.hints_each, got %v", recorded)
	}
	if len(got) != 0 {
		t.Errorf("expected empty desired-state, got %v", got)
	}
	if live, _ := h.livePrefixes("browser"); live["each"] {
		t.Error("a wiped prefix must no longer hold records")
	}
}

// Regression: the original silent-eviction scenario. Before the explicit-removal
// migration, a session_id change mid-scan cycled FrameSessions through:
//
//  1. Session A registered codewords across multiple prefixes
//     (drum/made, each/pit, each/air, gust/cap).
//  2. Session_id change triggered cleanup → cumulative-REPLACE push
//     of empty maps for affected prefixes → matcher silently lost
//     those codewords (badges stayed painted, voice broke).
//  3. Session B added codewords for OTHER prefixes that happened to
//     share the affected prefixes — the cumulative push of just B's
//     contributions REPLACED again, but the prefix-list still listed
//     every prefix from A, leaving a discoverable prefix with no
//     matchable suffix.
//
// Every removal is explicit now: cleanup syncs each affected prefix with an
// empty desired-state (a real replace-to-empty on the wire), and session B's
// syncs carry exactly B's data. The assertions run against what the sync seam
// RECEIVED — the desired states handed to the platform — plus which
// collections still hold our records afterwards.
func TestSessionCycleNoSilentEviction(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{
		PluginID:      "browser",
		FrameSessions: make(map[frameKey]*FrameSession),
	}
	recorded, stub := recordPushes()
	withTestPushFn(h, t, stub)

	key := frameKey{ConnID: "conn-1", TabID: 1, FrameID: 0}

	// Step 1: Session A populates four codewords across three prefixes.
	a := h.ensureFrameSession(key)
	a.Codewords["drum made"] = map[string]string{"id": "1", "letter": "dm"}
	a.Codewords["each pit"] = map[string]string{"id": "2", "letter": "ep"}
	a.Codewords["each air"] = map[string]string{"id": "3", "letter": "ea"}
	a.Codewords["gust cap"] = map[string]string{"id": "4", "letter": "gc"}

	// Cleanup that fires when a NEW scan begins with a different
	// session_id mid-stream. Under the old REPLACE protocol this silently
	// evicted A's codewords; now it must sync each affected prefix to an
	// explicit empty desired-state.
	h.cleanupFrameSessionLocked("browser", key, a, "session_id_changed")
	delete(h.state.FrameSessions, key)

	for _, prefix := range []string{"drum", "each", "gust"} {
		got, ok := recorded["plugin.browser.hints_"+prefix]
		if !ok {
			t.Errorf("after session-A cleanup, prefix %q must receive an explicit wipe", prefix)
			continue
		}
		if len(got) > 0 {
			t.Errorf("after session-A cleanup, prefix %q desired-state should be empty, got %v",
				prefix, got)
		}
	}
	if live, _ := h.livePrefixes("browser"); len(live) != 0 {
		t.Errorf("after session-A cleanup, no prefix should still hold records, got %v", live)
	}

	// Step 2: Session B starts and pushes ITS OWN codewords. The
	// previously-cleared codewords must NOT silently reappear, and
	// session B's additions must be present.
	b := h.ensureFrameSession(key)
	b.Codewords["drum made"] = map[string]string{"id": "10", "letter": "dm"}
	b.Codewords["kind ink"] = map[string]string{"id": "11", "letter": "ki"}

	conn, tab := h.projectionSourceLocked("conn-1", 1)
	allByPrefix, _ := h.buildTabPrefixState(conn, tab)
	for prefix := range map[string]bool{"drum": true, "kind": true} {
		data := allByPrefix[prefix]
		if data == nil {
			data = make(map[string]any)
		}
		if err := h.syncPerPrefix("browser", prefix, data); err != nil {
			t.Fatalf("syncPerPrefix(%s): %v", prefix, err)
		}
	}
	prefixData := buildPrefixListData(allByPrefix, h.snapshotCodewords())
	if err := h.syncPrefixList("browser", prefixData); err != nil {
		t.Fatalf("syncPrefixList: %v", err)
	}

	// Final invariants, against the last desired-state each collection saw:
	//   - drum has exactly {made} (re-added by session B).
	//   - kind has exactly {ink} (new from session B).
	//   - each + gust stay wiped (cleared with session A, not re-added).
	//   - only {drum, kind} still hold records.
	for prefix, wantSuffix := range map[string]string{"drum": "made", "kind": "ink"} {
		got := recorded["plugin.browser.hints_"+prefix]
		if len(got) != 1 || got[wantSuffix] == nil {
			t.Errorf("prefix %q desired-state = %v, want exactly {%s}", prefix, got, wantSuffix)
		}
	}
	for _, prefix := range []string{"each", "gust"} {
		if got := recorded["plugin.browser.hints_"+prefix]; len(got) > 0 {
			t.Errorf("prefix %q should stay wiped (cleared with session A), got %v", prefix, got)
		}
	}
	wantPrefixes := map[string]bool{"drum": true, "kind": true}
	if live, _ := h.livePrefixes("browser"); !mapsEqual(live, wantPrefixes) {
		t.Errorf("prefixes still holding records = %v, want %v", live, wantPrefixes)
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The focus-driven tab-level session end keeps sessions (deprojects only) so
// a focus return reprojects them with zero extension traffic — the fix for
// the tab-switch wipe / refocus delta asymmetry found by the grammar epoch
// tripwire (2026-06-12). The destructive cleanup remains for genuine
// endings (frame death, tab close).
func TestTabSwitchDeprojectsButKeepsSessions(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	tabA := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	tabA.Codewords["arch bake"] = map[string]string{"id": "1"}
	h.state.FocusedConnID = "conn-chrome"
	h.state.FocusedTabID = 1
	h.reprojectFocusedSourceLocked("browser")
	if _, ok := pushes["plugin.browser.hints_arch"]["bake"]; !ok {
		t.Fatalf("precondition: tab 1 projected, got %v", pushes)
	}

	// Focus moves to a fresh tab; the SW's session_end(tab_switch) deprojects.
	h.state.FocusedTabID = 2
	kept := h.deprojectTabSessionsLocked("browser", "conn-chrome", 1, "session_end_tab_tab_switch")
	if kept != 1 {
		t.Errorf("deproject reported %d codewords, want 1", kept)
	}
	sess := h.state.FrameSessions[frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0}]
	if sess == nil || len(sess.Codewords) != 1 {
		t.Fatal("session must SURVIVE a tab switch (keep-but-deproject)")
	}
	if !sess.SweptStale {
		t.Error("deprojected session should be parked SweptStale")
	}
	if archPush := pushes["plugin.browser.hints_arch"]; len(archPush) != 0 {
		t.Errorf("tab 1's entries must leave the collections on switch-away, got %v", archPush)
	}

	// Background long enough to cross the dormant TTL.
	sess.LastBatchAt = time.Now().Add(-2 * staleFrameSessionTTL)

	// Focus returns: revive + reproject restores the grammar — zero extension
	// traffic, despite the TTL.
	h.state.FocusedTabID = 1
	if revived := h.reviveTabSessionsLocked("conn-chrome", 1); revived != 1 {
		t.Errorf("revive touched %d sessions, want 1", revived)
	}
	h.reprojectFocusedSourceLocked("browser")
	if _, ok := pushes["plugin.browser.hints_arch"]["bake"]; !ok {
		t.Errorf("switch-back must reproject the kept session, got %v", pushes["plugin.browser.hints_arch"])
	}
	if sess.SweptStale {
		t.Error("revival must clear SweptStale")
	}
}

// A session_end(tab_switch) that races ahead of the focus recompute (source
// still pointing at the tab) must not blank the live projection.
func TestTabSwitchDeprojectDefersWhileStillProjected(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)

	tabA := h.ensureFrameSession(frameKey{ConnID: "conn-chrome", TabID: 1, FrameID: 0})
	tabA.Codewords["arch bake"] = map[string]string{"id": "1"}
	h.state.FocusedConnID = "conn-chrome"
	h.state.FocusedTabID = 1
	h.reprojectFocusedSourceLocked("browser")
	before := len(pushes["plugin.browser.hints_arch"])

	h.deprojectTabSessionsLocked("browser", "conn-chrome", 1, "session_end_tab_tab_switch")
	if after := len(pushes["plugin.browser.hints_arch"]); after != before {
		t.Errorf("deferred deproject must not touch the live projection (before=%d after=%d)", before, after)
	}
	if len(tabA.Codewords) != 1 {
		t.Error("session must survive the deferred deproject")
	}
}

// postFrameSessionEnd drives handleHintsSessionEndPost with a frame-scoped
// end, the way the SW's liveness onDisconnect does.
func postFrameSessionEnd(h *Host, t *testing.T, connID string, tabID, frameID int, docID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"conn_id": connID, "reason": "frame_liveness_disconnect",
		"tab_id": tabID, "frame_id": frameID, "doc_id": docID,
	})
	req := httptest.NewRequest(http.MethodPost, "/hints/session_end", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	serveRoute(h, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session_end status = %d", rec.Code)
	}
}

// The doc-identity fence: a frame-scoped session_end names the document it
// is about (doc_id off the liveness port name) and may only tear down that
// document's session. A navigation's liveness disconnect delivered late
// (4.5s observed on Firefox) lands after the successor document's batches
// have repopulated the same (conn, tab, frame) key — unfenced, it destroyed
// 262 live codewords while the successor's delta-sync shadow still believed
// them committed: painted badges, voice-dead (2026-07-24 wikipedia ZY repro).
func TestFrameSessionEndDocMismatchIgnored(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)

	key := frameKey{ConnID: "conn-1", TabID: 36, FrameID: 0}
	sess := h.ensureFrameSession(key)
	sess.DocID = "doc-successor"
	sess.Codewords["arch bake"] = map[string]string{"id": "1"}

	// Predecessor's late disconnect: same key, different document.
	postFrameSessionEnd(h, t, "conn-1", 36, 0, "doc-predecessor")
	if got := h.state.FrameSessions[key]; got == nil || len(got.Codewords) != 1 {
		t.Fatal("mismatched-doc session_end must not tear down the successor's session")
	}

	// The document's OWN end still tears it down.
	postFrameSessionEnd(h, t, "conn-1", 36, 0, "doc-successor")
	if h.state.FrameSessions[key] != nil {
		t.Fatal("matching-doc session_end must clean up the session")
	}
}

// A session that has never seen a batch has no DocID to discriminate on —
// it stays tearable (it has nothing to lose). Guards against the fence
// accidentally requiring identity where none can exist yet.
func TestFrameSessionEndEmptyDocStillTearsDown(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser", FrameSessions: make(map[frameKey]*FrameSession)}
	_, stub := recordPushes()
	withTestPushFn(h, t, stub)

	key := frameKey{ConnID: "conn-1", TabID: 5, FrameID: 2}
	sess := h.ensureFrameSession(key)
	sess.Codewords["arch bake"] = map[string]string{"id": "1"}

	postFrameSessionEnd(h, t, "conn-1", 5, 2, "doc-any")
	if h.state.FrameSessions[key] != nil {
		t.Fatal("unstamped session must remain tearable")
	}
}

// A reply's lists are [] on the wire, never null — for every element type,
// through the one generic helper.
func TestNonNilListsEncodeEmpty(t *testing.T) {
	for _, got := range []any{nonNil([]string(nil)), nonNil([]GrammarFailure(nil))} {
		b, err := json.Marshal(got)
		if err != nil || string(b) != "[]" {
			t.Fatalf("nonNil(nil) encodes %s (%v), want []", b, err)
		}
	}
	if s := []string{"a"}; &nonNil(s)[0] != &s[0] {
		t.Fatal("a non-nil list must come back as is")
	}
}
