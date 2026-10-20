package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// seedHintContributions stores a contribution set shaped like the extension's
// real payload: the two tab-targeted hint verbs (blank clears, stash retains)
// plus an ordinary literal command, so tests exercise both builder paths.
// Callers must restore with storeContributedCommands("", nil).
func seedHintContributions(h *Host) {
	h.storeContributedCommands("", []contributedCommand{
		{Action: "activate_hint_newtab", Pattern: "blank {hint}", Category: "Hints",
			Description: "Open the hinted link in a new focused tab."},
		{Action: "activate_hint_background", Pattern: "stash {hint+}", Category: "Hints",
			Description: "Open the hinted links in new background tabs.", RetainsHints: true},
		{Action: "scroll_down", Pattern: "scroll down", Category: "Scroll",
			Description: "Scroll down."},
	})
}

func TestContributedHintCommandExpansion(t *testing.T) {
	h := newTestHost()
	seedHintContributions(h)
	defer h.storeContributedCommands("", nil)

	cmds := h.buildContributedCommands("browser")
	// First occurrence per action type: each hint verb now also emits a
	// caret twin (caretHintTwin) right after its parent — the parent's
	// assertions below key off the first.
	byType := map[string]int{}
	for i, cmd := range cmds {
		if typ, ok := actionMap(cmd)["type"].(string); ok {
			if _, seen := byType[typ]; !seen {
				byType[typ] = i
			}
		}
	}

	i, ok := byType["browser.activate_hint_newtab"]
	if !ok {
		t.Fatal("blank {hint} contribution did not build a browser.activate_hint_newtab command")
	}
	blank := cmds[i]
	if got := patternSlot(blank, 0); got != "blank" {
		t.Errorf("slot 0 = %q, want the literal verb \"blank\"", got)
	}
	if got := patternSlot(blank, 1); got != "<prefix:plugin.browser.alpha>" {
		t.Errorf("slot 1 = %q, want the prefix capture", got)
	}
	if got := patternSlot(blank, 2); got != "<suffix:plugin.browser.hints_${prefix.codeword}_strict>" {
		t.Errorf("slot 2 = %q, want the strict dependent suffix capture", got)
	}
	// Gating mirrors the skeleton activate: hints tag required (NOT the
	// app-active tag the plain contributed commands get), completing tag on
	// partial, hints tag cleared (blank ends the hint interaction).
	if len(blank.RequiresTags) != 1 || blank.RequiresTags[0] != "plugin.browser.hints" {
		t.Errorf("RequiresTags = %v, want exactly [plugin.browser.hints]", blank.RequiresTags)
	}
	// SetsOnPartial left off — permitted now on the dependent suffix, but
	// capture.progress already drives the mid-pair narrowing.
	if len(blank.SetsOnPartial) != 0 {
		t.Errorf("SetsOnPartial = %v, want none (left off; capture.progress narrows)", blank.SetsOnPartial)
	}
	if got := blank.DisplaySources["prefix"]; got != "plugin.browser.hints_prefix" {
		t.Errorf("blank display_sources[prefix] = %q, want plugin.browser.hints_prefix", got)
	}
	if got := blank.DisplaySources["suffix"]; got != "plugin.browser.hints_${prefix.codeword}_strict" {
		t.Errorf("blank display_sources[suffix] = %q", got)
	}
	if len(blank.ClearsTags) != 1 || blank.ClearsTags[0] != "plugin.browser.hints" {
		t.Errorf("ClearsTags = %v, want [plugin.browser.hints]", blank.ClearsTags)
	}
	// Sealed identity params, same shape the skeleton activate delivers —
	// the extension resolves letter-pair → element at dispatch.
	a := actionParams(blank)
	for field, want := range map[string]string{
		"prefix_letter": "{prefix.letter}", "suffix_letter": "{suffix.letter}",
		"prefix_word": "{prefix.codeword}", "suffix_word": "{suffix.codeword}",
	} {
		if a[field] != want {
			t.Errorf("action param %s = %v, want %s", field, a[field], want)
		}
	}

	j, ok := byType["browser.activate_hint_background"]
	if !ok {
		t.Fatal("stash {hint+} contribution did not build a browser.activate_hint_background command")
	}
	stash := cmds[j]
	if len(stash.ClearsTags) != 0 {
		t.Errorf("stash ClearsTags = %v, want none (retains_hints: the gather continues)", stash.ClearsTags)
	}
	if len(stash.RequiresTags) != 1 || stash.RequiresTags[0] != "plugin.browser.hints" {
		t.Errorf("stash RequiresTags = %v, want exactly [plugin.browser.hints]", stash.RequiresTags)
	}
	// {hint+} expands to the manifest hint_pair macro reference, repeated,
	// with the ordered target list delivered under `targets`.
	if got := patternSlot(stash, 0); got != "stash" {
		t.Errorf("stash slot 0 = %q, want the literal verb", got)
	}
	if got := patternSlot(stash, 1); got != "<targets:hint_pair>+" {
		t.Errorf("stash slot 1 = %q, want the repeated macro reference", got)
	}
	sa := actionParams(stash)
	if sa["targets"] != "{targets}" {
		t.Errorf("stash action targets param = %v, want {targets}", sa["targets"])
	}
	if len(stash.SetsOnPartial) != 0 {
		t.Errorf("stash SetsOnPartial = %v, want none on the sealed shape", stash.SetsOnPartial)
	}

	// The ordinary contribution still takes the plain path with the
	// app-active gate.
	k, ok := byType["browser.scroll_down"]
	if !ok {
		t.Fatal("plain contribution missing from the built set")
	}
	if got := cmds[k].RequiresTags; len(got) != 1 || got[0] != "plugin.browser.active" {
		t.Errorf("plain contribution RequiresTags = %v, want [plugin.browser.active]", got)
	}

	// Every hint verb emits a caret twin directly after its parent: same
	// spec, [hints, caret] conjunction gate — hints stay usable inside
	// caret/visual selection, and the caret requirement makes the twin
	// modal-eligible so the codeword deck enters the selection-mode grammar.
	twin := cmds[i+1]
	if got := actionMap(twin)["type"]; got != "browser.activate_hint_newtab" {
		t.Fatalf("expected blank's caret twin at i+1, got action %v", got)
	}
	wantTags := []string{"plugin.browser.hints", "plugin.browser.caret"}
	if len(twin.RequiresTags) != 2 || twin.RequiresTags[0] != wantTags[0] || twin.RequiresTags[1] != wantTags[1] {
		t.Errorf("twin RequiresTags = %v, want %v", twin.RequiresTags, wantTags)
	}
	if got := patternSlot(twin, 0); got != "blank" {
		t.Errorf("twin slot 0 = %q — the twin must share its parent's pattern", got)
	}
	// A plain (non-hint) contribution gets NO twin.
	if k+1 < len(cmds) {
		next := cmds[k+1]
		if len(next.RequiresTags) == 2 && next.RequiresTags[1] == "plugin.browser.caret" &&
			actionMap(next)["type"] == "browser.scroll_down" {
			t.Error("plain contributions must not grow caret twins")
		}
	}
}

