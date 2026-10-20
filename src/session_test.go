package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
)

// putSession sends one PUT /session through its route, from a connection
// with a live stream (one is registered if the test has none: a session is
// refused without one).
func putSession(t *testing.T, h *Host, req wire.SessionRequest) {
	t.Helper()
	h.mu.Lock()
	if h.state.clientOf(req.ConnID) == "" {
		h.state.conn(req.ConnID).Client = "test-stream-" + req.ConnID
	}
	h.mu.Unlock()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/session", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /session status = %d body %s", rec.Code, rec.Body.String())
	}
}

type sessionTagSeams struct{ caretPuts, caretDeletes, findPuts, videoPuts, queryPuts, mediaPuts, mediaDeletes int }

func captureSessionSeams(h *Host, t *testing.T) *sessionTagSeams {
	t.Helper()
	s := &sessionTagSeams{}
	// newTestHost hosts are per test: no restore needed beyond the test.
	h.putCaretTag = func(string) error { s.caretPuts++; return nil }
	h.deleteCaretTag = func(string) error { s.caretDeletes++; return nil }
	h.putFindTag = func(string) error { s.findPuts++; return nil }
	h.deleteFindTag = func(string) error { return nil }
	h.putVideoModeTag = func(string) error { s.videoPuts++; return nil }
	h.deleteVideoModeTag = func(string) error { return nil }
	h.putAutoScrollTag = func(string) error { return nil }
	h.deleteAutoScrollTag = func(string) error { return nil }
	h.putQueryFieldTag = func(string) error { s.queryPuts++; return nil }
	h.deleteQueryFieldTag = func(string) error { return nil }
	h.putMediaActiveTag = func(string) error { s.mediaPuts++; return nil }
	h.deleteMediaActiveTag = func(string) error { s.mediaDeletes++; return nil }
	return s
}

// One request carries every mode; the same request again writes nothing.
func TestSessionPutAppliesEveryModeAndIsIdempotent(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	s := captureSessionSeams(h, t)
	h.state.FocusedConnID = "conn-a"

	req := wire.SessionRequest{ConnID: "conn-a", Modes: wire.SessionModes{Caret: true, Find: true, Video: true},
		QueryField: true, MediaActive: true}
	putSession(t, h, req)
	putSession(t, h, req)
	if s.caretPuts != 1 || s.findPuts != 1 || s.videoPuts != 1 || s.queryPuts != 1 || s.mediaPuts != 1 {
		t.Fatalf("puts caret=%d find=%d video=%d query=%d media=%d, want one each",
			s.caretPuts, s.findPuts, s.videoPuts, s.queryPuts, s.mediaPuts)
	}
	if h.state.MediaLastConnID != "conn-a" {
		t.Errorf("MediaLastConnID = %q, want conn-a", h.state.MediaLastConnID)
	}

	putSession(t, h, wire.SessionRequest{ConnID: "conn-a"})
	if h.state.CaretTagSet || h.state.FindTagSet || h.state.VideoModeTagSet || h.state.QueryFieldTagSet || h.state.MediaTagSet {
		t.Fatal("an all-off session must release every tag the connection held")
	}
}

// A mode asked for while another browser is in front is deferred, not
// dropped: the request is kept, and focus arriving applies it. The edge
// routes refused it and nothing asked again.
func TestSessionDeferredModeAppliesOnFocusGain(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	s := captureSessionSeams(h, t)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")

	putSession(t, h, wire.SessionRequest{ConnID: "profile-2", Modes: wire.SessionModes{Video: true}})
	if h.state.VideoModeTagSet || s.videoPuts != 0 {
		t.Fatal("a browser behind the focused one must not take an exclusive mode")
	}
	claimFocus(t, h, "profile-2")
	if !h.state.VideoModeTagSet || h.state.VideoModeConnID != "profile-2" {
		t.Fatalf("focus gain must apply the deferred mode: set=%v conn=%q", h.state.VideoModeTagSet, h.state.VideoModeConnID)
	}
}

