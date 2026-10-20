package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tripwire's emit half is behind the emitDisconnectNudge seam; these
// tests drive the arm/fire/clear state machine directly (Locked helpers are
// called without the mutex here — single-goroutine tests, except the timer
// fire which is exercised synchronously via fireDisconnectNudge).

func newNudgeTestState(h *Host) {
	h.state = &PluginState{
		PluginID:         "browser",
		SeenBundles:      make(map[string]bool),
		DisconnectNudged: make(map[string]bool),
	}
}

func captureNudges(h *Host, t *testing.T) *[]string {
	t.Helper()
	var fired []string
	orig := h.emitDisconnectNudge
	h.emitDisconnectNudge = func(_, bundle string) { fired = append(fired, bundle) }
	t.Cleanup(func() { h.emitDisconnectNudge = orig })
	return &fired
}

func TestNudgeIgnoresNeverConnectedBundles(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	h.armDisconnectNudgeLocked("dev.warp.Warp-Stable")
	if h.state.DisconnectNudgeTimer != nil {
		t.Fatal("never-connected bundle must not arm the nudge")
	}
}

func TestNudgeIgnoresLiveBundles(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	h.noteBundleConnectedLocked("com.google.Chrome")
	h.state.conn("conn-1").AssertedBundle = "com.google.Chrome"
	h.armDisconnectNudgeLocked("com.google.Chrome")
	if h.state.DisconnectNudgeTimer != nil {
		t.Fatal("live bundle must not arm the nudge")
	}
}

func TestNudgeFiresForSeenDeadBundleOncePerEpisode(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	fired := captureNudges(h, t)

	// Connected earlier this session, now gone.
	h.noteBundleConnectedLocked("com.google.Chrome")

	h.armDisconnectNudgeLocked("com.google.Chrome")
	if h.state.DisconnectNudgeTimer == nil || h.state.DisconnectNudgeBundle != "com.google.Chrome" {
		t.Fatal("seen+dead bundle should arm the grace timer")
	}
	// Fire synchronously (stand-in for the timer callback).
	h.state.DisconnectNudgeTimer.Stop()
	h.fireDisconnectNudge("com.google.Chrome")
	if len(*fired) != 1 || (*fired)[0] != "com.google.Chrome" {
		t.Fatalf("fired = %v, want one nudge for Chrome", *fired)
	}

	// Re-focusing during the same episode must not nudge again.
	h.armDisconnectNudgeLocked("com.google.Chrome")
	if h.state.DisconnectNudgeTimer != nil {
		t.Fatal("nudged episode must not re-arm")
	}
}

func TestNudgeSuppressedWhenReconnectWinsTheGrace(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	fired := captureNudges(h, t)
	h.noteBundleConnectedLocked("com.google.Chrome")

	h.armDisconnectNudgeLocked("com.google.Chrome")
	h.state.DisconnectNudgeTimer.Stop()
	// Extension reconnects inside the grace window...
	h.noteBundleConnectedLocked("com.google.Chrome")
	h.state.conn("conn-2").AssertedBundle = "com.google.Chrome"
	// ...then the (stale) timer fires anyway.
	h.fireDisconnectNudge("com.google.Chrome")
	if len(*fired) != 0 {
		t.Fatalf("fired = %v, want none — reconnect won the race", *fired)
	}
}

func TestReconnectClosesEpisodeAndCancelsPendingTimer(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	h.noteBundleConnectedLocked("com.google.Chrome")

	// Nudged once; then it reconnects.
	h.state.DisconnectNudged["com.google.Chrome"] = true
	h.state.DisconnectNudgeTimer = time.AfterFunc(time.Hour, func() {})
	h.state.DisconnectNudgeBundle = "com.google.Chrome"
	h.noteBundleConnectedLocked("com.google.Chrome")

	if h.state.DisconnectNudged["com.google.Chrome"] {
		t.Fatal("reconnect must clear the nudged latch")
	}
	if h.state.DisconnectNudgeTimer != nil || h.state.DisconnectNudgeBundle != "" {
		t.Fatal("reconnect must cancel a pending timer for the bundle")
	}

	// A NEW disconnect episode nudges again.
	fired := captureNudges(h, t)
	h.armDisconnectNudgeLocked("com.google.Chrome")
	if h.state.DisconnectNudgeTimer == nil {
		t.Fatal("new episode should re-arm")
	}
	h.state.DisconnectNudgeTimer.Stop()
	h.fireDisconnectNudge("com.google.Chrome")
	if len(*fired) != 1 {
		t.Fatalf("fired = %v, want one nudge for the new episode", *fired)
	}
}

func TestArmSwitchesPendingTimerToNewBundle(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	h.noteBundleConnectedLocked("com.google.Chrome")
	h.noteBundleConnectedLocked("org.mozilla.firefox")

	h.armDisconnectNudgeLocked("com.google.Chrome")
	h.armDisconnectNudgeLocked("org.mozilla.firefox")
	if h.state.DisconnectNudgeBundle != "org.mozilla.firefox" {
		t.Fatalf("pending bundle = %q, want the most recently focused", h.state.DisconnectNudgeBundle)
	}
	h.state.DisconnectNudgeTimer.Stop()
}

