package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// testCodewords provides a fixed codeword set for tests.
// Production code reads codewords from the alphabet store at runtime.
var testCodewords = [26]string{
	"arch", "bake", "check", "deck", "egg", "food", "glad", "half",
	"iron", "jake", "kind", "land", "make", "none", "own", "plan",
	"quick", "rain", "song", "take", "under", "voice", "work",
	"xray", "yoga", "zoo",
}

func TestMain(m *testing.M) {
	// Per-test state lives on each test's host (newTestHost); nothing is
	// package-wide any more.
	os.Exit(m.Run())
}

// --- codewordToLetter tests ---

func TestCodewordToLetterSingle(t *testing.T) {
	got := codewordToLetter("arch", testCodewords)
	if got != "a" {
		t.Errorf("codewordToLetter(\"arch\") = %q, want \"a\"", got)
	}
	got = codewordToLetter("check", testCodewords)
	if got != "c" {
		t.Errorf("codewordToLetter(\"check\") = %q, want \"c\"", got)
	}
	got = codewordToLetter("zoo", testCodewords)
	if got != "z" {
		t.Errorf("codewordToLetter(\"zoo\") = %q, want \"z\"", got)
	}
}

func TestCodewordToLetterPair(t *testing.T) {
	got := codewordToLetter("zoo arch", testCodewords)
	if got != "za" {
		t.Errorf("codewordToLetter(\"zoo arch\") = %q, want \"za\"", got)
	}
	got = codewordToLetter("yoga egg", testCodewords)
	if got != "ye" {
		t.Errorf("codewordToLetter(\"yoga egg\") = %q, want \"ye\"", got)
	}
}

func TestCodewordToLetterUnknownWord(t *testing.T) {
	got := codewordToLetter("nonsense", testCodewords)
	if got != "" {
		t.Errorf("codewordToLetter(unknown word) = %q, want empty (skip signal)", got)
	}
	got = codewordToLetter("zoo unknown", testCodewords)
	if got != "" {
		t.Errorf("codewordToLetter(pair with unknown) = %q, want empty", got)
	}
}

func TestCodewordToLetterEmpty(t *testing.T) {
	got := codewordToLetter("", testCodewords)
	if got != "" {
		t.Errorf("codewordToLetter(\"\") = %q, want empty", got)
	}
	got = codewordToLetter("   ", testCodewords)
	if got != "" {
		t.Errorf("codewordToLetter(whitespace) = %q, want empty", got)
	}
}

func TestCodewordToLetterReflectsAlphabetOverride(t *testing.T) {
	custom := testCodewords
	custom[1] = "bear"
	got := codewordToLetter("bear", custom)
	if got != "b" {
		t.Errorf("codewordToLetter with override = %q, want \"b\"", got)
	}
	got = codewordToLetter("bake", custom)
	if got != "" {
		t.Errorf("codewordToLetter(\"bake\") with override = %q, want empty", got)
	}
}

// --- command builder action format tests ---

// actionMap decodes a built command's opaque action into a map (the
// ENVELOPE: type / params / phase).
func actionMap(cmd branchkit.CommandSpec) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(cmd.Action, &m); err != nil {
		return nil
	}
	return m
}

// actionParams is the action's nested params object (one params dialect —
// the payload never rides flat on the envelope).
func actionParams(cmd branchkit.CommandSpec) map[string]interface{} {
	p, _ := actionMap(cmd)["params"].(map[string]interface{})
	return p
}

// patternSlot decodes the i-th pattern slot as a literal string (Word /
// Capture / Text). Returns "" for an out-of-range index or an OneOf slot.
func patternSlot(cmd branchkit.CommandSpec, i int) string {
	if i < 0 || i >= len(cmd.Pattern) {
		return ""
	}
	var s string
	if err := json.Unmarshal(cmd.Pattern[i], &s); err != nil {
		return ""
	}
	return s
}

