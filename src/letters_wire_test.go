package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Actions leave on the stream in letters: the plugin translates spoken hint
// params with its own alphabet, the one current when the command matched.

// alphabetOf builds a 26-word alphabet from a word per letter, a-z.
func alphabetOf(words ...string) [26]string {
	var out [26]string
	copy(out[:], words)
	return out
}

var (
	alphabetOne = alphabetOf("arch", "bat", "cape", "drum", "each", "fine", "gust", "harp", "sit", "jury",
		"crunch", "look", "made", "near", "odd", "pit", "quench", "red", "sun", "trap",
		"urge", "vest", "whale", "plex", "yank", "zip")
	// alphabetTwo reuses alphabetOne's words on different letters, so a word
	// translated under the wrong alphabet gives a wrong letter, not the same one.
	alphabetTwo = alphabetOf("bat", "arch", "drum", "cape", "fine", "each", "harp", "gust", "jury", "sit",
		"look", "crunch", "near", "made", "pit", "odd", "red", "quench", "trap", "sun",
		"vest", "urge", "plex", "whale", "zip", "yank")
)

func TestParamsToLetters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"codeword pair", `{"codeword":"cape glad"}`, `{"codeword":"c glad"}`},
		{"codeword known pair", `{"codeword":"cape gust"}`, `{"codeword":"c g"}`},
		{"prefix word word2", `{"prefix":"arch","word":"bat","word2":"zip"}`, `{"prefix":"a","word":"b","word2":"z"}`},
		{"letters pass through", `{"codeword":"c g","prefix":"c"}`, `{"codeword":"c g","prefix":"c"}`},
		{"unknown word passes through", `{"prefix":"gamma"}`, `{"prefix":"gamma"}`},
		{"sealed pair carries its token", `{"prefix_letter":"c","suffix_letter":"g","prefix_word":"cape","suffix_word":"gust"}`,
			`{"codeword":"c g","prefix_letter":"c","suffix_letter":"g","prefix_word":"cape","suffix_word":"gust"}`},
		{"targets each translated", `{"targets":[{"codeword":"arch bat"},{"prefix_letter":"z","suffix_letter":"y"}]}`,
			`{"targets":[{"codeword":"a b"},{"codeword":"z y","prefix_letter":"z","suffix_letter":"y"}]}`},
		{"other params untouched", `{"action":"scroll_down","amount":3}`, `{"action":"scroll_down","amount":3}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got, want map[string]any
			if err := json.Unmarshal([]byte(c.in), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			changed := paramsToLetters(got, alphabetOne)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("paramsToLetters(%s) = %v, want %v", c.in, got, want)
			}
			if changed != (c.in != c.want) {
				t.Fatalf("changed = %v for %s", changed, c.in)
			}
		})
	}
}

// An event with no hint params goes out byte for byte as it came: numbers
// keep their exact form and nothing is re-encoded.
func TestInLettersLeavesOtherEventsAlone(t *testing.T) {
	h, _ := newRoutingHost(t)
	h.applyAlphabet(alphabetOne)
	ev := ActionEvent{Action: "set_tab", Params: json.RawMessage(`{"tab_id":12345678901234567}`)}
	if got := h.inLetters(ev); string(got.Params) != string(ev.Params) {
		t.Fatalf("params = %s, want %s unchanged", got.Params, ev.Params)
	}
}

// The alphabet swaps between two spoken commands. Each action reaches the
// browser in letters under the alphabet current when it matched: the same
// words mean different letters before and after the swap.
func TestActionsGoOutInLettersAcrossAlphabetSwap(t *testing.T) {
	h, _ := newRoutingHost(t)
	_, ch := connectBrowser(h, "c1", chromeBundle)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "c1")
	received(ch)

	send := func(params string) map[string]any {
		t.Helper()
		resp, _ := h.handleOnAction(&branchkit.OnActionRequest{
			Action: "browser.stash",
			Params: json.RawMessage(params),
		})
		if r, _ := resp.(branchkit.OnActionResponse); r.Status != branchkit.OnActionStatusOk {
			t.Fatalf("dispatch status = %v", resp)
		}
		var got map[string]any
		for {
			select {
			case ev := <-ch:
				if ev.Action != "stash" {
					continue
				}
				if err := json.Unmarshal(ev.Params, &got); err != nil {
					t.Fatal(err)
				}
				return got
			default:
				t.Fatal("no stash event on the stream")
				return nil
			}
		}
	}

	h.applyAlphabet(alphabetOne)
	first := send(`{"codeword":"arch bat","word":"cape","targets":[{"codeword":"drum each"}]}`)
	if first["codeword"] != "a b" || first["word"] != "c" {
		t.Fatalf("before the swap: %v, want codeword \"a b\", word \"c\"", first)
	}
	if tg := first["targets"].([]any)[0].(map[string]any); tg["codeword"] != "d e" {
		t.Fatalf("before the swap: target %v, want codeword \"d e\"", tg)
	}

	h.applyAlphabet(alphabetTwo)
	second := send(`{"codeword":"arch bat","word":"cape","targets":[{"codeword":"drum each"}]}`)
	if second["codeword"] != "b a" || second["word"] != "d" {
		t.Fatalf("after the swap: %v, want codeword \"b a\", word \"d\"", second)
	}
	if tg := second["targets"].([]any)[0].(map[string]any); tg["codeword"] != "c f" {
		t.Fatalf("after the swap: target %v, want codeword \"c f\"", tg)
	}
}

// The mid-codeword narrow (a synthetic noop) carries its prefix as a letter
// too, under the alphabet of the moment.
func TestNoopPrefixGoesOutAsLetter(t *testing.T) {
	h, _ := newRoutingHost(t)
	_, ch := connectBrowser(h, "c1", chromeBundle)
	focusApp(h, chromeBundle)
	claimFocus(t, h, "c1")
	received(ch)

	for _, tc := range []struct {
		alphabet [26]string
		want     string
	}{{alphabetOne, "c"}, {alphabetTwo, "d"}} {
		h.applyAlphabet(tc.alphabet)
		h.handleCaptureProgress(branchkit.CaptureProgressEventParams{
			OwnerPlugin: "browser",
			NextCapture: "suffix",
			Captured:    map[string]json.RawMessage{"prefix": json.RawMessage(`{"codeword":"cape","letter":"c"}`)},
		})
		var prefix string
		for done := false; !done; {
			select {
			case ev := <-ch:
				if ev.Action == "noop" {
					prefix = ev.Param("prefix")
					done = true
				}
			default:
				t.Fatal("no noop event on the stream")
			}
		}
		if prefix != tc.want {
			t.Fatalf("noop prefix = %q, want %q", prefix, tc.want)
		}
	}
}

// --- Letters on the wire, outbound: grammar and range-pick ---
//
// A frame's grammar and a range-pick arrive in letters. The plugin stores the
// letters and projects the spoken words with the alphabet installed at
// projection time, so an alphabet swap reprojects what is stored: the
// collections carry the new words with no request from the extension.

// hintEntries is the set of suffix keys last synced to prefix's collection
// (base, or its _strict companion with strict).
func hintEntries(pushes map[string]map[string]any, prefix string, strict bool) map[string]bool {
	name := hintCollection("browser", prefix)
	if strict {
		name += "_strict"
	}
	out := map[string]bool{}
	for suffix := range pushes[name] {
		out[suffix] = true
	}
	return out
}

func setOf(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// letterFrame is a frame request sent in letters; strict names the tokens in
// the strict viewport.
func letterFrame(frame int, tokens []string, strict ...string) GrammarFrameRequest {
	req := GrammarFrameRequest{
		ConnID: "chrome", TabID: driftTab, FrameID: frame, WindowID: 1, Visible: true,
		HintVisibility: "always",
	}
	inStrict := setOf(strict...)
	for i, tok := range tokens {
		req.Elements = append(req.Elements, GrammarElement{
			ID: i + 1, Type: "link", Codeword: tok, InStrictViewport: inStrict[tok],
		})
	}
	return req
}

// The alphabet swaps partway through a session. The frame's letters were sent
// once, before the swap; afterwards the per-prefix collections, their _strict
// companions and the prefix list carry the new words, the old words'
// collections are emptied, and no grammar request was needed for any of it.
func TestGrammarLettersReprojectAcrossAlphabetSwap(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.applyAlphabet(alphabetOne)
	grammarRequests := 0
	put := func(req GrammarFrameRequest) GrammarFrameResponse {
		grammarRequests++
		return putFrame(t, h, req)
	}

	resp := put(letterFrame(0, []string{"a b", "c d", "z y"}, "a b", "c d"))
	if resp.Result != "ok" || len(resp.Failed) != 0 {
		t.Fatalf("frame in letters: %+v, want ok with nothing failed", resp)
	}
	// alphabetOne: a=arch b=bat c=cape d=drum y=yank z=zip.
	if got := hintEntries(pushes, "arch", false); !mapsEqual(got, setOf("bat")) {
		t.Fatalf("before the swap hints_arch = %v, want {bat}", got)
	}
	if got := hintEntries(pushes, "cape", true); !mapsEqual(got, setOf("drum")) {
		t.Fatalf("before the swap hints_cape_strict = %v, want {drum}", got)
	}
	if got := hintEntries(pushes, "zip", true); len(got) != 0 {
		t.Fatalf("a band-only entry reached _strict: %v", got)
	}

	h.applyAlphabet(alphabetTwo)

	if grammarRequests != 1 {
		t.Fatalf("grammar requests = %d, want the one before the swap", grammarRequests)
	}
	// alphabetTwo: a=bat b=arch c=drum d=cape y=zip z=yank.
	for prefix, want := range map[string]map[string]bool{
		"bat": setOf("arch"), "drum": setOf("cape"), "yank": setOf("zip"),
	} {
		if got := hintEntries(pushes, prefix, false); !mapsEqual(got, want) {
			t.Errorf("after the swap hints_%s = %v, want %v", prefix, got, want)
		}
	}
	for prefix, want := range map[string]map[string]bool{
		"bat": setOf("arch"), "drum": setOf("cape"), "yank": {},
	} {
		if got := hintEntries(pushes, prefix, true); !mapsEqual(got, want) {
			t.Errorf("after the swap hints_%s_strict = %v, want %v", prefix, got, want)
		}
	}
	// The suffix entry's own fields follow the new words too: the activate
	// command binds {suffix.codeword} and {suffix.letter}.
	entry, _ := pushes[hintCollection("browser", "bat")]["arch"].(map[string]any)
	if entry["codeword"] != "arch" || entry["letter"] != "b" {
		t.Errorf("hints_bat[arch] = %v, want codeword arch, letter b", entry)
	}
	// The old words' collections are emptied, not stranded.
	for _, prefix := range []string{"arch", "cape", "zip"} {
		if got := hintEntries(pushes, prefix, false); len(got) != 0 {
			t.Errorf("after the swap the old hints_%s still holds %v", prefix, got)
		}
	}
	if live, _ := h.livePrefixes("browser"); !mapsEqual(live, setOf("bat", "drum", "yank")) {
		t.Errorf("prefixes holding records = %v, want bat, drum, yank", live)
	}
	list := pushes[hintCollection("browser", "prefix")]
	if len(list) != 3 || list["bat"] == nil || list["drum"] == nil || list["yank"] == nil {
		t.Errorf("prefix list = %v, want bat, drum, yank", list)
	}
	if e, _ := list["bat"].(map[string]string); e["letter"] != "a" {
		t.Errorf("prefix list bat = %v, want letter a", list["bat"])
	}
	// The failed list of the next request answers in letters, as sent.
	resp = put(letterFrame(0, []string{"a b", "arch bat"}))
	if len(resp.Failed) != 1 || resp.Failed[0].Codeword != "arch bat" || resp.Failed[0].Reason != "not_a_letter" {
		t.Errorf("failed = %+v, want the spoken pair refused as sent", resp.Failed)
	}
}

// A frame that arrives before the alphabet has loaded is stored whole, not
// refused: nothing projects until the alphabet arrives, and then everything
// does, with no resend.
//
// The hint commands themselves (activate and the rest) are built from the
// alphabet, so a cold start that sees grammar first registers them only when
// the alphabet arrives: nothing resends the frame to do it.
func TestGrammarLettersWaitInThePluginForTheAlphabet(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.codewordsMu.Lock()
	h.codewords = [26]string{}
	h.codewordsMu.Unlock()
	h.mu.Lock()
	h.state.HintCommandSkeletonRegistered = false // a cold start
	h.mu.Unlock()
	h.runAsync = func(f func()) { f() }
	var pushedGroups []string
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		if len(cmds) > 0 {
			pushedGroups = append(pushedGroups, group)
		}
		return len(cmds), nil
	}

	resp := putFrame(t, h, letterFrame(0, []string{"a b"}, "a b"))
	if len(resp.Failed) != 0 || resp.CommittedCodewords != 1 {
		t.Fatalf("frame before the alphabet: %+v, want it stored whole", resp)
	}
	if len(pushes[hintCollection("browser", "arch")]) != 0 {
		t.Fatalf("projected with no alphabet: %v", pushes)
	}
	if h.state.HintCommandSkeletonRegistered {
		t.Fatal("the hint commands were claimed with no alphabet to build them from")
	}

	h.applyAlphabet(alphabetOne)
	if got := hintEntries(pushes, "arch", true); !mapsEqual(got, setOf("bat")) {
		t.Errorf("once the alphabet arrives hints_arch_strict = %v, want {bat}", got)
	}
	if !h.state.HintCommandSkeletonRegistered || len(pushedGroups) == 0 {
		t.Errorf("once the alphabet arrives the hint commands must be registered (pushed %v)", pushedGroups)
	}
}

// An alphabet change that lands while a grammar request is in flight, after
// the request began and before its commit, is the one the commit names its
// prefixes under: the frame's new codewords reach the new words' collections.
// (The swap is run from inside the request, at the hint-command push that
// precedes the commit.)
func TestGrammarCommitFollowsAnAlphabetSwapMidRequest(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.applyAlphabet(alphabetOne)
	putFrame(t, h, letterFrame(0, []string{"a b"}, "a b"))

	h.mu.Lock()
	h.state.HintCommandSkeletonRegistered = false
	h.mu.Unlock()
	swapped := false
	h.pushCommandGroupRPC = func(string, []branchkit.CommandSpec) (int, error) {
		if !swapped {
			swapped = true
			h.applyAlphabet(alphabetTwo)
		}
		return 0, nil
	}
	putFrame(t, h, letterFrame(0, []string{"a b", "c d"}, "a b", "c d"))
	if !swapped {
		t.Fatal("setup: the swap did not run inside the request")
	}
	// alphabetTwo: a=bat b=arch c=drum d=cape.
	if got := hintEntries(pushes, "drum", true); !mapsEqual(got, setOf("cape")) {
		t.Errorf("the new codeword c d: hints_drum_strict = %v, want {cape}", got)
	}
	if got := hintEntries(pushes, "bat", true); !mapsEqual(got, setOf("arch")) {
		t.Errorf("hints_bat_strict = %v, want {arch}", got)
	}
	if e, _ := pushes[hintCollection("browser", "prefix")]["drum"].(map[string]string); e["letter"] != "c" {
		t.Errorf("prefix list drum = %v, want letter c", pushes[hintCollection("browser", "prefix")]["drum"])
	}
}

// A range-pick arms in letters and keeps narrowing across an alphabet swap:
// the chip stays the only entry, now under the new words.
func TestRangePickLettersNarrowAcrossAlphabetSwap(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.applyAlphabet(alphabetOne)
	putFrame(t, h, letterFrame(0, []string{"a b", "a c", "d e"}, "a b", "a c", "d e"))

	w := httptest.NewRecorder()
	serveRoute(h, w, httptest.NewRequest(http.MethodPost, "/range-pick",
		strings.NewReader(`{"conn_id":"chrome","tab_id":7,"codewords":["a  c"]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("/range-pick status %d: %s", w.Code, w.Body.String())
	}
	// alphabetOne: a=arch c=cape. Only the chip projects.
	if got := hintEntries(pushes, "arch", true); !mapsEqual(got, setOf("cape")) {
		t.Fatalf("armed, hints_arch_strict = %v, want only the chip {cape}", got)
	}
	if got := hintEntries(pushes, "drum", false); len(got) != 0 {
		t.Fatalf("armed, a non-chip prefix still projects: %v", got)
	}

	h.applyAlphabet(alphabetTwo)
	// alphabetTwo: a=bat c=drum.
	if got := hintEntries(pushes, "bat", true); !mapsEqual(got, setOf("drum")) {
		t.Errorf("after the swap hints_bat_strict = %v, want only the chip {drum}", got)
	}
	if got := hintEntries(pushes, "bat", false); !mapsEqual(got, setOf("drum")) {
		t.Errorf("after the swap hints_bat = %v, want only the chip {drum}", got)
	}
	if got := hintEntries(pushes, "arch", true); len(got) != 0 {
		t.Errorf("after the swap the old hints_arch_strict still holds %v", got)
	}
	if got := hintEntries(pushes, "cape", false); len(got) != 0 {
		t.Errorf("after the swap a non-chip entry (d e = cape fine) projects: %v", got)
	}
}

