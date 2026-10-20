package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Grammar and hints-tag drift: ways the plugin's grammar and tag state
// drifted from what the extension shows.

const driftTab = 7

// newDriftHost is a routing host with Chrome connected and in front, its
// focused window and tab known, and the collection syncs recorded.
func newDriftHost(t *testing.T) (*Host, map[string]map[string]any, *seamLog) {
	t.Helper()
	h, log := newRoutingHost(t)
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)
	h.putHintsTag = func(string) error { log.add("put_hints"); return nil }
	connectBrowser(h, "chrome", chromeBundle)
	h.mu.Lock()
	h.state.conn("chrome").ActiveTab = driftTab
	h.state.conn("chrome").FocusedWindow = 1
	h.state.HintVisibility = "always"
	h.mu.Unlock()
	focusApp(h, chromeBundle)
	if h.state.FocusedConnID != "chrome" || h.state.FocusedTabID != driftTab {
		t.Fatalf("focus = %q/%d, want chrome/%d", h.state.FocusedConnID, h.state.FocusedTabID, driftTab)
	}
	return h, pushes, log
}

func frameReq(frame int, codewords ...string) GrammarFrameRequest {
	req := GrammarFrameRequest{
		ConnID: "chrome", TabID: driftTab, FrameID: frame, WindowID: 1, Visible: true,
		HintVisibility: "always",
	}
	for i, cw := range codewords {
		req.Elements = append(req.Elements, GrammarElement{
			ID: i + 1, Type: "link", Codeword: cw, InStrictViewport: true,
		})
	}
	return req
}

func putFrame(t *testing.T, h *Host, req GrammarFrameRequest) GrammarFrameResponse {
	t.Helper()
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequest(http.MethodPut, "/grammar/frame", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("/grammar/frame status = %d: %s", w.Code, w.Body.String())
	}
	var resp GrammarFrameResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

func strictSuffixes(pushes map[string]map[string]any, prefix string) map[string]bool {
	out := map[string]bool{}
	for suffix := range pushes[hintCollection("browser", prefix)+"_strict"] {
		out[suffix] = true
	}
	return out
}

// A static article's top frame goes quiet past the TTL; an ad iframe then
// sends one batch. The top frame's codewords must stay matchable — through
// the batch's own sync and through the sweeper.
func TestIdleTopFrameKeepsCodewordsWhenIframeBatches(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	putFrame(t, h, frameReq(0, "arch bake"))
	h.mu.Lock()
	h.state.FrameSessions[frameKey{ConnID: "chrome", TabID: driftTab, FrameID: 0}].LastBatchAt =
		time.Now().Add(-2 * staleFrameSessionTTL)
	h.mu.Unlock()

	putFrame(t, h, frameReq(5, "arch check"))
	if got := strictSuffixes(pushes, "arch"); !got["bake"] || !got["check"] {
		t.Fatalf("after the iframe batch hints_arch_strict = %v, want bake (idle top frame) and check", got)
	}

	h.sweepStaleFrameSessionsOnce(time.Now())
	if got := strictSuffixes(pushes, "arch"); !got["bake"] {
		t.Errorf("the sweeper dropped the idle top frame: hints_arch_strict = %v", got)
	}

	// A dormant IFRAME still ages out.
	h.mu.Lock()
	h.state.FrameSessions[frameKey{ConnID: "chrome", TabID: driftTab, FrameID: 5}].LastBatchAt =
		time.Now().Add(-2 * staleFrameSessionTTL)
	h.mu.Unlock()
	h.sweepStaleFrameSessionsOnce(time.Now())
	if got := strictSuffixes(pushes, "arch"); got["check"] || !got["bake"] {
		t.Errorf("a dormant iframe must age out and the top frame stay: hints_arch_strict = %v", got)
	}
}

// Every service-worker restart mints a new conn_id; the old connection's
// sessions must go with its stream, and what they projected must drain.
func TestDisconnectDestroysTheConnectionsSessions(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	putFrame(t, h, frameReq(0, "arch bake"))
	if got := strictSuffixes(pushes, "arch"); !got["bake"] {
		t.Fatalf("setup: bake not projected, got %v", got)
	}

	h.disconnectSSEClient("browser", "chrome", h.state.peek("chrome").Client)

	for key := range h.state.FrameSessions {
		if key.ConnID == "chrome" {
			t.Errorf("session %+v outlived its connection", key)
		}
	}
	if got := strictSuffixes(pushes, "arch"); len(got) != 0 {
		t.Errorf("the dead connection's codewords are still matchable: %v", got)
	}
}

// A same-conn_id reconnect is the SAME connection: its sessions stay.
func TestSameConnIDReconnectKeepsSessions(t *testing.T) {
	h, _, _ := newDriftHost(t)
	putFrame(t, h, frameReq(0, "arch bake"))
	oldClient := h.state.peek("chrome").Client
	connectBrowser(h, "chrome", chromeBundle)

	h.disconnectSSEClient("browser", "chrome", oldClient)

	if _, ok := h.state.FrameSessions[frameKey{ConnID: "chrome", TabID: driftTab, FrameID: 0}]; !ok {
		t.Error("the old stream's cleanup destroyed the reconnected connection's sessions")
	}
}

// Frame A commits, frame B commits and publishes, THEN A publishes. A's
// publish must not remove what B just stored.
func TestLatePublishDoesNotDropAnotherFramesCodewords(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	reqA := frameReq(1, "arch bake")
	admA := h.admitGrammarFrame(&reqA)
	commitA := h.commitGrammarFrame(&reqA, admA, testCodewords)

	respB := putFrame(t, h, frameReq(2, "arch check"))
	if respB.Result != "ok" || len(respB.Failed) != 0 || respB.CommittedCodewords != 1 {
		t.Fatalf("B's batch: %+v", respB)
	}

	h.syncAffectedPrefixes("browser", commitA)
	h.syncTabPrefixList("browser", commitA, testCodewords)

	if got := strictSuffixes(pushes, "arch"); !got["bake"] || !got["check"] {
		t.Errorf("after A's late publish hints_arch_strict = %v, want bake AND check", got)
	}
}

// The same, with the handlers really concurrent — for -race, and for the
// ordering: once both batches have returned, the published state holds both.
func TestConcurrentFrameBatchesBothSurvive(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	suffixes := []string{"bake", "check", "deck", "egg", "food", "glad", "half", "iron"}
	for round := 0; round < 40; round++ {
		a, b := suffixes[round%len(suffixes)], suffixes[(round+3)%len(suffixes)]
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); putFrame(t, h, frameReq(1, "arch "+a)) }()
		go func() { defer wg.Done(); putFrame(t, h, frameReq(2, "arch "+b)) }()
		wg.Wait()
		h.mu.Lock()
		want, _ := h.buildTabPrefixState("chrome", driftTab)
		got := strictSuffixes(pushes, "arch")
		h.mu.Unlock()
		for suffix := range want["arch"] {
			if !got[suffix] {
				t.Fatalf("round %d: session holds arch %s but the published collection lacks it (%v)", round, suffix, got)
			}
		}
	}
}