func TestCommandActionPluginType(t *testing.T) {
	cmd := branchkit.Command(branchkit.Word("show")).Action("browser.show_hints").Build()
	a := actionMap(cmd)
	if a["type"] != "browser.show_hints" {
		t.Errorf("type = %v, want browser.show_hints", a["type"])
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	json.Unmarshal(b, &parsed)
	action := parsed["action"].(map[string]interface{})
	if action["type"] != "browser.show_hints" {
		t.Errorf("serialized type = %v", action["type"])
	}
}

func TestCommandActionTemplateWithDotPath(t *testing.T) {
	cmd := branchkit.Command(branchkit.Capture("hint", "plugin.browser.hints_arch")).
		Action("browser.activate", map[string]any{
			"letter":    "{hint.letter}",
			"id":        "{hint.id}",
			"frame_id":  "{hint.frame_id}",
			"codeword":  "{hint.codeword}",
			"elem_type": "{hint.type}",
		}).Build()
	a := actionMap(cmd)
	if a["type"] != "browser.activate" {
		t.Errorf("type = %v, want browser.activate", a["type"])
	}
	ap := actionParams(cmd)
	if ap["letter"] != "{hint.letter}" {
		t.Errorf("letter = %v, want {hint.letter}", a["letter"])
	}
	if ap["id"] != "{hint.id}" {
		t.Errorf("id = %v, want {hint.id}", ap["id"])
	}
	if ap["elem_type"] != "{hint.type}" {
		t.Errorf("elem_type = %v, want {hint.type}", ap["elem_type"])
	}
	if got := patternSlot(cmd, 0); got != "<hint:plugin.browser.hints_arch>" {
		t.Errorf("pattern slot 0 = %q, want <hint:plugin.browser.hints_arch>", got)
	}
}

func TestStructuredListValue(t *testing.T) {
	entry := map[string]interface{}{
		"arch": map[string]string{"letter": "a", "id": "1", "frame_id": "0", "type": "button"},
		"bake": map[string]string{"letter": "b", "id": "2", "frame_id": "0", "type": "link"},
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	json.Unmarshal(b, &parsed)
	arch := parsed["arch"].(map[string]interface{})
	if arch["letter"] != "a" {
		t.Errorf("arch.letter = %v, want a", arch["letter"])
	}
	if arch["type"] != "button" {
		t.Errorf("arch.type = %v, want button", arch["type"])
	}
}

// --- initCodewords / codeword override tests ---

func TestCodewordsPopulatedByTestMain(t *testing.T) {
	h := newTestHost()
	h.codewordsMu.RLock()
	got := h.codewords
	h.codewordsMu.RUnlock()
	if got[0] != "arch" {
		t.Errorf("codewords[0] = %q, want arch", got[0])
	}
	if got[25] != "zoo" {
		t.Errorf("codewords[25] = %q, want zoo", got[25])
	}
}

// --- buildAllHintCommands tests ---

func TestBuildAllHintCommandsCount(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "plugin.browser.active")
	// 1 toggle + activate + its caret twin + implicit multi-pair stash + its
	// caret twin = 5. (Hover moved to the extension's {hint} contribution,
	// 3b; "over" became the app-gated spoken Esc — buildEscapeCommand — no
	// longer a hint command; the spoken muting cancel ("dismiss"/"hide") was
	// removed 2026-08-05 — muting follows badge visibility now;
	// the caret twins keep hints usable
	// inside caret/visual selection.)
	if len(cmds) != 5 {
		t.Errorf("buildAllHintCommands returned %d commands, want 5", len(cmds))
	}
}

func TestBuildAllHintCommandsTagStructure(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "plugin.browser.active")

	var toggleCmd, activateCmd *branchkit.CommandSpec
	for i := range cmds {
		p := cmds[i].Pattern
		if len(p) == 1 && patternSlot(cmds[i], 0) == "toggle" {
			toggleCmd = &cmds[i]
		}
		// Multiple 2-slot commands exist (activate + its caret twin + the
		// implicit multi-pair stash) — key on the prefix capture AND the
		// hints-only gate to pick the parent activate.
		if len(p) == 2 && patternSlot(cmds[i], 0) == "<prefix:plugin.browser.alpha>" &&
			len(cmds[i].RequiresTags) == 1 {
			activateCmd = &cmds[i]
		}
	}

	if toggleCmd == nil {
		t.Fatal("no toggle command found")
	}
	if len(toggleCmd.RequiresTags) != 1 || toggleCmd.RequiresTags[0] != "plugin.browser.active" {
		t.Errorf("toggle command requires_tags = %v, want [plugin.browser.active]", toggleCmd.RequiresTags)
	}
	// A toggle can't statically pick set-or-clear; the extension owns
	// visibility, so the command carries no tag ops.
	if len(toggleCmd.SetsTags) != 0 {
		t.Errorf("toggle command sets_tags = %v, want none", toggleCmd.SetsTags)
	}
	if len(toggleCmd.ClearsTags) != 0 {
		t.Errorf("toggle command clears_tags = %v, want none", toggleCmd.ClearsTags)
	}

	if activateCmd == nil {
		t.Fatal("no activate command found")
	}
	if len(activateCmd.RequiresTags) != 1 || activateCmd.RequiresTags[0] != "plugin.browser.hints" {
		t.Errorf("activate requires_tags = %v, want [plugin.browser.hints]", activateCmd.RequiresTags)
	}
	if len(activateCmd.ClearsTags) != 1 || activateCmd.ClearsTags[0] != "plugin.browser.hints" {
		t.Errorf("activate clears_tags = %v, want [plugin.browser.hints]", activateCmd.ClearsTags)
	}

	// No spoken muting cancel: the "dismiss"/"hide" command is gone
	// — muting rides badge visibility.
	for i := range cmds {
		if len(cmds[i].Pattern) == 1 &&
			(patternSlot(cmds[i], 0) == "hide" || patternSlot(cmds[i], 0) == "dismiss") {
			t.Errorf("unexpected muting-cancel command %q — removed by design", patternSlot(cmds[i], 0))
		}
	}
}