// During a calibration trial the fixture owns the collections, so a retune's
// alphabet change does not reproject the browser's grammar over it; the
// release puts the browser's grammar back, under the alphabet as it stands.
func TestAlphabetSwapDuringCalibrationWaitsForRelease(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.applyAlphabet(alphabetOne)
	putFrame(t, h, letterFrame(0, []string{"a b"}, "a b"))

	handle, err := h.applyHintsFixture("browser")
	if err != nil {
		t.Fatal(err)
	}
	fixture := len(pushes[hintCollection("browser", "arch")])
	h.applyAlphabet(alphabetTwo)
	if got := len(pushes[hintCollection("browser", "arch")]); got != fixture {
		t.Fatalf("the swap reprojected over the fixture: hints_arch has %d entries, want the fixture's %d", got, fixture)
	}

	if err := h.releaseHintsFixture("browser", handle); err != nil {
		t.Fatal(err)
	}
	// alphabetTwo: a=bat b=arch.
	if got := hintEntries(pushes, "bat", true); !mapsEqual(got, setOf("arch")) {
		t.Errorf("after the release hints_bat_strict = %v, want {arch}", got)
	}
	if !h.state.HintGate.IsActive() {
		t.Error("after the release the focused tab's grammar must arm the hints tag again")
	}
}

