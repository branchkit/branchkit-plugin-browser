package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderBrowserSettingsHTML(t *testing.T, d BrowserSettingsData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := BrowserSettings(d).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The hint chooser names each mode, marks the chosen one for a screen
// reader, and draws its word samples from the user's own alphabet.
func TestBrowserSettings_HintChoicesAreNamedAndPressed(t *testing.T) {
	html := renderBrowserSettingsHTML(t, BrowserSettingsData{IsWord: true, WordA: "aim", WordB: "bus"})
	for _, want := range []string{"Letters", "Words", "Word, then letter", `aria-label="Hint badges"`, "aim bus", "aim b"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in %s", want, html)
		}
	}
	if strings.Count(html, `aria-pressed="true"`) != 1 {
		t.Errorf("exactly one choice should be pressed; got %s", html)
	}
}

// Before the alphabet loads there are no codewords to show, so the word
// samples are left out instead of naming words the user does not say.
func TestBrowserSettings_NoWordSamplesWithoutAlphabet(t *testing.T) {
	html := renderBrowserSettingsHTML(t, BrowserSettingsData{IsLetter: true})
	if strings.Contains(html, "arch") || strings.Contains(html, `class="bh-sample"> b<`) {
		t.Errorf("no codeword samples expected before the alphabet loads; got %s", html)
	}
	if !strings.Contains(html, "AB") {
		t.Error("the letter sample needs no alphabet and should always show")
	}
}
