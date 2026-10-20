package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Dispatch routing: a spoken command reaches exactly one browser connection,
// or none with a reason — never a broadcast.

const (
	chromeBundle  = "com.google.Chrome"
	firefoxBundle = "org.mozilla.firefox"
)

// seamLog records the tag seams a routing test cares about. Mutex-guarded:
// the concurrent tests call the seams from several goroutines.
type seamLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *seamLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *seamLog) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c == s {
			n++
		}
	}
	return n
}

func newRoutingHost(t *testing.T) (*Host, *seamLog) {
	t.Helper()
	h := newTestHost()
	h.state = &PluginState{
		PluginID:            "browser",
		SSEClients:          make(map[string]chan ActionEvent),
		SSEClientBundles:    make(map[string]string),
		FrameSessions:       make(map[frameKey]*FrameSession),
		LastSyncFingerprint: make(map[string]uint64),
		LastPushedTabKeys:   make(map[string]bool),
		SeenBundles:         make(map[string]bool),
		DisconnectNudged:    make(map[string]bool),
	}
	h.state.HintGate = newHintGate(h, "browser")
	log := &seamLog{}
	h.putActiveTag = func(string) error { log.add("put_active"); return nil }
	h.deleteActiveTag = func(string) error { log.add("delete_active"); return nil }
	h.deleteHintsTag = func(string) error { log.add("delete_hints"); return nil }
	h.pushCommandGroupRPC = func(string, []branchkit.CommandSpec) (int, error) { return 0, nil }
	h.emitDisconnectNudge = func(string, string) { log.add("nudge") }
	h.reportHealth = func(bool, string) {}
	h.putMediaActiveTag = func(string) error { return nil }
	h.deleteMediaActiveTag = func(string) error { return nil }
	return h, log
}

// connectBrowser registers a stream for conn and records its OS-asserted
// identity, as handleSSE + resolveAssertedBundle would.
func connectBrowser(h *Host, conn, bundle string) (string, chan ActionEvent) {
	_, client, ch := h.registerSSEClient(conn)
	h.mu.Lock()
	h.state.conn(conn).AssertedBundle = bundle
	h.state.SSEClientBundles[client] = bundle
	h.state.SeenBundles[bundle] = true
	h.mu.Unlock()
	return client, ch
}

// focusApp feeds an OS app-focus event, as the platform would.
func focusApp(h *Host, bundle string) {
	h.handleAppFocused(branchkit.AppFocusedEventParams{BundleID: bundle})
}

// claimFocus posts a /focus claim from conn, as the extension's
// windows.onFocusChanged listener would.
func claimFocus(t *testing.T, h *Host, conn string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/focus",
		strings.NewReader(`{"conn_id":"`+conn+`","focused":true}`))
	rec := httptest.NewRecorder()
	serveRoute(h, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/focus status = %d", rec.Code)
	}
}

func dispatch(h *Host, action string) branchkit.OnActionResponse {
	resp, _ := h.handleOnAction(&branchkit.OnActionRequest{
		Action: "browser." + action,
		Params: json.RawMessage(`{}`),
	})
	r, _ := resp.(branchkit.OnActionResponse)
	return r
}

// received drains a client channel and returns the action names it held.
func received(ch chan ActionEvent) []string {
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

func countAction(actions []string, want string) int {
	n := 0
	for _, a := range actions {
		if a == want {
			n++
		}
	}
	return n
}

// Two profiles of one browser share its bundle. The focused connection must be
// the one that claimed window focus, stay stable across recomputes, and be the
// ONLY one a command reaches.
func TestTwoProfilesSameBundleDispatchReachesClaimantOnly(t *testing.T) {
	h, _ := newRoutingHost(t)
	_, ch1 := connectBrowser(h, "profile-1", chromeBundle)
	_, ch2 := connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)

	claimFocus(t, h, "profile-2")
	for i := 0; i < 50; i++ {
		h.mu.Lock()
		h.recomputeFocusedSourceLocked("test")
		got := h.state.FocusedConnID
		h.mu.Unlock()
		if got != "profile-2" {
			t.Fatalf("recompute %d picked %q, want the claimant profile-2", i, got)
		}
	}
	received(ch1)
	received(ch2)

	dispatch(h, "close_tab")
	if got := countAction(received(ch1), "close_tab"); got != 0 {
		t.Errorf("profile-1 received close_tab %d time(s); only the focused profile may", got)
	}
	if got := countAction(received(ch2), "close_tab"); got != 1 {
		t.Errorf("profile-2 received close_tab %d time(s), want 1", got)
	}

	// The other profile's window takes focus: it becomes the target.
	claimFocus(t, h, "profile-1")
	received(ch1)
	received(ch2)
	dispatch(h, "close_tab")
	if got := countAction(received(ch1), "close_tab"); got != 1 {
		t.Errorf("after profile-1's claim it received close_tab %d time(s), want 1", got)
	}
	if got := countAction(received(ch2), "close_tab"); got != 0 {
		t.Errorf("after profile-1's claim profile-2 still received close_tab %d time(s)", got)
	}
}