// The swap's new words reach the recognizer: through the real replace path,
// the reprojection schedules a vocabulary commit, with no grammar request.
func TestAlphabetSwapSchedulesVocabularyCommit(t *testing.T) {
	h, _, _ := newDriftHost(t)
	h.syncPerPrefix = h.syncPerPrefixDefault
	h.syncPerPrefixStrict = h.syncPerPrefixStrictDefault
	h.syncPrefixList = h.syncPrefixListDefault
	captureHintRPC(h, t)
	h.applyAlphabet(alphabetOne)
	putFrame(t, h, letterFrame(0, []string{"a b"}, "a b"))
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
	pending() // whatever the frame scheduled

	h.applyAlphabet(alphabetTwo)
	if !pending() {
		t.Error("the swap's new words must reach the recognizer: no vocabulary commit was scheduled")
	}
}

// The alphabet can arrive inside the startup command push: its hint group
// refreshes the alphabet mirror while holding pushMu, and the mirror's
// OnChange runs applyAlphabet on that same goroutine. Registering the hint
// commands from there must not take pushMu again on it (a self-deadlock that
// would block every later command push).
func TestAlphabetArrivingInsideACommandPushDoesNotDeadlock(t *testing.T) {
	h, _, _ := newDriftHost(t)
	h.codewordsMu.Lock()
	h.codewords = [26]string{}
	h.codewordsMu.Unlock()
	h.mu.Lock()
	h.state.HintCommandSkeletonRegistered = false
	h.mu.Unlock()
	putFrame(t, h, letterFrame(0, []string{"a b"}, "a b"))

	registered := make(chan struct{}, 1)
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		if group == groupHints && len(cmds) > 0 {
			select {
			case registered <- struct{}{}:
			default:
			}
		}
		return len(cmds), nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.pushMu.Lock() // as pushAllCommandGroups holds it
		h.applyAlphabet(alphabetOne)
		h.pushMu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("applyAlphabet under pushMu did not return: the hint registration re-took pushMu on the same goroutine")
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("the hint commands were never registered after the push released pushMu")
	}
}