// Caret twins: activate and the implicit stash each emit a copy gated on
// [hints, caret] so the hint system stays usable inside caret/visual
// selection (chips pick, links follow — no mode exit) and the caret
// requirement pulls the codeword deck into the selection-mode grammar.
func TestBuildAllHintCommandsCaretTwins(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "plugin.browser.active")
	twins := 0
	for i := range cmds {
		tags := cmds[i].RequiresTags
		if len(tags) == 2 && tags[0] == "plugin.browser.hints" && tags[1] == "plugin.browser.caret" {
			twins++
		}
	}
	if twins != 2 {
		t.Errorf("expected 2 caret twins (activate + implicit stash), got %d", twins)
	}
}

// The platform owns "over" (it closes the most recent mode, else presses
// Escape, which the extension's cascade answers one layer at a time). The
// browser pushes its escape group EMPTY, so a running platform drops the
// command it used to register; a second "over" could tie with a mode's own.
func TestEscapeGroupPushesNoOver(t *testing.T) {
	h := newTestHost()
	var pushed []branchkit.CommandSpec
	orig := h.pushCommandGroupRPC
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		if group == groupEscape {
			pushed = cmds
		}
		return len(cmds), nil
	}
	t.Cleanup(func() { h.pushCommandGroupRPC = orig })

	if err := h.pushCommandGroupLocked("browser", groupEscape); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 0 {
		t.Fatalf("escape group pushed %d commands, want none", len(pushed))
	}
}

func TestBuildAllHintCommandsNoAppTag(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "")

	for i := range cmds {
		cmd := cmds[i]
		if len(cmd.Pattern) == 1 && patternSlot(cmd, 0) == "toggle" {
			if len(cmd.RequiresTags) != 0 {
				t.Errorf("toggle command with no appTag should have empty requires_tags, got %v", cmd.RequiresTags)
			}
			return
		}
	}
	t.Fatal("no toggle command found")
}

// --- activateTagSideEffects tests ---
//
// Regression coverage for the 2026-05-23 fix that wires the inline tag
// re-put after browser.activate. Easy to silently break: a refactor that
// changes the subAction string or the visibility-mode comparison would
// leave hold-to-talk multi-hint sessions broken (the tag stays cleared
// after the first activate, follow-up "arch X" falls through to the
// alphabet command). These tests lock the decision in place.

func TestActivateTagSideEffectsAlwaysMode(t *testing.T) {
	clearActive, rePut := hintTagSideEffects("activate", "always")
	if !clearActive {
		t.Error("expected clearActive=true on activate")
	}
	if !rePut {
		t.Error("expected rePut=true on activate in always-mode (inline restore so next utterance lands as a hint)")
	}
}

func TestActivateTagSideEffectsManualMode(t *testing.T) {
	clearActive, rePut := hintTagSideEffects("activate", "manual")
	if !clearActive {
		t.Error("expected clearActive=true on activate (manifest clearsTags still fires)")
	}
	if rePut {
		t.Error("expected rePut=false in manual-mode — hints are meant to hide after activation")
	}
}

func TestHintTagSideEffectsNonClearing(t *testing.T) {
	// Actions that do NOT clear plugin.browser.hints leave the gate untouched.
	for _, sub := range []string{"scroll", "find_open", "show_hints", "hover_hint"} {
		clearActive, rePut := hintTagSideEffects(sub, "always")
		if clearActive {
			t.Errorf("subAction=%q: expected clearActive=false (does not clear the hints tag)", sub)
		}
		if rePut {
			t.Errorf("subAction=%q: expected rePut=false", sub)
		}
	}
}

