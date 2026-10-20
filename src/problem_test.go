package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// A browser command that cannot run says so: an error status
// with the reason, and a problem the person is told.

type problemRecorder struct {
	mu   sync.Mutex
	seen []actionProblem
}

func (r *problemRecorder) record(p actionProblem) {
	r.mu.Lock()
	r.seen = append(r.seen, p)
	r.mu.Unlock()
}

func (r *problemRecorder) last(t *testing.T) actionProblem {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		t.Fatal("no problem was reported")
	}
	return r.seen[len(r.seen)-1]
}

func newProblemHost(t *testing.T) (*Host, *problemRecorder, *seamLog) {
	t.Helper()
	h, log := newRoutingHost(t)
	rec := &problemRecorder{}
	h.reportProblem = rec.record
	h.runAsync = func(f func()) { f() }
	return h, rec, log
}

func failureReason(t *testing.T, resp branchkit.OnActionResponse) string {
	t.Helper()
	if resp.Status != branchkit.OnActionStatusError {
		t.Fatalf("status = %q, want error", resp.Status)
	}
	var result struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil || result.Message == "" {
		t.Fatalf("error result %s carries no reason and message", resp.Result)
	}
	return result.Reason
}

func TestCommandWithNoBrowserIsAnErrorTheUserHears(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	chromeClient, _ := connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, chromeBundle)
	h.disconnectSSEClient("browser", "chrome", chromeClient)

	resp := dispatch(h, "close_tab")
	if got := failureReason(t, resp); got != dispatchNoBrowser {
		t.Errorf("reason = %q, want %q", got, dispatchNoBrowser)
	}
	p := rec.last(t)
	if p.Reason != dispatchNoBrowser || !p.Show || p.Title == "" {
		t.Errorf("problem = %+v, want a shown %s", p, dispatchNoBrowser)
	}
	if !strings.Contains(p.Phrase, "Chrome") {
		t.Errorf("problem phrase %q should name the browser in front", p.Phrase)
	}
}

func TestCommandWithNoConnectionAtAllIsAnError(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	if got := failureReason(t, dispatch(h, "scroll_down")); got != dispatchNoBrowser {
		t.Errorf("reason = %q, want %q", got, dispatchNoBrowser)
	}
	rec.last(t)
}

func TestCommandToAFullChannelIsAnError(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	_, ch := connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)
	for len(ch) < cap(ch) {
		ch <- ActionEvent{Action: "filler"}
	}
	if got := failureReason(t, dispatch(h, "close_tab")); got != dispatchBusy {
		t.Errorf("reason = %q, want %q", got, dispatchBusy)
	}
	if p := rec.last(t); p.Reason != dispatchBusy {
		t.Errorf("problem reason = %q, want %q", p.Reason, dispatchBusy)
	}
}

func TestDeliveredCommandIsOK(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)
	if resp := dispatch(h, "close_tab"); resp.Status != branchkit.OnActionStatusOk {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if len(rec.seen) != 0 {
		t.Errorf("a delivered command reported %v", rec.seen)
	}
}

// The browser stays in front while its extension goes: the nudge arms from
// the disconnect itself, not from a later app switch that may never come.
func TestFocusedDisconnectArmsTheNudge(t *testing.T) {
	h, _, log := newProblemHost(t)
	chromeClient, _ := connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, chromeBundle)

	h.disconnectSSEClient("browser", "chrome", chromeClient)

	h.mu.Lock()
	timer, bundle := h.state.DisconnectNudgeTimer, h.state.DisconnectNudgeBundle
	h.mu.Unlock()
	if timer == nil || bundle != chromeBundle {
		t.Fatalf("nudge not armed for the browser in front (timer=%v bundle=%q)", timer != nil, bundle)
	}
	timer.Stop()
	h.fireDisconnectNudge(chromeBundle)
	if log.count("nudge") != 1 {
		t.Errorf("nudges = %d, want 1", log.count("nudge"))
	}
}