// With no claim yet, the pick among same-bundle connections is stable: the one
// already focused, then the lowest conn id — never map iteration order.
func TestConnForAssertedBundleTieBreakIsStable(t *testing.T) {
	h, _ := newRoutingHost(t)
	h.state.conn("b").AssertedBundle = chromeBundle
	h.state.conn("a").AssertedBundle = chromeBundle
	h.state.conn("c").AssertedBundle = chromeBundle
	for i := 0; i < 50; i++ {
		if got := h.connForAssertedBundleLocked(chromeBundle); got != "a" {
			t.Fatalf("unfocused tie picked %q, want lowest id a", got)
		}
	}
	h.state.FocusedConnID = "c"
	for i := 0; i < 50; i++ {
		if got := h.connForAssertedBundleLocked(chromeBundle); got != "c" {
			t.Fatalf("tie with c focused picked %q, want c (sticky)", got)
		}
	}
	h.noteConnFocusClaimLocked("b", 1)
	if got := h.connForAssertedBundleLocked(chromeBundle); got != "b" {
		t.Fatalf("after b's claim picked %q, want b", got)
	}
}

// A profile switch changes the connection without changing the bundle, and
// must park the losing profile's grammar exactly as a browser switch does —
// otherwise its codewords keep matching while the other profile is in front.
func TestProfileSwitchParksLosingProfile(t *testing.T) {
	h, _ := newRoutingHost(t)
	pushes, stub := recordPushes()
	withTestPushFn(h, t, stub)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	h.state.conn("profile-1").ActiveTab = 3
	sess := h.ensureFrameSession(frameKey{ConnID: "profile-1", TabID: 3})
	sess.Codewords["arch bake"] = map[string]string{"id": "1", "letter": "ab"}

	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")
	if h.state.FocusedConnID != "profile-1" {
		t.Fatalf("focused = %q, want profile-1", h.state.FocusedConnID)
	}
	for k := range pushes {
		delete(pushes, k)
	}

	claimFocus(t, h, "profile-2")
	if !sess.SweptStale {
		t.Error("the losing profile's session must be parked on a profile switch")
	}
	if got, ok := pushes[hintCollection("browser", "arch")]; !ok || len(got) != 0 {
		t.Errorf("the losing profile's prefix must be drained, got %v (synced=%v)", got, ok)
	}
}

// Chrome in front, Firefox behind; Chrome's extension goes away. A command
// must reach nobody — not Firefox — and the browser tags must drop.
func TestFocusedDisconnectDropsCommandInsteadOfBroadcasting(t *testing.T) {
	h, log := newRoutingHost(t)
	var tabsProjected [][]branchkit.CollectionPutEntry
	h.replaceTabsCollection = func(recs []branchkit.CollectionPutEntry) error {
		tabsProjected = append(tabsProjected, recs)
		return nil
	}
	chromeClient, _ := connectBrowser(h, "chrome", chromeBundle)
	_, foxCh := connectBrowser(h, "fox", firefoxBundle)
	h.state.setTabs("chrome", []TabEntry{{Spoken: "docs", TabID: "4"}})
	h.state.setTabs("fox", []TabEntry{{Spoken: "mail", TabID: "9"}})
	focusApp(h, chromeBundle)
	if h.state.FocusedConnID != "chrome" {
		t.Fatalf("focused = %q, want chrome", h.state.FocusedConnID)
	}
	tabsProjected = nil

	h.disconnectSSEClient("browser", "chrome", chromeClient)

	if h.state.FocusedConnID != "" || h.state.BrowserFocused {
		t.Errorf("focused slot not released: conn=%q browser=%v", h.state.FocusedConnID, h.state.BrowserFocused)
	}
	if log.count("delete_active") != 1 {
		t.Errorf("plugin.browser.active deletes = %d, want 1", log.count("delete_active"))
	}
	if log.count("delete_hints") == 0 {
		t.Error("the hints tag must be recomputed (idled) when the focused browser disconnects")
	}
	if len(tabsProjected) == 0 {
		t.Error("the dead browser's tab words must be drained")
	}
	for _, recs := range tabsProjected {
		if len(recs) != 0 {
			t.Errorf("a background browser's tab words were projected: %v", recs)
		}
	}

	received(foxCh)
	dispatch(h, "close_tab")
	if got := received(foxCh); len(got) != 0 {
		t.Errorf("Firefox (behind) received %v; a command for the browser in front must not reach it", got)
	}
}