// captureHealth stubs the platform health report so a test can assert what
// the plugin would tell the platform, without an RPC connection.
func captureHealth(h *Host, t *testing.T) *[]string {
	t.Helper()
	var reports []string
	orig := h.reportHealth
	h.reportHealth = func(degraded bool, reason string) {
		if degraded {
			reports = append(reports, "degraded: "+reason)
		} else {
			reports = append(reports, "healthy")
		}
	}
	t.Cleanup(func() { h.reportHealth = orig })
	return &reports
}

// The point of the whole tripwire: the platform reads "Active" for a plugin
// that is alive and answering while serving a fraction of its commands, so
// the plugin has to say so itself.
func TestDisconnectReportsDegradedToThePlatform(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	captureNudges(h, t)
	health := captureHealth(h, t)

	// Seen this session, no live connection now — the vanished case.
	h.noteBundleConnectedLocked("com.google.Chrome")
	h.fireDisconnectNudge("com.google.Chrome")

	if len(*health) != 1 {
		t.Fatalf("expected one health report, got %v", *health)
	}
	// The reason must name the browser and the remedy — a status the user
	// cannot act on is the thing this replaces.
	got := (*health)[0]
	if !strings.HasPrefix(got, "degraded: ") ||
		!strings.Contains(got, "Chrome") ||
		!strings.Contains(got, "chrome://extensions") {
		t.Errorf("reason must name the browser and the remedy, got %q", got)
	}
}

func TestReconnectReportsHealthyAgain(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)

	// Mid-episode: the plugin has already told the platform it is degraded.
	h.state.SeenBundles["com.google.Chrome"] = true
	h.state.DisconnectNudged["com.google.Chrome"] = true

	h.noteBundleConnectedLocked("com.google.Chrome")

	if len(*health) != 1 || (*health)[0] != "healthy" {
		t.Fatalf("reconnect must clear the report, got %v", *health)
	}
}

// A reconnect with no episode open must stay silent: every focus on a healthy
// browser runs this path, and a report per focus would be noise the platform
// re-renders on.
func TestReconnectWithoutAnEpisodeReportsNothing(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)

	h.noteBundleConnectedLocked("com.google.Chrome")
	h.noteBundleConnectedLocked("com.google.Chrome")

	if len(*health) != 0 {
		t.Fatalf("expected silence, got %v", *health)
	}
}

// Two browsers dead, one comes back: the plugin must not announce that it is
// healthy while the other is still broken. The health report is plugin-wide
// and an episode is per-bundle, so the clear has to wait for the last one.
func TestOneBrowserReturningDoesNotClearWhileAnotherIsDead(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)

	for _, b := range []string{"com.google.Chrome", "org.mozilla.firefox"} {
		h.state.SeenBundles[b] = true
		h.state.DisconnectNudged[b] = true
	}

	h.noteBundleConnectedLocked("com.google.Chrome")
	if len(*health) != 0 {
		t.Fatalf("Firefox is still dead — must stay degraded, got %v", *health)
	}

	h.noteBundleConnectedLocked("org.mozilla.firefox")
	if len(*health) != 1 || (*health)[0] != "healthy" {
		t.Fatalf("last one back must clear, got %v", *health)
	}
}

// stubRunningApps supplies the reaper's OS truth without an RPC connection,
// same seam pattern as captureHealth.
func stubRunningApps(h *Host, t *testing.T, bundles ...string) {
	t.Helper()
	running := make(map[string]bool)
	for _, b := range bundles {
		running[b] = true
	}
	orig := h.listRunningBundles
	h.listRunningBundles = func() (map[string]bool, error) { return running, nil }
	t.Cleanup(func() { h.listRunningBundles = orig })
}

// A quit browser is "standalone by choice" — the state this tripwire exists
// to NOT warn about. A standing "reload the extension" complaint about a
// browser that is no longer running is a false alarm the user cannot act on.
func TestQuitBrowserClosesItsEpisode(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)
	stubRunningApps(h, t) // Chrome no longer running

	h.state.SeenBundles["com.google.Chrome"] = true
	h.state.DisconnectNudged["com.google.Chrome"] = true

	h.reapQuitEpisodes()

	if h.state.DisconnectNudged["com.google.Chrome"] {
		t.Fatal("a quit browser's episode must close")
	}
	if len(*health) != 1 || (*health)[0] != "healthy" {
		t.Fatalf("last open episode gone must clear the report, got %v", *health)
	}
}