// Last connection gone: the app-active tag goes too, even when that
// connection was never bound as the focused one.
func TestLastDisconnectClearsTheActiveTag(t *testing.T) {
	h, _, log := newProblemHost(t)
	client, _ := connectBrowser(h, "chrome", chromeBundle)
	focusApp(h, "com.apple.Terminal")
	before := log.count("delete_active")

	h.disconnectSSEClient("browser", "chrome", client)

	if log.count("delete_active") <= before {
		t.Error("the last disconnect must delete plugin.browser.active")
	}
}

// "no such hint": the extension's toast is the visual, so the problem goes to
// the renderers that are not windows — not shown again.
func TestNoSuchHintIsToldWithoutASecondWindow(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"chrome","action":"activate","codeword":"arch bake","ok":false,"detail":"no_such_hint"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	p := rec.last(t)
	if p.Reason != "no_such_hint" || p.Show {
		t.Errorf("problem = %+v, want an unshown no_such_hint", p)
	}
	if !strings.Contains(p.Phrase, "arch bake") {
		t.Errorf("phrase %q should name the hint", p.Phrase)
	}
}

// Every refusal the extension reports on /dispatch-result from a PAGE is told
// the same way as no_such_hint: a problem document, no window (the page shows
// its own toast). restricted_page, which has no page, is below. Only a refusal that left the badges up after a hint activation rolls
// back the hints tag the activate's ClearsTags dropped at match time.
func TestExtensionRefusalsAreToldWithoutAWindow(t *testing.T) {
	cases := []struct {
		name, body, reason, says string
		rearm                    bool
	}{
		{"no such hint", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","ok":false,"detail":"no_such_hint"}`,
			"no_such_hint", "arch bake", true},
		{"reference not found", `{"conn_id":"chrome","action":"resolve_reference","codeword":"save","resolution":"reference","ok":false,"detail":"reference_not_found"}`,
			"reference_not_found", "save", false},
		{"reference not visible", `{"conn_id":"chrome","action":"resolve_reference","codeword":"save","resolution":"reference","ok":false,"detail":"reference_not_visible"}`,
			"reference_not_visible", "save", false},
		{"needs a real key press", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","elem_tag":"input","taken":"skipped","ok":false,"detail":"needs_user_gesture"}`,
			"needs_user_gesture", "Enter", false},
		{"search match off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"search_badge","taken":"skipped","ok":false,"detail":"match off screen"}`,
			"match_off_screen", "off screen", true},
		{"pick chip off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"skipped","ok":false,"detail":"chip off screen"}`,
			"match_off_screen", "off screen", true},
		{"pick swallows a stray codeword", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"skipped","ok":false,"detail":"pick pending — codeword is not a chip"}`,
			"pick_pending", "escape", true},
		// The cases above are an extension older than the holder codes; these are the codes a current one sends. A detail that
		// no longer reads "pick pending" must still be told as the pick.
		{"coded search match off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"search_badge","taken":"skipped","ok":false,"detail":"match_off_screen"}`,
			"match_off_screen", "off screen", true},
		{"coded pick chip off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"skipped","ok":false,"detail":"chip_off_screen"}`,
			"match_off_screen", "off screen", true},
		{"coded pick pending", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"skipped","ok":false,"detail":"pick_pending"}`,
			"pick_pending", "escape", true},
		{"coded extra hint off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"extra_hint","taken":"skipped","ok":false,"detail":"extra_hint_off_screen"}`,
			"extra_hint_off_screen", "out of sight", true},
		// A hint that exists but is out of sight is not "nothing has the hint".
		{"hint off screen", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"live_store","ok":false,"detail":"no_such_hint","unseen":"off_screen"}`,
			"hint_off_screen", "off screen", true},
		{"hint covered", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"live_store","ok":false,"detail":"no_such_hint","unseen":"not_visible"}`,
			"hint_not_visible", "covered", true},
		// A control the page disabled: a mouse couldn't use it either.
		{"hint disabled", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"live_store","ok":false,"detail":"no_such_hint","unseen":"disabled"}`,
			"hint_disabled", "turned that control off", true},
		// An unseen value this plugin does not know reads as before.
		{"unknown unseen reason", `{"conn_id":"chrome","action":"activate","codeword":"arch bake","ok":false,"detail":"no_such_hint","unseen":"elsewhere"}`,
			"no_such_hint", "arch bake", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newDriftHost(t)
			rec := &problemRecorder{}
			h.reportProblem = rec.record
			h.runAsync = func(f func()) { f() }
			putFrame(t, h, frameReq(0, "arch bake"))
			// The activate's manifest ClearsTags, at match time.
			h.state.HintGate.transitionToIdle("clearstags_activate")

			w := httptest.NewRecorder()
			serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/dispatch-result", strings.NewReader(c.body)))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			p := rec.last(t)
			if p.Reason != c.reason || p.Show || p.Title == "" {
				t.Errorf("problem = %+v, want an unshown %s", p, c.reason)
			}
			if !strings.Contains(p.Phrase, c.says) {
				t.Errorf("phrase %q should say %q", p.Phrase, c.says)
			}
			if got := h.state.HintGate.IsActive(); got != c.rearm {
				t.Errorf("hints tag armed = %v after the refusal, want %v", got, c.rearm)
			}
		})
	}
}

// A result that is not a refusal tells the person nothing.
func TestDispatchResultsThatAreNotRefusalsAreNotTold(t *testing.T) {
	for _, body := range []string{
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"registry","elem_tag":"a","taken":"click","ok":true}`,
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"search_badge","taken":"click","ok":true,"detail":"jumped to search match"}`,
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"click","ok":true,"detail":"range pick resolved"}`,
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","elem_tag":"select","taken":"focus","ok":true,"detail":"select_focus_only"}`,
		// The holders' acted codes.
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"search_badge","taken":"click","ok":true,"detail":"search_jumped"}`,
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"range_pick","taken":"click","ok":true,"detail":"range_pick_resolved"}`,
		`{"conn_id":"chrome","action":"activate","codeword":"arch bake","resolution":"extra_hint","taken":"click","ok":true,"detail":"extra_hint_activated"}`,
	} {
		h, rec, _ := newProblemHost(t)
		w := httptest.NewRecorder()
		serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/dispatch-result", strings.NewReader(body)))
		if len(rec.seen) != 0 {
			t.Errorf("%s: told %+v", body, rec.seen)
		}
	}
}