// A failed Put must leave the gate idle, so the next arm retries it.
func TestHintGateRetriesAFailedPut(t *testing.T) {
	h, _, _ := newDriftHost(t)
	putFrame(t, h, frameReq(0, "arch bake"))
	h.state.HintGate.markCleared("test")

	var calls int32
	h.putHintsTag = func(string) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errors.New("platform busy")
		}
		return nil
	}
	h.applyDerivedHintsTag("first")
	if h.state.HintGate.IsActive() {
		t.Fatal("the gate claims the tag after its Put failed")
	}
	h.applyDerivedHintsTag("second")
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("Put calls = %d; the second arm must retry the Put", calls)
	}
	if !h.state.HintGate.IsActive() {
		t.Error("the gate should hold the tag once a Put succeeds")
	}
}

// An arm decided while the browser was in front must never land after the
// app-focus handler has released the tags for another app. A stress test:
// with decide-and-act in one lock hold it cannot fail; against the old
// decide-unlock-act it catches the race on a fraction of runs (the window is
// narrow), which is why it runs under -race in CI rather than being the only
// guard — the lock discipline in applyDerivedHintsTag is.
func TestHintsArmNeverLandsAfterFocusRelease(t *testing.T) {
	h, _, _ := newDriftHost(t)
	putFrame(t, h, frameReq(0, "arch bake"))

	var released atomic.Bool // true between a completed release for Terminal and the next Chrome focus
	var violations atomic.Int32
	h.putHintsTag = func(string) error {
		if released.Load() {
			violations.Add(1)
		}
		return nil
	}
	h.putActiveTag = func(string) error {
		if released.Load() {
			violations.Add(1)
		}
		return nil
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.applyDerivedHintsTag("stress")
				}
			}
		}()
	}
	for i := 0; i < 300; i++ {
		released.Store(false)
		focusApp(h, chromeBundle)
		focusApp(h, "com.apple.Terminal")
		released.Store(true)
	}
	close(stop)
	wg.Wait()
	if n := violations.Load(); n > 0 {
		t.Errorf("%d tag Put(s) landed after the release for another app", n)
	}
	if h.state.HintGate.IsActive() {
		t.Error("the hints gate is armed with Terminal in front")
	}
}