// Same plugin-wide rule as reconnect: quitting Chrome must not announce
// health while Firefox is still broken and running.
func TestQuitOfOneBrowserDoesNotClearWhileAnotherIsDead(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)
	stubRunningApps(h, t, "org.mozilla.firefox")

	for _, b := range []string{"com.google.Chrome", "org.mozilla.firefox"} {
		h.state.SeenBundles[b] = true
		h.state.DisconnectNudged[b] = true
	}

	h.reapQuitEpisodes()

	if h.state.DisconnectNudged["com.google.Chrome"] {
		t.Fatal("quit Chrome's episode must close")
	}
	if !h.state.DisconnectNudged["org.mozilla.firefox"] {
		t.Fatal("running-but-dead Firefox must stay nudged")
	}
	if len(*health) != 0 {
		t.Fatalf("Firefox is still broken — must stay degraded, got %v", *health)
	}
}

// A quit during the grace window cancels the pending timer: a nudge about a
// browser that is gone answers nothing.
func TestQuitDuringGraceCancelsPendingTimer(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	captureHealth(h, t)
	stubRunningApps(h, t) // nothing running

	h.noteBundleConnectedLocked("com.google.Chrome")
	h.armDisconnectNudgeLocked("com.google.Chrome")
	if h.state.DisconnectNudgeTimer == nil {
		t.Fatal("precondition: grace timer armed")
	}

	h.reapQuitEpisodes()

	if h.state.DisconnectNudgeTimer != nil || h.state.DisconnectNudgeBundle != "" {
		t.Fatal("quit must cancel the pending grace timer")
	}
}

// The reaper keeps its hands off episodes when OS truth is unavailable —
// guessing is what this design refuses to do.
func TestReapKeepsEpisodesWhenRunningAppsUnavailable(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	health := captureHealth(h, t)
	orig := h.listRunningBundles
	h.listRunningBundles = func() (map[string]bool, error) {
		return nil, fmt.Errorf("rpc down")
	}
	t.Cleanup(func() { h.listRunningBundles = orig })

	h.state.SeenBundles["com.google.Chrome"] = true
	h.state.DisconnectNudged["com.google.Chrome"] = true

	h.reapQuitEpisodes()

	if !h.state.DisconnectNudged["com.google.Chrome"] {
		t.Fatal("no OS truth means no reap")
	}
	if len(*health) != 0 {
		t.Fatalf("expected silence, got %v", *health)
	}
}

// The stopped-too-late race: a timer for one bundle can fire after a newer
// timer was armed for another. The stale fire must neither clobber the new
// timer's bookkeeping nor (via the nudged latch) emit twice for one episode.
func TestStaleTimerFireDoesNotClobberNewerArm(t *testing.T) {
	h := newTestHost()
	newNudgeTestState(h)
	fired := captureNudges(h, t)
	captureHealth(h, t)
	h.noteBundleConnectedLocked("com.google.Chrome")
	h.noteBundleConnectedLocked("org.mozilla.firefox")

	h.armDisconnectNudgeLocked("com.google.Chrome")
	// Firefox arm stops Chrome's timer — but too late; its callback is
	// already running. Simulate the interleaving.
	h.armDisconnectNudgeLocked("org.mozilla.firefox")
	h.fireDisconnectNudge("com.google.Chrome") // the stale callback

	if h.state.DisconnectNudgeBundle != "org.mozilla.firefox" || h.state.DisconnectNudgeTimer == nil {
		t.Fatal("stale fire must not clear the newer bundle's pending timer")
	}
	h.state.DisconnectNudgeTimer.Stop()

	// Chrome still nudged exactly once despite the racing fire.
	if len(*fired) != 1 || (*fired)[0] != "com.google.Chrome" {
		t.Fatalf("fired = %v, want exactly one Chrome nudge", *fired)
	}
	h.fireDisconnectNudge("com.google.Chrome") // a second racing duplicate
	if len(*fired) != 1 {
		t.Fatalf("fired = %v, the nudged latch must hold in the fire path too", *fired)
	}
}

// A Firefox user was told to "reload it at chrome://extensions" — a page
// Firefox does not have, for an add-on a restart had already removed. The remedy is the browser's own, all the way to the platform.
func TestDisconnectRemedyIsTheBrowsersOwn(t *testing.T) {
	for _, tc := range []struct{ bundle, want, not string }{
		{"org.mozilla.firefox", "load it again from about:debugging", "chrome://"},
		{"org.mozilla.firefoxdeveloperedition", "about:debugging", "reload"},
		{"firefox", "about:debugging", "chrome://"},
		{"com.microsoft.edgemac", "edge://extensions", "chrome://"},
		{"com.brave.Browser", "brave://extensions", "chrome://"},
		{"com.google.Chrome", "reload it at chrome://extensions", "about:debugging"},
	} {
		h := newTestHost()
		newNudgeTestState(h)
		captureNudges(h, t)
		health := captureHealth(h, t)
		h.noteBundleConnectedLocked(tc.bundle)
		h.fireDisconnectNudge(tc.bundle)
		if len(*health) != 1 {
			t.Fatalf("%s: expected one health report, got %v", tc.bundle, *health)
		}
		got := (*health)[0]
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) {
			t.Errorf("%s: reason %q, want it to contain %q and not %q", tc.bundle, got, tc.want, tc.not)
		}
	}
}