// Losing the focused profile hands focus to the other profile of the same
// browser: it is the one in front now, and commands reach it.
func TestFocusedDisconnectHandsOverToSameBundleProfile(t *testing.T) {
	h, log := newRoutingHost(t)
	c1, _ := connectBrowser(h, "profile-1", chromeBundle)
	_, ch2 := connectBrowser(h, "profile-2", chromeBundle)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")

	h.disconnectSSEClient("browser", "profile-1", c1)

	if h.state.FocusedConnID != "profile-2" || !h.state.BrowserFocused {
		t.Fatalf("focus should pass to profile-2, got conn=%q browser=%v", h.state.FocusedConnID, h.state.BrowserFocused)
	}
	if log.count("delete_active") != 0 {
		t.Error("the active tag must stay while another connection of the frontmost browser remains")
	}
	received(ch2)
	dispatch(h, "close_tab")
	if got := countAction(received(ch2), "close_tab"); got != 1 {
		t.Errorf("profile-2 received close_tab %d time(s), want 1", got)
	}
}

// The extension reconnects under the SAME conn_id and the new stream registers
// before the old one's cleanup runs. The old cleanup must leave the new
// stream's state alone.
func TestSameConnIDReconnectKeepsNewStreamState(t *testing.T) {
	h, log := newRoutingHost(t)
	oldClient, _ := connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)
	h.storeContributedCommands("chrome", []contributedCommand{{Action: "scroll_down", Pattern: "scroll down"}})

	newClient, newCh := connectBrowser(h, "chrome", chromeBundle)
	h.disconnectSSEClient("browser", "chrome", oldClient)

	h.contribMu.RLock()
	_, kept := h.contributed["chrome"]
	h.contribMu.RUnlock()
	if !kept {
		t.Error("the old stream's cleanup deleted the command catalog the new stream owns")
	}
	if h.state.peek("chrome").Client != newClient {
		t.Errorf("conn maps to %q, want the new stream %q", h.state.peek("chrome").Client, newClient)
	}
	if h.state.FocusedConnID != "chrome" || h.state.peek("chrome").AssertedBundle != chromeBundle {
		t.Errorf("focus/identity lost: focused=%q asserted=%q", h.state.FocusedConnID, h.state.peek("chrome").AssertedBundle)
	}
	if log.count("delete_active") != 0 {
		t.Error("the active tag must not drop on a same-conn reconnect")
	}
	received(newCh)
	dispatch(h, "close_tab")
	if got := countAction(received(newCh), "close_tab"); got != 1 {
		t.Errorf("new stream received close_tab %d time(s), want 1", got)
	}
}

// The same reconnect race through the real handler, streams on their own
// goroutines, with dispatches running alongside — for -race.
func TestSameConnIDReconnectConcurrentStreams(t *testing.T) {
	h, _ := newRoutingHost(t)
	h.mu.Lock()
	h.state.FocusedBundleID = chromeBundle
	h.mu.Unlock()

	openStream := func() (context.CancelFunc, chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/events?conn_id=chrome", nil).WithContext(ctx)
		req.RemoteAddr = "" // no identity lookup goroutine: the test asserts it
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.handleSSE(httptest.NewRecorder(), req, "chrome")
		}()
		return cancel, done
	}
	clientFor := func() string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.state.peek("chrome").Client
	}
	waitFor := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(time.Millisecond)
		}
	}
	assertIdentity := func(client string) {
		h.mu.Lock()
		h.state.conn("chrome").AssertedBundle = chromeBundle
		h.state.SSEClientBundles[client] = chromeBundle
		h.recomputeFocusedSourceLocked("test")
		h.mu.Unlock()
	}

	cancelOld, oldDone := openStream()
	waitFor(func() bool { return clientFor() != "" })
	oldClient := clientFor()
	assertIdentity(oldClient)
	h.storeContributedCommands("chrome", []contributedCommand{{Action: "scroll_down", Pattern: "scroll down"}})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				dispatch(h, "scroll_down")
			}
		}
	}()

	cancelNew, newDone := openStream()
	waitFor(func() bool { c := clientFor(); return c != "" && c != oldClient })
	newClient := clientFor()
	assertIdentity(newClient)
	cancelOld()
	<-oldDone
	close(stop)
	wg.Wait()

	h.contribMu.RLock()
	_, kept := h.contributed["chrome"]
	h.contribMu.RUnlock()
	if !kept {
		t.Error("the old stream's cleanup deleted the new stream's command catalog")
	}
	h.mu.Lock()
	client, reason := h.focusedClientLocked()
	h.mu.Unlock()
	if client != newClient {
		t.Errorf("focused client = %q (%s), want the new stream %q", client, reason, newClient)
	}
	cancelNew()
	<-newDone
}