// TestHintExpansionMatchesManifestMacro pins the {hint} bespoke expansion
// (kept for bridge semantics — a macro instance can't bridge-resume across a
// chop yet) to plugin.json's hint_pair capture macro, so the two encodings of
// the pair shape can't drift silently. If this fails, one side changed the
// pair's collections or delivery fields without the other.
func TestHintExpansionMatchesManifestMacro(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		CaptureMacros map[string]struct {
			Slots  []string          `json:"slots"`
			Fields map[string]string `json:"fields"`
		} `json:"capture_macros"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse plugin.json: %v", err)
	}
	m, ok := manifest.CaptureMacros[hintPairMacro]
	if !ok {
		t.Fatalf("plugin.json declares no %q capture macro — {hint+} contributions reference it", hintPairMacro)
	}

	wantSlots := []string{
		"<prefix:plugin.browser.alpha>",
		"<suffix:plugin.browser.hints_${prefix.codeword}_strict>",
	}
	if len(m.Slots) != len(wantSlots) || m.Slots[0] != wantSlots[0] || m.Slots[1] != wantSlots[1] {
		t.Errorf("macro slots = %v, want %v (the {hint} expansion's pair shape)", m.Slots, wantSlots)
	}

	// The macro's per-instance fields must be exactly the identity fields the
	// bespoke expansion stamps into single-target actions (params only —
	// the action id rides the envelope, not the field set).
	want := sealedHintFields()
	if len(m.Fields) != len(want) {
		t.Errorf("macro fields = %v, want the hintSuffixFields identity set %v", m.Fields, want)
	}
	for k, v := range want {
		if m.Fields[k] != v {
			t.Errorf("macro field %s = %q, want %q", k, m.Fields[k], v)
		}
	}
}

func TestContributedHintCommandRejectsMalformedPatterns(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("", nil)
	h.storeContributedCommands("", []contributedCommand{
		{Action: "mid_hint", Pattern: "{hint} extra", Category: "Hints", Description: "x"},
		{Action: "capture_before_hint", Pattern: "verb {number} {hint}", Category: "Hints", Description: "x"},
	})
	if cmds := h.buildContributedCommands("browser"); len(cmds) != 0 {
		t.Errorf("malformed {hint} patterns built %d commands, want 0 (drop, don't half-build)", len(cmds))
	}
}

// TestImplicitMultiPairStashSkeleton pins the verbless min-two command
// (implicit batching): two macro
// references (ONE + ONE-OR-MORE), the stash action with split first/targets
// bindings, hints-tag gate, retains hints (no ClearsTags — the gather
// continues), completing tag on partial.
func TestImplicitMultiPairStashSkeleton(t *testing.T) {
	cmds := buildAllHintCommands("browser", testCodewords, "")
	idx := -1
	for i, cmd := range cmds {
		if patternSlot(cmd, 0) == "<first:hint_pair>" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatal("skeleton is missing the verbless min-two hint_pair command")
	}
	implicit := cmds[idx]
	if got := patternSlot(implicit, 1); got != "<targets:hint_pair>+" {
		t.Errorf("slot 1 = %q, want the repeated macro reference", got)
	}
	a := actionMap(implicit)
	if a["type"] != "browser.activate_hint_background" {
		t.Errorf("action type = %v, want browser.activate_hint_background (reuse: extension fan-out unchanged)", a["type"])
	}
	p := actionParams(implicit)
	if p["first"] != "{first}" || p["targets"] != "{targets}" {
		t.Errorf("action bindings = first:%v targets:%v, want the split {first}/{targets} templates", a["first"], a["targets"])
	}
	if len(implicit.RequiresTags) != 1 || implicit.RequiresTags[0] != "plugin.browser.hints" {
		t.Errorf("RequiresTags = %v, want exactly [plugin.browser.hints]", implicit.RequiresTags)
	}
	if len(implicit.ClearsTags) != 0 {
		t.Errorf("ClearsTags = %v, want none (implicit stash retains hints)", implicit.ClearsTags)
	}
	if len(implicit.SetsOnPartial) != 0 {
		t.Errorf("SetsOnPartial = %v, want none on the sealed shape", implicit.SetsOnPartial)
	}
}

// TestMergeImplicitTargets pins the first+targets fold the SSE bridge applies
// before forwarding, so the extension always sees ONE ordered targets list.
func TestMergeImplicitTargets(t *testing.T) {
	raw := map[string]any{
		"type":    "browser.activate_hint_background",
		"first":   map[string]any{"codeword": "day jar"},
		"targets": []any{map[string]any{"codeword": "flag jar"}},
	}
	mergeImplicitTargets(raw)
	if _, ok := raw["first"]; ok {
		t.Error("first must be folded away")
	}
	targets, _ := raw["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want first prepended (len 2)", targets)
	}
	if cw := targets[0].(map[string]any)["codeword"]; cw != "day jar" {
		t.Errorf("targets[0] = %v, want the first pair in spoken order", cw)
	}

	// No-op for ordinary actions.
	plain := map[string]any{"type": "browser.activate", "codeword": "day jar"}
	mergeImplicitTargets(plain)
	if len(plain) != 2 {
		t.Errorf("plain action mutated: %v", plain)
	}
}

// TestContributedHintTagEffects pins the gate-mirror lifecycle for contributed
// hint verbs: a clearing verb behaves exactly like the skeleton activate
// (clear + re-arm only in always mode); a retains_hints verb touches nothing;
// non-hint and unknown subActions report ok=false so handleOnAction falls
// through to hintTagSideEffects.
func TestContributedHintTagEffects(t *testing.T) {
	h := newTestHost()
	seedHintContributions(h)
	defer h.storeContributedCommands("", nil)

	clearActive, rePut, ok := h.contributedHintTagEffects("activate_hint_newtab", "always")
	if !ok || !clearActive || !rePut {
		t.Errorf("blank in always mode = (clear=%v rePut=%v ok=%v), want (true true true)", clearActive, rePut, ok)
	}
	clearActive, rePut, ok = h.contributedHintTagEffects("activate_hint_newtab", "manual")
	if !ok || !clearActive || rePut {
		t.Errorf("blank in manual mode = (clear=%v rePut=%v ok=%v), want (true false true)", clearActive, rePut, ok)
	}
	clearActive, rePut, ok = h.contributedHintTagEffects("activate_hint_background", "always")
	if !ok || clearActive || rePut {
		t.Errorf("stash = (clear=%v rePut=%v ok=%v), want (false false true) — hints stay live", clearActive, rePut, ok)
	}
	if _, _, ok := h.contributedHintTagEffects("scroll_down", "always"); ok {
		t.Error("non-hint contributed action reported ok=true; must fall through to hintTagSideEffects")
	}
	if _, _, ok := h.contributedHintTagEffects("activate", "always"); ok {
		t.Error("skeleton activate reported ok=true; its lifecycle is hintTagSideEffects' job")
	}
}

func TestContributedFindContextGating(t *testing.T) {
	h := newTestHost()
	h.storeContributedCommands("", []contributedCommand{
		{Action: "find_next", Pattern: "next", Category: "Find",
			Description: "Jump to the next find match.", Context: "find"},
	})
	defer h.storeContributedCommands("", nil)

	cmds := h.buildContributedCommands("browser")
	var next *branchkit.CommandSpec
	for i := range cmds {
		if typ, _ := actionMap(cmds[i])["type"].(string); typ == "browser.find_next" {
			next = &cmds[i]
			break
		}
	}
	if next == nil {
		t.Fatal("find_next contribution did not build")
	}
	// app-active AND find: the find tag survives blur (find.go), so app-active
	// carries the focus half of eligibility.
	want := []string{"plugin.browser.active", "plugin.browser.find"}
	if len(next.RequiresTags) != 2 || next.RequiresTags[0] != want[0] || next.RequiresTags[1] != want[1] {
		t.Errorf("RequiresTags = %v, want %v", next.RequiresTags, want)
	}
	if len(next.ClearsTags) != 0 {
		t.Errorf("find-jump must not clear tags at match (sustained session); got %v", next.ClearsTags)
	}
}

// TestEveryContributedHintClearingCommandIsGateMirrored is the contributed-set
// twin of TestEveryHintsTagClearingCommandIsGateMirrored (grammar_test.go):
// every contributed command that clears plugin.browser.hints must be mirrored
// by contributedHintTagEffects, or the gate cache drifts (stuck GrammarOwned,
// codewords paint but won't activate until a focus reset).
func TestEveryContributedHintClearingCommandIsGateMirrored(t *testing.T) {
	h := newTestHost()
	seedHintContributions(h)
	defer h.storeContributedCommands("", nil)

	const hintsTag = "plugin.browser.hints"
	clearing := 0
	for _, cmd := range h.buildContributedCommands("browser") {
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
		if clearActive, _, ok := h.contributedHintTagEffects(subAction, "always"); !ok || !clearActive {
			t.Errorf("contributed command %q clears %s but contributedHintTagEffects does not mirror it", typ, hintsTag)
		}
	}
	if clearing == 0 {
		t.Fatal("expected at least one contributed hints-tag-clearing command (blank)")
	}
}

func TestExtensionPatternToPlatform(t *testing.T) {
	cases := []struct{ in, want string }{
		{"toggle", "toggle"},
		{"scroll down {number}", "scroll down <number>"},
		{"find {text}", "find <text>"},
		{"page {number} now", "page <number> now"},
		// {hint}/{hint+} are left as-is (not V1 override scope) — their platform
		// display form is a dependent-capture expansion, so they won't key-match.
		{"blank {hint}", "blank {hint}"},
		{"stash {hint+}", "stash {hint+}"},
		{"  scroll   up  ", "scroll up"}, // whitespace normalized via Fields
	}
	for _, c := range cases {
		if got := extensionPatternToPlatform(c.in); got != c.want {
			t.Errorf("extensionPatternToPlatform(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPlatformPatternToExtension(t *testing.T) {
	// Round-trips with extensionPatternToPlatform for the V1 capture shapes.
	cases := []struct{ in, want string }{
		{"toggle", "toggle"},
		{"scroll down <number>", "scroll down {number}"},
		{"find <text>", "find {text}"},
		{"page <number> now", "page {number} now"},
	}
	for _, c := range cases {
		if got := platformPatternToExtension(c.in); got != c.want {
			t.Errorf("platformPatternToExtension(%q) = %q, want %q", c.in, got, c.want)
		}
		// Inverse of the forward translator on the platform-notation side.
		if rt := extensionPatternToPlatform(platformPatternToExtension(c.in)); rt != c.in {
			t.Errorf("round-trip failed: %q -> %q", c.in, rt)
		}
	}
}

func TestPhraseEditViews(t *testing.T) {
	all := []branchkit.CommandOverride{
		{Action: "browser.scroll", DefaultPattern: "scroll down <number>", NewPattern: "zoom <number>"},
		{Action: "browser.toggle_hints", DefaultPattern: "toggle", NewPattern: "hints"},
		{Action: "tiling.snap", DefaultPattern: "snap", NewPattern: "dock"}, // other plugin — filtered out
	}
	views := phraseEditViews(all, "browser.")
	if len(views) != 2 {
		t.Fatalf("expected 2 browser views, got %d: %+v", len(views), views)
	}
	// Prefix stripped + <..> translated back to {..}.
	if views[0].Action != "scroll" || views[0].DefaultPattern != "scroll down {number}" || views[0].NewPattern != "zoom {number}" {
		t.Errorf("view0 wrong: %+v", views[0])
	}
	if views[1].Action != "toggle_hints" {
		t.Errorf("view1 action wrong: %+v", views[1])
	}
}

// A second browser connecting with a partially-assembled catalog used to
// DELETE the first browser's commands: `contributed` was one slice replaced
// wholesale, so last writer won across every connection. The loss was silent —
// the command stopped being speakable and its words left the engine grammar,
// so a mishear took its place (2026-07-25: "highlight" vanished mid-session and
// Sherpa decoded it as "HI RIGHT"). Firefox and Chrome side by side is enough.
func TestContributionsAreScopedPerConnection(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	defer h.storeContributedCommands("firefox", nil)

	h.storeContributedCommands("chrome", []contributedCommand{
		{Action: "select_to", Pattern: "highlight", Category: "Selection"},
		{Action: "scroll_down", Pattern: "scroll down", Category: "Scroll"},
	})
	// A second browser mid-boot, catalog not yet assembled.
	h.storeContributedCommands("firefox", []contributedCommand{
		{Action: "scroll_down", Pattern: "scroll down", Category: "Scroll"},
	})

	union := h.contributedUnion()
	if len(union) != 2 {
		t.Fatalf("union = %d commands, want 2 (a partial contribution deleted one)", len(union))
	}
	var sawSelectTo bool
	for _, c := range union {
		if c.Action == "select_to" {
			sawSelectTo = true
		}
	}
	if !sawSelectTo {
		t.Fatal("select_to was deleted by another connection's partial contribution")
	}
}

func TestUnionIsStableAcrossPushes(t *testing.T) {
	h := newTestHost()
	// Map iteration order is random; an unstable union would churn the pushed
	// command set and, with it, the engine grammar.
	defer h.storeContributedCommands("a", nil)
	defer h.storeContributedCommands("b", nil)
	h.storeContributedCommands("a", []contributedCommand{{Action: "one", Pattern: "one"}})
	h.storeContributedCommands("b", []contributedCommand{{Action: "two", Pattern: "two"}})

	first := h.contributedUnion()
	for i := 0; i < 20; i++ {
		next := h.contributedUnion()
		if len(next) != len(first) {
			t.Fatalf("union length changed between pushes: %d vs %d", len(next), len(first))
		}
		for j := range next {
			if next[j].Action != first[j].Action {
				t.Fatalf("union order changed at %d: %q vs %q", j, next[j].Action, first[j].Action)
			}
		}
	}
}

func TestDisconnectDropsOnlyThatConnection(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	h.storeContributedCommands("chrome", []contributedCommand{{Action: "select_to", Pattern: "highlight"}})
	h.storeContributedCommands("firefox", []contributedCommand{{Action: "scroll_down", Pattern: "scroll down"}})

	if !h.dropContributedCommands("firefox") {
		t.Fatal("dropContributedCommands reported nothing to drop")
	}
	union := h.contributedUnion()
	if len(union) != 1 || union[0].Action != "select_to" {
		t.Fatalf("after firefox disconnected, union = %+v, want just select_to", union)
	}
	if h.dropContributedCommands("firefox") {
		t.Fatal("dropping an already-dropped connection reported a change")
	}
}

// One action legitimately carries several spoken forms — select_to is both
// "highlight" and "select to". A union keyed on the action id collapsed them,
// which showed up live as 118 contributed commands becoming 76.
func TestUnionKeepsEveryPatternForAnAction(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	h.storeContributedCommands("chrome", []contributedCommand{
		{Action: "select_to", Pattern: "highlight", Category: "Selection"},
		{Action: "select_to", Pattern: "select to", Category: "Selection", Context: "caret"},
	})
	if got := len(h.contributedUnion()); got != 2 {
		t.Fatalf("union = %d, want 2 — both spoken forms of select_to", got)
	}
}

// Two browsers running the same build contribute identical catalogs; the union
// must not double them.
func TestUnionDeDupesIdenticalContributions(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	defer h.storeContributedCommands("firefox", nil)
	same := []contributedCommand{
		{Action: "select_to", Pattern: "highlight"},
		{Action: "select_to", Pattern: "select to", Context: "caret"},
	}
	h.storeContributedCommands("chrome", same)
	h.storeContributedCommands("firefox", same)
	if got := len(h.contributedUnion()); got != 2 {
		t.Fatalf("union = %d, want 2 — identical catalogs must not double", got)
	}
}

// Two LIVE connections describing the same phrase differently — a browser that
// hasn't reloaded next to one that has. Both are legitimately connected, so
// pruning can't help: only one shape can win, and it must be the newer one.
//
// Field failure this pins (2026-07-26): retiring select_to's dictated_param
// left the old shape contributed by a stale browser, both survived the
// whole-command de-dup, and speaking "highlight" kept opening the retired cue
// card. The hazard grows teeth once a retired path is DELETED rather than
// merely unused — a stale connection can then name code that doesn't exist.
func TestUnionPrefersTheNewestShapeForAConflictingPhrase(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("old", nil)
	defer h.storeContributedCommands("new", nil)

	retired := []contributedCommand{
		{Action: "select_to", Pattern: "highlight", Description: "the previous build's behaviour"},
	}
	h.storeContributedCommands("old", retired)
	h.storeContributedCommands("new", []contributedCommand{
		{Action: "select_to", Pattern: "highlight", Description: "the current behaviour"},
	})

	union, conflicts := h.contributedUnionWithConflicts()
	if len(union) != 1 {
		t.Fatalf("union = %d entries, want 1 — one phrase cannot have two answers", len(union))
	}
	if union[0].Description != "the current behaviour" {
		t.Errorf("union kept %q; want the newer contribution's shape", union[0].Description)
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want 1 — a disagreement must be reported, not absorbed", conflicts)
	}

	// Order of arrival decides, not connection name: re-contributing the OLD
	// shape last makes it current again (a browser reverted to a prior build).
	h.storeContributedCommands("old", retired)
	union, _ = h.contributedUnionWithConflicts()
	if len(union) != 1 || union[0].Description != "the previous build's behaviour" {
		t.Errorf("union = %+v, want the most recently contributed shape", union)
	}
}

// Same phrase, different CONTEXT gates, is not a conflict: the context is part
// of what the matcher keys on, so "select to" in caret mode and a hypothetical
// ungated twin are separate spoken identities.
func TestUnionKeepsSamePhraseUnderDifferentContexts(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	h.storeContributedCommands("chrome", []contributedCommand{
		{Action: "select_to", Pattern: "select to"},
		{Action: "select_to", Pattern: "select to", Context: "caret"},
	})
	union, conflicts := h.contributedUnionWithConflicts()
	if len(union) != 2 {
		t.Fatalf("union = %d, want 2 — context is part of the spoken identity", len(union))
	}
	if conflicts != 0 {
		t.Errorf("conflicts = %d, want 0", conflicts)
	}
}

// Identical catalogs from two browsers are not a conflict — the common case.
func TestIdenticalCatalogsReportNoConflict(t *testing.T) {
	h := newTestHost()
	defer h.storeContributedCommands("chrome", nil)
	defer h.storeContributedCommands("firefox", nil)
	same := []contributedCommand{{Action: "select_to", Pattern: "highlight"}}
	h.storeContributedCommands("chrome", same)
	h.storeContributedCommands("firefox", same)
	if _, conflicts := h.contributedUnionWithConflicts(); conflicts != 0 {
		t.Errorf("conflicts = %d, want 0 — agreeing browsers are not a conflict", conflicts)
	}
}

// A reloaded extension reconnects under a NEW conn id, and until the old
// connection's SSE closes both catalogs are registered — so the union keeps the
// stale one and a command change appears not to take effect. The disconnect
// drain is the primary path; pruning on write is what converges when it's slow
// or missed. Observed live 2026-07-26 as union=119 against a 118-command
// catalog, the extra entry pinning a superseded pattern.
func TestContributeDropsCatalogsOfDeadConnections(t *testing.T) {
	h := newTestHost()
	prev := h.state
	h.state = &PluginState{PluginID: "browser"}
	prevConns := h.state.Conns
	_ = prev
	h.state.Conns = conns(map[string]Conn{"live": {Client: "client-1"}})
	t.Cleanup(func() {
		h.state.Conns = prevConns
		h.storeContributedCommands("live", nil)
		h.storeContributedCommands("stale", nil)
	})

	h.storeContributedCommands("stale", []contributedCommand{
		{Action: "find_immediate", Pattern: "search"},
	})
	h.storeContributedCommands("live", []contributedCommand{
		{Action: "find_open", Pattern: "search"},
	})

	h.contribMu.Lock()
	pruned := h.pruneContributionsLocked("live")
	h.contribMu.Unlock()

	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (the disconnected conn)", pruned)
	}
	union := h.contributedUnion()
	if len(union) != 1 || union[0].Action != "find_open" {
		t.Fatalf("union = %+v, want only the live connection's catalog", union)
	}
}

// The unstamped bucket is pruned like any other. Exempting it made it
// immortal: an older build filled it, the same browser then reloaded and
// started stamping, and the stale catalog outlived it forever.
func TestPruneClearsTheUnstampedBucket(t *testing.T) {
	h := newTestHost()
	prev := h.state
	h.state = &PluginState{PluginID: "browser"}
	prevConns := h.state.Conns
	_ = prev
	h.state.Conns = conns(map[string]Conn{"live": {Client: "c"}})
	t.Cleanup(func() {
		h.state.Conns = prevConns
		h.storeContributedCommands("", nil)
	})
	h.storeContributedCommands("", []contributedCommand{{Action: "scroll_down", Pattern: "scroll down"}})

	// A stamped contribution arrives from the live connection: the unstamped
	// leftover is not the poster, and its "connection" is not in the client
	// set, so it goes.
	h.contribMu.Lock()
	pruned := h.pruneContributionsLocked("live")
	h.contribMu.Unlock()

	if pruned != 1 || len(h.contributedUnion()) != 0 {
		t.Fatalf("pruned=%d union=%d — an unstamped catalog from a dead conn must go",
			pruned, len(h.contributedUnion()))
	}
}
