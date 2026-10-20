package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// /focus claims are ordered: per connection by seq, across
// connections sharing a bundle by claimed_at. Both fields are optional; a
// claim without them is applied in arrival order, as before.

// postClaim posts one /focus claim. seq 0 and claimedAt 0 leave the field out,
// as an extension that predates them would. Returns whether it was dropped.
func postClaim(t *testing.T, h *Host, conn string, focused bool, seq uint64, claimedAt int64) (stale bool) {
	t.Helper()
	body := map[string]any{"conn_id": conn, "focused": focused}
	if seq != 0 {
		body["seq"] = seq
	}
	if claimedAt != 0 {
		body["claimed_at"] = claimedAt
	}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPost, "/focus", strings.NewReader(string(raw))))
	if rec.Code != http.StatusOK {
		t.Errorf("/focus status = %d", rec.Code)
		return false
	}
	// The reply is the one write shape ({ok}); a dropped claim is the one
	// whose seq was not applied.
	if seq == 0 {
		return false
	}
	h.mu.Lock()
	applied := h.state.peek(conn).ClaimSeq
	h.mu.Unlock()
	return applied != seq
}

// Chrome gains focus and the user leaves at once: the blur is delivered
// first, the OS reports Terminal, and the gain arrives last. It is older than
// the blur, so it is dropped — the browser tags never go up over Terminal.
func TestFocusGainDeliveredAfterItsBlurIsDropped(t *testing.T) {
	h, log := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, "com.apple.Terminal")

	if postClaim(t, h, "chrome", false, 2, 2_000) {
		t.Fatal("the blur was dropped; it is the newest claim")
	}
	if !postClaim(t, h, "chrome", true, 1, 1_000) {
		t.Error("a gain older than the applied blur must be dropped")
	}
	h.mu.Lock()
	focused, timer := h.state.BrowserFocused, h.state.ClaimVerifyTimer
	h.mu.Unlock()
	if focused {
		t.Error("the stale gain put Chrome in front while Terminal is")
	}
	if timer != nil {
		t.Error("a dropped claim must not arm the OS re-check")
	}
	if log.count("put_active") != 0 {
		t.Error("the browser-active tag went up for a stale claim")
	}

	// The next real gain (newer seq) applies.
	if postClaim(t, h, "chrome", true, 3, 3_000) {
		t.Error("a newer gain was dropped")
	}
	if !h.state.BrowserFocused {
		t.Error("a newer gain must apply")
	}
	h.state.ClaimVerifyTimer.Stop()
}

// The other order: the late gain arrives BEFORE its blur, so it applies (the
// OS contradicts it and a re-check is armed); the blur then withdraws it, and
// the OS is asked at once rather than after the delay.
func TestBlurWithdrawsALateGainAndAsksTheOSAtOnce(t *testing.T) {
	h, log := newRoutingHost(t)
	h.runAsync = func(f func()) { f() }
	connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, "com.apple.Terminal")

	postClaim(t, h, "chrome", true, 1, 1_000)
	if !h.state.BrowserFocused || h.state.ClaimVerifyTimer == nil {
		t.Fatalf("setup: the contradicting gain should apply and arm a re-check (focused=%v)", h.state.BrowserFocused)
	}
	var asked int
	h.frontmostBundle = func() (string, error) { asked++; return "com.apple.Terminal", nil }

	postClaim(t, h, "chrome", false, 2, 2_000)
	if asked != 1 {
		t.Errorf("OS asked %d time(s) on the blur, want 1", asked)
	}
	if h.state.BrowserFocused {
		t.Error("the withdrawn gain is still in force with Terminal in front")
	}
	if h.state.ClaimVerifyTimer != nil {
		t.Error("the delayed re-check must be spent by the immediate one")
	}
	if log.count("delete_active") == 0 {
		t.Error("the browser tags must drop once the OS answers")
	}
}

// A blur from a connection whose claim the OS confirmed does nothing on its
// own: the OS event for wherever the user went does the release.
func TestBlurOfAConfirmedClaimWaitsForTheOS(t *testing.T) {
	h, _ := newRoutingHost(t)
	h.runAsync = func(f func()) { f() }
	h.frontmostBundle = func() (string, error) { t.Error("the OS was asked; nothing was unconfirmed"); return "", nil }
	connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)
	postClaim(t, h, "chrome", true, 1, 1_000)
	postClaim(t, h, "chrome", false, 2, 2_000)
	if !h.state.BrowserFocused || h.state.FocusedConnID != "chrome" {
		t.Errorf("a blur alone moved focus (focused=%v conn=%q)", h.state.BrowserFocused, h.state.FocusedConnID)
	}
}