func TestHintTagSideEffectsNoopNotMirrored(t *testing.T) {
	// The spoken muting cancel ("dismiss"/"hide" → noop) was removed
	// 2026-08-05: no command carries a
	// noop-driven hints clear anymore, so the gate must NOT treat "noop" as a
	// tag-clearing action — muting rides badge visibility (the extension
	// retracts grammar on hide), not a word.
	clearActive, rePut := hintTagSideEffects("noop", "always")
	if clearActive {
		t.Error(`subAction="noop": expected clearActive=false — no command clears hints via noop anymore`)
	}
	if rePut {
		t.Error(`subAction="noop": expected rePut=false`)
	}
}

// TestEveryHintsTagClearingCommandIsGateMirrored pins the invariant that every
// built hint command which clears plugin.browser.hints is mirrored by the gate
// (hintTagSideEffects returns clearActive=true for its action). A clearsTags
// command the gate does NOT mirror drifts the cache: it stays GrammarOwned while
// the platform tag is cleared, so the next re-arm SET-skips and codewords paint
// but won't activate until a focus reset. Adding a new clearing command without
// wiring it into hintTagSideEffects re-introduces that class of bug — this test
// fails loudly if that happens.
func TestEveryHintsTagClearingCommandIsGateMirrored(t *testing.T) {
	const hintsTag = "plugin.browser.hints"
	clearing := 0
	for _, cmd := range buildAllHintCommands("browser", testCodewords, "") {
		clears := false
		for _, tg := range cmd.ClearsTags {
			if tg == hintsTag {
				clears = true
			}
		}
		if !clears {
			continue
		}
		clearing++
		typ, _ := actionMap(cmd)["type"].(string)
		subAction := typ
		if i := strings.LastIndex(typ, "."); i >= 0 {
			subAction = typ[i+1:]
		}
		if clearActive, _ := hintTagSideEffects(subAction, "always"); !clearActive {
			t.Errorf("command action %q clears %s but the gate does not mirror it (hintTagSideEffects clearActive=false) — gate cache will drift", typ, hintsTag)
		}
	}
	if clearing == 0 {
		t.Fatal("expected at least one hints-tag-clearing command (activate/hide/cancel)")
	}
}

// The hybrid activate spec: SEALED prefix (any first letter, always startable)
// + STRICT dependent suffix (`plugin.browser.hints_<prefix>_strict` — match == what the
// Discovery HUD shows). Letters + words ride the action params; the extension
// still does the live-geometry check at dispatch. Pins the default-path shape.
func TestBuildAllHintCommandsHybridActivate(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "")
	var activate *branchkit.CommandSpec
	for i := range cmds {
		if len(cmds[i].Pattern) == 2 && patternSlot(cmds[i], 0) == "<prefix:plugin.browser.alpha>" {
			activate = &cmds[i]
			break
		}
	}
	if activate == nil {
		t.Fatal("no sealed-alphabet activate command found")
	}
	if got := patternSlot(*activate, 1); got != "<suffix:plugin.browser.hints_${prefix.codeword}_strict>" {
		t.Errorf("hybrid activate Pattern[1] = %q, want the strict dependent suffix", got)
	}
	if len(activate.RequiresTags) != 1 || activate.RequiresTags[0] != "plugin.browser.hints" {
		t.Errorf("sealed activate requires_tags = %v, want [plugin.browser.hints]", activate.RequiresTags)
	}
	if len(activate.ClearsTags) != 1 || activate.ClearsTags[0] != "plugin.browser.hints" {
		t.Errorf("sealed activate clears_tags = %v, want [plugin.browser.hints]", activate.ClearsTags)
	}
	// SetsOnPartial left off — the dependent suffix would now permit it, but
	// capture.progress already drives the mid-pair narrowing.
	if len(activate.SetsOnPartial) != 0 {
		t.Errorf("hybrid activate sets_on_partial = %v, want none (left off)", activate.SetsOnPartial)
	}
	params := actionParams(*activate)
	if params == nil {
		t.Fatal("sealed activate action carries no nested params object")
	}
	if got := activate.DisplaySources["prefix"]; got != "plugin.browser.hints_prefix" {
		t.Errorf("sealed activate display_sources[prefix] = %q, want plugin.browser.hints_prefix", got)
	}
	if got := activate.DisplaySources["suffix"]; got != "plugin.browser.hints_${prefix.codeword}_strict" {
		t.Errorf("sealed activate display_sources[suffix] = %q", got)
	}
	for field, want := range map[string]string{
		"prefix_letter": "{prefix.letter}",
		"suffix_letter": "{suffix.letter}",
		"prefix_word":   "{prefix.codeword}",
		"suffix_word":   "{suffix.codeword}",
	} {
		if got, _ := params[field].(string); got != want {
			t.Errorf("sealed activate params[%q] = %q, want %q", field, got, want)
		}
	}
}