// Closing a background tab — in this browser or another — must not kill
// hints in the tab the user is looking at; closing the focused tab does.
func TestTabSessionEndRederivesTheHintsTag(t *testing.T) {
	h, _, _ := newDriftHost(t)
	connectBrowser(h, "fox", firefoxBundle)
	putFrame(t, h, frameReq(0, "arch bake"))
	if !h.state.HintGate.IsActive() {
		t.Fatal("setup: the focused tab's grammar should arm the gate")
	}
	end := func(conn string, tab int) {
		w := httptest.NewRecorder()
		serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/hints/session_end",
			strings.NewReader(fmt.Sprintf(`{"conn_id":%q,"tab_id":%d,"reason":"tab_closed"}`, conn, tab))))
		if w.Code != http.StatusOK {
			t.Fatalf("session_end status %d", w.Code)
		}
	}

	end("chrome", 99)
	if !h.state.HintGate.IsActive() {
		t.Error("closing a background tab dropped the focused tab's hints")
	}
	end("fox", driftTab)
	if !h.state.HintGate.IsActive() {
		t.Error("another browser closing a tab dropped the focused browser's hints")
	}
	end("chrome", driftTab)
	if h.state.HintGate.IsActive() {
		t.Error("closing the focused tab must idle the hints gate")
	}
}

// Before the alphabet loads the skeleton builds to nothing; that empty push
// must not count as registered, or no `activate` command ever exists.
func TestSkeletonNotClaimedBeforeTheAlphabetLoads(t *testing.T) {
	h, _ := newRoutingHost(t)
	var pushed []int
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		if group == groupHints {
			pushed = append(pushed, len(cmds))
		}
		return len(cmds), nil
	}
	h.codewords = [26]string{}

	h.ensureSkeleton("browser")
	if h.state.HintCommandSkeletonRegistered {
		t.Fatal("skeleton claimed while the alphabet is absent")
	}

	h.codewords = testCodewords
	h.ensureSkeleton("browser")
	if !h.state.HintCommandSkeletonRegistered {
		t.Fatal("skeleton not registered once the alphabet arrived")
	}
	if n := len(pushed); n == 0 || pushed[n-1] == 0 {
		t.Errorf("hint group pushes = %v, want a non-empty push", pushed)
	}
}

// A /focus claim delivered after the user moved to another app arms the
// browser; the delayed OS re-check undoes it. An early claim (the OS event
// lagging) survives the re-check.
func TestLateFocusClaimIsUndoneByOSRecheck(t *testing.T) {
	h, log := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, "com.apple.Terminal")

	claimFocus(t, h, "chrome")
	h.mu.Lock()
	timer := h.state.ClaimVerifyTimer
	focused := h.state.BrowserFocused
	h.mu.Unlock()
	if !focused || timer == nil {
		t.Fatalf("a contradicting claim should apply and arm a re-check (focused=%v timer=%v)", focused, timer != nil)
	}
	timer.Stop()

	h.frontmostBundle = func() (string, error) { return "com.apple.Terminal", nil }
	h.verifyFocusClaim()
	if h.state.BrowserFocused {
		t.Error("the late claim is still in force with Terminal in front")
	}
	if log.count("delete_active") == 0 {
		t.Error("the browser tags must drop once the claim proves late")
	}

	// Early claim: the OS catches up to it.
	claimFocus(t, h, "chrome")
	h.state.ClaimVerifyTimer.Stop()
	h.frontmostBundle = func() (string, error) { return chromeBundle, nil }
	h.verifyFocusClaim()
	if !h.state.BrowserFocused {
		t.Error("an early claim the OS confirms must stand")
	}

	// An OS event after the claim supersedes it; no re-check left pending.
	focusApp(h, "com.apple.Terminal")
	claimFocus(t, h, "chrome")
	focusApp(h, chromeBundle)
	if h.state.ClaimVerifyTimer != nil {
		t.Error("an OS event after the claim must cancel the pending re-check")
	}
}