// A page whose hints all live in an iframe that has gone quiet past the stale
// TTL keeps them projected (the empty-source guard). An alphabet change must
// move them to the new words too: its badges show the new words, and the
// content key (letters) is unchanged, so the frame sends nothing.
func TestAlphabetSwapReprojectsAnIdleIframesHints(t *testing.T) {
	h, pushes, _ := newDriftHost(t)
	h.applyAlphabet(alphabetOne)
	putFrame(t, h, letterFrame(5, []string{"a b"}, "a b"))
	h.mu.Lock()
	h.state.FrameSessions[frameKey{ConnID: "chrome", TabID: driftTab, FrameID: 5}].LastBatchAt =
		time.Now().Add(-2 * staleFrameSessionTTL)
	h.mu.Unlock()
	h.sweepStaleFrameSessionsOnce(time.Now())
	if got := hintEntries(pushes, "arch", true); !mapsEqual(got, setOf("bat")) {
		t.Fatalf("setup: the idle iframe's hints must stay projected, hints_arch_strict = %v", got)
	}

	h.applyAlphabet(alphabetTwo)
	// alphabetTwo: a=bat b=arch.
	if got := hintEntries(pushes, "bat", true); !mapsEqual(got, setOf("arch")) {
		t.Errorf("after the swap hints_bat_strict = %v, want {arch}", got)
	}
	if got := hintEntries(pushes, "arch", true); len(got) != 0 {
		t.Errorf("after the swap the old hints_arch_strict still holds %v", got)
	}
}