// A command that needs a page, on a tab that has none (a new-tab page, the
// browser's own pages, the stores): the extension reports restricted_page. The
// page cannot toast, so unlike every other refusal the window is SHOWN as well
// as told, and the way off the page is named. It never re-arms the hints tag.
func TestRestrictedPageIsToldAndShownAndNeverReArms(t *testing.T) {
	h, _, _ := newDriftHost(t)
	rec := &problemRecorder{}
	h.reportProblem = rec.record
	h.runAsync = func(f func()) { f() }
	// The state in which a re-arm WOULD arm it: badges up in the focused tab,
	// the tag dropped at match time.
	putFrame(t, h, frameReq(0, "arch bake"))
	h.state.HintGate.transitionToIdle("clearstags_activate")

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"chrome","action":"scroll_down","resolution":"none","taken":"skipped","ok":false,"frame":"","detail":"restricted_page"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	p := rec.last(t)
	if p.Reason != "restricted_page" || !p.Show || p.Title == "" {
		t.Errorf("problem = %+v, want a SHOWN restricted_page", p)
	}
	if !strings.Contains(p.Phrase, "“palette all”") {
		t.Errorf("phrase %q should name the way off the page", p.Phrase)
	}
	if h.state.HintGate.IsActive() {
		t.Error("restricted_page re-armed the hints tag")
	}
}

// The way off is said in the extension's own words: a contributed phrase for
// the palette opener wins over the built-in default.
func TestRestrictedPageNamesTheContributedPalettePhrase(t *testing.T) {
	h, rec, _ := newProblemHost(t)
	h.storeContributedCommands("chrome", []contributedCommand{
		{Action: "toggle_palette", Pattern: "palette {text}"},
		{Action: "toggle_palette", Pattern: "palette everything", Context: "palette"},
		{Action: "toggle_palette", Pattern: "open palette"},
	})
	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/dispatch-result",
		strings.NewReader(`{"conn_id":"chrome","action":"toggle_hints","ok":false,"detail":"restricted_page"}`)))
	if p := rec.last(t); !strings.Contains(p.Phrase, "“open palette”") {
		t.Errorf("phrase %q should name the contributed default-context phrase", p.Phrase)
	}
}
