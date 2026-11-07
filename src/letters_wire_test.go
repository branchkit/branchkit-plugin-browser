package main

import (
	"encoding/json"
	"reflect"
	"testing"

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