// Media verbs go where the media is: the tag gating them is an OR over every
// connection, so "pause" with a video in the browser behind must reach that
// browser, not the focused one.
func TestMediaVerbFollowsPlayingBrowser(t *testing.T) {
	h, _ := newRoutingHost(t)
	_, chromeCh := connectBrowser(h, "chrome", chromeBundle)
	_, foxCh := connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, chromeBundle)
	h.state.conn("fox").Session.MediaActive = true
	h.state.MediaLastConnID = "fox"
	received(chromeCh)
	received(foxCh)

	dispatch(h, "media_play_pause")
	if got := received(chromeCh); len(got) != 0 {
		t.Errorf("focused Chrome (no media) received %v", got)
	}
	if got := countAction(received(foxCh), "media_play_pause"); got != 1 {
		t.Errorf("Firefox (playing) received media_play_pause %d time(s), want 1", got)
	}

	// Media in the focused browser too: what you see wins.
	h.state.conn("chrome").Session.MediaActive = true
	dispatch(h, "media_play_pause")
	if got := countAction(received(chromeCh), "media_play_pause"); got != 1 {
		t.Errorf("focused Chrome with media received %d, want 1", got)
	}
	if got := received(foxCh); len(got) != 0 {
		t.Errorf("Firefox received %v while the focused browser had media", got)
	}

	// "pause everything" reaches every browser with media.
	dispatch(h, "media_pause_all")
	if countAction(received(chromeCh), "media_pause_all") != 1 || countAction(received(foxCh), "media_pause_all") != 1 {
		t.Error("media_pause_all must reach every media-active browser once")
	}

	// Nothing asserting media: the focused browser, as every other verb.
	for _, c := range h.state.Conns {
		c.Session.MediaActive = false
	}
	dispatch(h, "media_play_pause")
	if countAction(received(chromeCh), "media_play_pause") != 1 {
		t.Error("with no media anywhere the verb falls back to the focused browser")
	}
	if got := received(foxCh); len(got) != 0 {
		t.Errorf("Firefox received %v with no media anywhere", got)
	}
}

// Fail-open without a bound focused connection: only to a lone connection
// that could be the one in front.
func TestFocusedClientFailOpen(t *testing.T) {
	cases := []struct {
		name       string
		front      string
		conns      map[string]string // conn -> asserted bundle ("" = unresolved)
		wantClient string            // conn whose client should be picked, "" for none
		wantReason string
	}{
		{"no clients", chromeBundle, nil, "", dispatchNoBrowser},
		{"focus unknown, lone browser", "", map[string]string{"a": chromeBundle}, "a", ""},
		{"focus unknown, two browsers", "", map[string]string{"a": chromeBundle, "b": firefoxBundle}, "", dispatchAmbiguous},
		{"lone unresolved identity", chromeBundle, map[string]string{"a": ""}, "a", ""},
		{"two unresolved identities", chromeBundle, map[string]string{"a": "", "b": ""}, "", dispatchAmbiguous},
		{"only a background browser", "com.apple.Terminal", map[string]string{"b": firefoxBundle}, "", dispatchNotFocused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newRoutingHost(t)
			clients := map[string]string{}
			for conn, bundle := range c.conns {
				_, client, _ := h.registerSSEClient(conn)
				clients[conn] = client
				h.state.conn(conn).AssertedBundle = bundle
				h.state.SSEClientBundles[client] = bundle
			}
			h.state.FocusedBundleID = c.front
			client, reason := h.focusedClientLocked()
			if want := clients[c.wantClient]; client != want || reason != c.wantReason {
				t.Errorf("got (%q, %q), want (%q, %q)", client, reason, want, c.wantReason)
			}
		})
	}
}