// The query field needs a KNOWN focused connection (query_field.go): with
// focus unknown, a background tab's autofocused input would shape dictation
// aimed at another app.
func TestSessionQueryFieldNeedsKnownFocus(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	s := captureSessionSeams(h, t)

	putSession(t, h, wire.SessionRequest{ConnID: "conn-a", QueryField: true, Modes: wire.SessionModes{Caret: true}})
	if s.queryPuts != 0 || h.state.QueryFieldTagSet {
		t.Fatal("focus unknown: the query field must not be claimed")
	}
	if !h.state.CaretTagSet {
		t.Fatal("focus unknown: an entered mode fails open (the single-browser case)")
	}
}

// A connection's disconnect releases what it held and drops its session, in
// one place: nothing of it survives to be re-applied.
func TestSessionDroppedWithItsConnection(t *testing.T) {
	h, _ := newRoutingHost(t)
	client, _ := connectBrowser(h, "conn-v", chromeBundle)
	captureSessionSeams(h, t)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "conn-v")

	putSession(t, h, wire.SessionRequest{ConnID: "conn-v", Modes: wire.SessionModes{Caret: true}, MediaActive: true})
	if !h.state.CaretTagSet || !h.state.MediaTagSet {
		t.Fatal("setup: caret and media held")
	}
	h.disconnectSSEClient("browser", "conn-v", client)
	if h.state.CaretTagSet || h.state.MediaTagSet {
		t.Fatalf("disconnect must release the connection's tags: caret=%v media=%v", h.state.CaretTagSet, h.state.MediaTagSet)
	}
	if _, kept := h.state.Conns["conn-v"]; kept {
		t.Fatal("the connection's state must be dropped whole")
	}
}

// A switch between exclusive modes releases the old before taking the new:
// the platform never holds two exclusive tags at once.
func TestSessionReleasesBeforeTaking(t *testing.T) {
	h := newTestHost()
	newCaretTestState(h)
	captureSessionSeams(h, t)
	var order []string
	h.putCaretTag = func(string) error { order = append(order, "put caret"); return nil }
	h.deleteVideoModeTag = func(string) error { order = append(order, "delete video"); return nil }
	h.state.FocusedConnID = "conn-a"

	putSession(t, h, wire.SessionRequest{ConnID: "conn-a", Modes: wire.SessionModes{Video: true}})
	order = nil
	putSession(t, h, wire.SessionRequest{ConnID: "conn-a", Modes: wire.SessionModes{Caret: true}})
	if len(order) != 2 || order[0] != "delete video" || order[1] != "put caret" {
		t.Fatalf("order = %v, want [delete video, put caret]", order)
	}
}

// A session from a connection with no live stream is refused, and leaves
// nothing behind: only a stream's disconnect drops a Conn, so one created
// here would hold media_active (and any slot it took) for good.
func TestSessionRefusedWithoutALiveStream(t *testing.T) {
	h, _ := newRoutingHost(t)
	s := captureSessionSeams(h, t)
	body, _ := json.Marshal(wire.SessionRequest{ConnID: "phantom", MediaActive: true, Modes: wire.SessionModes{Caret: true}})
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/session", strings.NewReader(string(body))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if _, made := h.state.Conns["phantom"]; made {
		t.Fatal("a refused session must not create a Conn")
	}
	if h.state.MediaTagSet || h.state.CaretTagSet || s.mediaPuts != 0 {
		t.Fatalf("a stream-less session took tags: media=%v caret=%v", h.state.MediaTagSet, h.state.CaretTagSet)
	}

	// The same request once the stream is up is applied.
	connectBrowser(h, "phantom", chromeBundle)
	putSession(t, h, wire.SessionRequest{ConnID: "phantom", MediaActive: true})
	if !h.state.MediaTagSet {
		t.Fatal("a live connection's session must be applied")
	}
}

