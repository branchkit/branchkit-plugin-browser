package main

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
)

// Phonetic codewords matching BranchKit's voice plugin alphabet (a-z).
// Decoded from a mirror of the voice-owned `alphabet` collection.
// Protected by codewordsMu for concurrent access from grammar handlers.
// Tests run with plugin==nil and pre-populate this global directly.
var ()

// initCodewordMirror wires the alphabet mirror. Must be called before
// plugin.Run(). Freshness: fetch at on_ready, refetch on alphabet
// update events (including calibration retune — collection overrides
// emit `_platform.collection.updated`), plus forced refreshCodewords()
// at call sites that need synchronous freshness.
func (h *Host) initCodewordMirror() {
	h.codewordMirror = h.plugin.MirrorCollection("alphabet")
	h.codewordMirror.OnChange(func() {
		var data []struct {
			Letter string `json:"letter"`
			Word   string `json:"codeword"`
		}
		if err := h.codewordMirror.Decode(&data); err != nil {
			branchkit.Logf("browser", "alphabet mirror: decode failed: %v", err)
			return
		}
		var updated [26]string
		for _, e := range data {
			letter := strings.ToLower(e.Letter)
			if len(letter) == 1 {
				idx := int(letter[0]) - int('a')
				if idx >= 0 && idx < 26 {
					updated[idx] = e.Word
				}
			}
		}
		h.applyAlphabet(updated)
		h.pushSealedAlpha(data)
	})
}

// applyAlphabet installs a decoded alphabet as the plugin's codewords and,
// when it changed, pushes it to every open stream as an `alphabet` event —
// the connect-time event, re-sent. Without the push a connected extension
// kept translating hint letters with the old words until it reconnected, and
// the plugin dropped those codewords as codeword_not_in_alphabet. The push is
// also what re-requests the frames sent under the old words: the extension
// stores a changed alphabet, and every content script answers that storage
// change by relabelling and re-sending its frame's grammar (resyncFrame,
// reason alphabet_change). No `reactivate` with it: the plugin's frame
// sessions are intact, and reactivate reaches only the active tab.
//
// Codewords are installed before the push, so a grammar re-sent under the new
// words validates against them. A half-loaded alphabet (blank slots) is never
// pushed, as at connect (snapshotCodewords).
func (h *Host) applyAlphabet(updated [26]string) {
	h.codewordsMu.Lock()
	changed := h.codewords != updated
	h.codewords = updated
	h.codewordsMu.Unlock()
	if !changed {
		return
	}
	words := h.snapshotCodewords()
	if words == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == nil {
		return
	}
	for _, ch := range h.state.SSEAlphabet {
		// Latest wins: replace an undelivered older alphabet. Only this
		// function sends, under mu, so the drain cannot race another send.
		select {
		case <-ch:
		default:
		}
		ch <- words
	}
}

// pushSealedAlpha maintains `browser_alpha` — the browser-owned sealed
// mirror of the voice alphabet with OBJECT payloads ({letter, codeword}),
// so the pull-resolution activate's captures bind the full entry (the
// voice-owned `alphabet` binds its value_field, the bare letter — required
// by spell/press captures and not ours to change). Grammar-seeded from
// `alphabet` in plugin.json, so Layer 1 stays static; content changes only
// on an alphabet swap.
func (h *Host) pushSealedAlpha(data []struct {
	Letter string `json:"letter"`
	Word   string `json:"codeword"`
}) {
	type entry struct {
		Letter   string `json:"letter"`
		Codeword string `json:"codeword"`
	}
	records := make([]branchkit.CollectionPutEntry, 0, len(data))
	for _, e := range data {
		if e.Word == "" || e.Letter == "" {
			continue
		}
		raw, err := json.Marshal(entry{Letter: e.Letter, Codeword: e.Word})
		if err != nil {
			branchkit.Logf("browser", "plugin.browser.alpha: marshal %q: %v", e.Word, err)
			return
		}
		records = append(records, branchkit.CollectionPutEntry{ID: e.Word, Payload: raw})
	}
	if len(records) == 0 {
		return
	}
	if _, err := h.plugin.Replace("plugin.browser.alpha", records, branchkit.ScopeCollection()); err != nil {
		branchkit.Logf("browser", "plugin.browser.alpha push failed: %v", err)
	}
}

// refreshCodewords forces a synchronous alphabet refetch for call sites
// that need freshness at a specific moment (mid-walk retune pickup, SSE
// cold start, per-batch validation). No-op when the mirror isn't wired
// (tests). An unpopulated store is the mirror's silent boot-race no-op.
func (h *Host) refreshCodewords() {
	if h.codewordMirror == nil {
		return
	}
	if err := h.codewordMirror.Refresh(); err != nil {
		branchkit.Logf("browser", "WARNING: alphabet store unavailable: %v", err)
	}
}