// Two profiles of one browser: the OS names the same app for both, so the
// newest claim by when it was MADE wins — not the last to arrive. A released
// claim stops counting.
func TestNewestClaimWinsBetweenProfiles(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)

	postClaim(t, h, "profile-2", true, 1, 2_000)
	postClaim(t, h, "profile-1", true, 1, 1_000) // made first, delivered late
	if got := h.state.FocusedConnID; got != "profile-2" {
		t.Fatalf("focused = %q, want profile-2 (the newer claim)", got)
	}

	postClaim(t, h, "profile-1", true, 2, 3_000)
	if got := h.state.FocusedConnID; got != "profile-1" {
		t.Fatalf("focused = %q, want profile-1 after its newer claim", got)
	}

	// profile-1 lets go; the OS then reports the browser again (a recompute):
	// the only standing claim is profile-2's.
	postClaim(t, h, "profile-1", false, 3, 4_000)
	focusApp(h, chromeBundle)
	if got := h.state.FocusedConnID; got != "profile-2" {
		t.Errorf("focused = %q, want profile-2 once profile-1 released its claim", got)
	}
}

// A claimed_at later than the claim's arrival is not believed — it would win
// every same-bundle pick until released.
func TestFutureClaimedAtCountsAsArrival(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)

	postClaim(t, h, "profile-1", true, 1, time.Now().Add(time.Hour).UnixMilli())
	postClaim(t, h, "profile-2", true, 0, 0) // an old client, after it
	if got := h.state.FocusedConnID; got != "profile-2" {
		t.Errorf("focused = %q, want profile-2: a future claimed_at must not outrank a later claim", got)
	}
}

// An extension that predates the fields: every claim applies, in arrival
// order, exactly as before.
func TestClaimsWithoutOrderingApplyInArrivalOrder(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)
	for i, conn := range []string{"profile-2", "profile-1", "profile-2", "profile-1"} {
		if postClaim(t, h, conn, true, 0, 0) {
			t.Fatalf("claim %d without a seq was dropped", i)
		}
		if got := h.state.FocusedConnID; got != conn {
			t.Fatalf("claim %d: focused = %q, want the last to arrive, %q", i, got, conn)
		}
	}
}

// Claims delivered concurrently and in any order: per connection only the
// newest seq sticks, and across two profiles the newest claimed_at wins.
// Decided under one lock hold per claim, so -race and the shuffled delivery
// together are the test.
func TestConcurrentClaimsSettleOnTheNewest(t *testing.T) {
	const n = 40
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)

	type claim struct {
		conn    string
		focused bool
		seq     uint64
		at      int64
	}
	var claims []claim
	for seq := uint64(1); seq <= n; seq++ {
		// profile-1's claims alternate gain and blur, ending on a gain;
		// profile-2's are all gains, each 1ms after profile-1's same seq.
		claims = append(claims,
			claim{"profile-1", seq%2 == 0, seq, int64(1_000 + 2*seq)},
			claim{"profile-2", true, seq, int64(1_000 + 2*seq + 1)})
	}
	rand.Shuffle(len(claims), func(i, j int) { claims[i], claims[j] = claims[j], claims[i] })

	var wg sync.WaitGroup
	for _, c := range claims {
		wg.Add(1)
		go func(c claim) {
			defer wg.Done()
			postClaim(t, h, c.conn, c.focused, c.seq, c.at)
		}(c)
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, conn := range []string{"profile-1", "profile-2"} {
		if got := h.state.peek(conn).ClaimSeq; got != n {
			t.Errorf("%s: last applied seq = %d, want %d", conn, got, n)
		}
	}
	if got, want := h.state.peek("profile-1").ClaimAt, int64(1_000+2*n); got != want {
		t.Errorf("profile-1 standing claim at %d, want its newest (%d)", got, want)
	}
	if got := h.state.FocusedConnID; got != "profile-2" {
		t.Errorf("focused = %q, want profile-2, whose last claim is the newest", got)
	}
}