// A browser in the background while another app is in front must not take an
// exclusive mode: FocusedConnID is "" then exactly as when focus is unknown,
// and the window-blur re-send of the session used to re-take the video mode
// the unfocus had just drained, suppressing the terminal's commands. The
// request is kept, and focus coming back applies it.
func TestSessionExclusiveModeNotTakenWhileAnotherAppIsInFront(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "conn-v", chromeBundle)
	s := captureSessionSeams(h, t)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "conn-v")
	putSession(t, h, wire.SessionRequest{ConnID: "conn-v", Modes: wire.SessionModes{Video: true, Caret: true}, MediaActive: true})
	if !h.state.VideoModeTagSet {
		t.Fatal("setup: video mode held while focused")
	}

	focusApp(h, "com.apple.Terminal")
	if h.state.VideoModeTagSet || h.state.CaretTagSet {
		t.Fatal("setup: the unfocus drains the exclusive modes")
	}
	puts := s.videoPuts
	// The extension's blur re-send: the same levels, from the background.
	putSession(t, h, wire.SessionRequest{ConnID: "conn-v", Modes: wire.SessionModes{Video: true, Caret: true}, MediaActive: true})
	if h.state.VideoModeTagSet || h.state.CaretTagSet || s.videoPuts != puts {
		t.Fatalf("a background browser re-took exclusive modes: video=%v caret=%v", h.state.VideoModeTagSet, h.state.CaretTagSet)
	}
	if !h.state.MediaTagSet {
		t.Fatal("media_active is asserted from the background by design")
	}

	focusApp(h, chromeBundle)
	if !h.state.VideoModeTagSet || h.state.VideoModeConnID != "conn-v" {
		t.Fatal("focus returning must apply the kept session")
	}
}

// The media pick follows news about media — media starting in a browser, or
// the user being in a browser with media — not every PUT that repeats
// media_active. A background browser's connect replay used to take the pick
// from the browser the user had just been watching.
func TestSessionMediaPickMovesOnlyOnMediaNews(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)
	captureSessionSeams(h, t)
	putSession(t, h, wire.SessionRequest{ConnID: "fox", MediaActive: true})
	putSession(t, h, wire.SessionRequest{ConnID: "chrome", MediaActive: true})
	if h.state.MediaLastConnID != "chrome" {
		t.Fatalf("setup: media starting in chrome moves the pick, got %q", h.state.MediaLastConnID)
	}

	// The user watches in Firefox, then moves to the terminal.
	focusApp(h, firefoxBundle)
	if h.state.MediaLastConnID != "fox" {
		t.Fatalf("focus on a browser with media must move the pick, got %q", h.state.MediaLastConnID)
	}
	focusApp(h, "com.apple.Terminal")

	// Chrome's stream reconnects in the background and replays its session.
	putSession(t, h, wire.SessionRequest{ConnID: "chrome", MediaActive: true})
	if h.state.MediaLastConnID != "fox" {
		t.Fatalf("a replay that repeats media_active stole the pick: %q", h.state.MediaLastConnID)
	}

	// Media newly starting in the background browser is news.
	putSession(t, h, wire.SessionRequest{ConnID: "chrome"})
	putSession(t, h, wire.SessionRequest{ConnID: "chrome", MediaActive: true})
	if h.state.MediaLastConnID != "chrome" {
		t.Fatalf("media starting moves the pick, got %q", h.state.MediaLastConnID)
	}
}

// browser.caret_updated follows the TAG, wherever it moves: a caret asked for
// in the background is announced when focus takes it, and the unfocus drain
// announces its release. It used to be emitted by the session route alone,
// for the request — so a deferred caret was announced as active=false and
// its later take on focus gain never at all.
func TestCaretUpdatedFollowsTheTag(t *testing.T) {
	h, _ := newRoutingHost(t)
	connectBrowser(h, "profile-1", chromeBundle)
	connectBrowser(h, "profile-2", chromeBundle)
	captureSessionSeams(h, t)
	type ev struct {
		conn   string
		active bool
	}
	var got []ev
	h.appendEvent = func(typ string, data map[string]any) {
		if typ == "browser.caret_updated" {
			got = append(got, ev{data["conn_id"].(string), data["active"].(bool)})
		}
	}
	focusApp(h, chromeBundle)
	claimFocus(t, h, "profile-1")

	putSession(t, h, wire.SessionRequest{ConnID: "profile-2", Modes: wire.SessionModes{Caret: true}})
	if len(got) != 0 {
		t.Fatalf("a deferred caret announced %v before any tag moved", got)
	}
	claimFocus(t, h, "profile-2")
	if len(got) != 1 || got[0] != (ev{"profile-2", true}) {
		t.Fatalf("events after focus gain = %v, want [profile-2 active]", got)
	}
	focusApp(h, "com.apple.Terminal")
	if len(got) != 2 || got[1] != (ev{"profile-2", false}) {
		t.Fatalf("events after unfocus = %v, want the release announced for profile-2", got)
	}
}
