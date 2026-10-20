package main

import (
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