// snapshotCodewords returns a defensive copy of the active codeword list.
// Returns nil if the alphabet hasn't been loaded yet (any blank slot means
// the [26]string is still in its zero state) — callers shouldn't push a
// half-initialized alphabet to the extension.
func (h *Host) snapshotCodewords() []string {
	h.codewordsMu.RLock()
	defer h.codewordsMu.RUnlock()
	for _, w := range h.codewords {
		if w == "" {
			return nil
		}
	}
	out := make([]string, 26)
	copy(out, h.codewords[:])
	return out
}

// --- Codeword → letter mapping ---
//
// Codewords arrive from the extension as full strings ("arch", "zoo arch").
// The collection-key flow uses short letter codes ("a", "za") for compactness
// and as part of action params. This helper maps spoken codewords back to
// letters by looking up each word's index in the alphabet.

// codewordToLetter maps a spoken codeword to its short letter form. "arch"
// becomes "a"; "zoo arch" becomes "za". Returns "" if any word in the
// codeword isn't present in the alphabet — caller should skip the element.
func codewordToLetter(codeword string, words [26]string) string {
	parts := strings.Fields(codeword)
	if len(parts) == 0 {
		return ""
	}
	var letter strings.Builder
	for _, p := range parts {
		idx := -1
		for i, w := range words {
			if w == p {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ""
		}
		letter.WriteByte(byte('a' + idx))
	}
	return letter.String()
}

// --- Letters on the wire (actions) ---
//
// The extension keys hints on letter tokens ("c g" for a pair, "c" for one
// letter); spoken words are this plugin's business. So every action leaves on
// the stream in letters: inLetters rewrites the hint params of an outgoing
// event with the alphabet installed now, the one whose words the command just
// matched. An alphabet swap therefore needs nothing from the extension for
// actions: the next action is translated under the new words.

// spokenWordToLetter maps one alphabet word to its letter ("cape" -> "c").
// A word the alphabet does not hold passes through unchanged, so a param that
// is already a letter (or a word some other surface owns, such as a palette
// prefix) is left as it was.
func spokenWordToLetter(word string, words [26]string) string {
	if word == "" {
		return word
	}
	for i, w := range words {
		if w == word {
			return string(rune('a' + i))
		}
	}
	return word
}

// spokenCodewordToToken maps a spoken codeword to its letter token, word by
// word ("cape glad" -> "c g"), each word as spokenWordToLetter does.
func spokenCodewordToToken(codeword string, words [26]string) string {
	parts := strings.Fields(codeword)
	for i, p := range parts {
		parts[i] = spokenWordToLetter(p, words)
	}
	return strings.Join(parts, " ")
}

// paramsToLetters rewrites one action's hint params in place and reports
// whether anything changed:
//   - codeword: a spoken codeword, to its letter token;
//   - prefix, word, word2: one spoken word each, to its letter;
//   - prefix_letter + suffix_letter (the sealed pair match, already letters):
//     codeword becomes their token, the identity the extension routes on;
//   - targets (a multi-target verb): each target's params, the same way.
func paramsToLetters(params map[string]any, words [26]string) bool {
	changed := false
	set := func(key, next string) {
		if cur, _ := params[key].(string); cur != next {
			params[key] = next
			changed = true
		}
	}
	if s, ok := params["codeword"].(string); ok {
		set("codeword", spokenCodewordToToken(s, words))
	}
	for _, key := range []string{"prefix", "word", "word2"} {
		if s, ok := params[key].(string); ok {
			set(key, spokenWordToLetter(s, words))
		}
	}
	pl, okP := params["prefix_letter"].(string)
	sl, okS := params["suffix_letter"].(string)
	if okP && okS {
		set("codeword", pl+" "+sl)
	}
	if targets, ok := params["targets"].([]any); ok {
		for _, t := range targets {
			if tp, ok := t.(map[string]any); ok && paramsToLetters(tp, words) {
				changed = true
			}
		}
	}
	return changed
}

// inLetters returns event with its hint params in letters under the current
// alphabet (paramsToLetters). An event without such params, or with params
// that are not an object, is returned as it came.
func (h *Host) inLetters(event ActionEvent) ActionEvent {
	if len(event.Params) == 0 {
		return event
	}
	dec := json.NewDecoder(bytes.NewReader(event.Params))
	dec.UseNumber() // re-encode numbers exactly as they came
	var params map[string]any
	if err := dec.Decode(&params); err != nil || params == nil {
		return event
	}
	h.codewordsMu.RLock()
	words := h.codewords
	h.codewordsMu.RUnlock()
	if !paramsToLetters(params, words) {
		return event
	}
	b, err := json.Marshal(params)
	if err != nil {
		return event
	}
	event.Params = b
	return event
}
