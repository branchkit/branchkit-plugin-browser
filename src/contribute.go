package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// contributedCommand is one spoken form the browser EXTENSION owns and
// contributes over POST /commands/contribute. The extension is the single
// source of truth for its command vocabulary; this plugin is a thin registrar
// that folds the contributed set into pushAllCommands. `Action` is a bare id
// ("scroll", "find_next"); the plugin prefixes it and owns the context gate, so
// the extension never names a platform tag.
type contributedCommand = wire.ContributedCommand

// anyParams widens a contribution's string params to the command builder's
// map. A copy: the builder may add keys.
func anyParams(p map[string]string) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// Contributions are keyed PER CONNECTION, not global.
//
// One slice replaced wholesale meant last writer wins across every browser: a
// second browser connecting with a partially-assembled catalog deleted commands
// for the first, and the loss was silent — the command simply stopped being
// speakable, and its words left the engine grammar, so a mishear replaced it
// (2026-07-25: "highlight" vanished, Sherpa then decoded it as "HI RIGHT").
// Firefox and Chrome connected side by side is enough to trigger it; so is one
// browser reconnecting mid-boot. This was on record as an unexplained
// disappearance for weeks.
//
// Per-connection storage mirrors FrameSessions, and the union at build time
// means a partial contribution can only ever affect its own connection's
// entry. Same-id commands from different browsers are the same extension
// build, so a straight union with last-in-wins per action is correct; the
// ordering is made deterministic below so the pushed set doesn't churn.
// connCatalog is one connection's contribution plus the order it arrived in.
// The sequence breaks SHAPE conflicts: when two live connections disagree about
// what a spoken phrase does, the most recent contribution is the current truth
// (see contributedUnion).
type connCatalog struct {
	seq  uint64
	cmds []contributedCommand
}

var ()

// storeContributedCommands records one connection's catalog. The route
// refuses a body without conn_id, so every catalog has its own key.
func (h *Host) storeContributedCommands(connID string, cmds []contributedCommand) {
	h.contribMu.Lock()
	if len(cmds) == 0 {
		// An empty catalog is the absence of one — keeping the key would leave
		// empty buckets accumulating per reconnect for no benefit, since the
		// union treats them identically.
		delete(h.contributed, connID)
	} else {
		h.contribSeq++
		h.contributed[connID] = connCatalog{seq: h.contribSeq, cmds: cmds}
	}
	h.contribMu.Unlock()
}

// pruneContributionsLocked forgets catalogs whose connection is no longer in
// the SSE client set.
//
// The disconnect drain is the primary path, but it is not sufficient on its
// own: a reloaded extension reconnects under a NEW conn id, and until the old
// connection's SSE actually closes, both catalogs are registered and the union
// keeps BOTH. In development that reads as a command change simply not taking
// effect — the old mapping is still live, contributed by a connection nobody
// is using. Observed 2026-07-26: two live conns from one browser, union=119
// against a 118-command catalog, with the stale entry pinning the old pattern.
//
// Pruning on write is the cheap fix: it needs no new signal, runs only when a
// contribution arrives, and converges even if a disconnect is missed entirely.
//
// Two guards, both learned the hard way on the first cut:
//
//   - never prune `keep` (the connection currently posting). Its SSE may not be
//     registered yet — contribution and SSE registration are independent
//     arrivals — and pruning the poster is self-defeating.
//   - do nothing when the client set is EMPTY. That is not "everyone
//     disconnected", it is "we have no picture yet", which is exactly the state
//     right after a plugin restart. The first cut wiped every catalog there and
//     logged union=0.
//
// Caller holds mu.
func (h *Host) pruneContributionsLocked(keep string) int {
	if h.state.liveConnCount() == 0 {
		return 0
	}
	pruned := 0
	for conn := range h.contributed {
		if conn == keep {
			continue // the poster
		}
		// The unstamped bucket is pruned like any other. Exempting it made it
		// IMMORTAL: an older build filled it, then the same browser reloaded
		// and started stamping, and the stale catalog outlived it forever —
		// union stuck at 119 against a 118-command catalog with a superseded
		// pattern still winning. Nothing ships an unstamped build, so there is
		// no compatibility to protect, only a leak to close. An unstamped post
		// still works; it simply doesn't survive the next stamped one.
		if h.state.clientOf(conn) != "" {
			continue
		}
		delete(h.contributed, conn)
		pruned++
	}
	return pruned
}

// dropContributedCommands forgets a connection's catalog — called when the
// browser disconnects, so a dead browser's commands don't outlive it.
func (h *Host) dropContributedCommands(connID string) bool {
	h.contribMu.Lock()
	defer h.contribMu.Unlock()
	if _, ok := h.contributed[connID]; !ok {
		return false
	}
	delete(h.contributed, connID)
	return true
}

// spokenIdentity is what the MATCHER keys on: the phrase, and the context gate
// that decides when the phrase is eligible. Two contributions sharing one are
// two answers to the same question, and only one can win.
//
// Deliberately NOT the action: one action legitimately carries several spoken
// forms (select_to is both "highlight" and "select to"), and keying on the
// action alone silently collapsed 118 contributed commands to 76.
func spokenIdentity(c contributedCommand) string {
	return c.Context + "\x00" + c.Pattern
}

// contributedUnion flattens every connection's catalog, one entry per spoken
// identity, NEWEST CONTRIBUTION WINNING.
//
// The earlier cut de-duplicated by the whole marshaled command, which kept
// nothing distinct from being folded together — but two connections describing
// the same phrase DIFFERENTLY are distinct by that measure, so both survived and
// the matcher picked between them by ordering. Removing a field could therefore
// fail to take effect: retiring select_to's dictated_param descriptor left the
// old shape live, contributed by a browser still running the previous build, and
// speaking "highlight" kept opening the retired cue card (2026-07-26).
//
// That descriptor has since been deleted outright, which changes the failure
// mode rather than removing it. An unknown field now decodes away silently, so
// a stale browser's contribution arrives SHAPED like the current one and
// differs only where the two builds genuinely disagree — quieter, and harder to
// spot. Newest-wins is what keeps the live build's answer on top; without it the
// choice falls back to ordering, which is how this went wrong the first time.
//
// Recency is the right tiebreak because divergence means one connection has been
// reloaded and the other has not, and a reload is the statement that the new
// shape is current. A conflict is never silently absorbed: the count is
// returned so the contribute handler can log it.
//
// Emission order stays keyed to sorted connections, and an identity takes the
// position of its FIRST appearance, so the pushed set doesn't churn just because
// a browser re-contributed — an unstable order would rebuild the engine grammar.
func (h *Host) contributedUnion() []contributedCommand {
	union, _ := h.contributedUnionWithConflicts()
	return union
}

func (h *Host) contributedUnionWithConflicts() (union []contributedCommand, conflicts int) {
	h.contribMu.RLock()
	defer h.contribMu.RUnlock()
	conns := make([]string, 0, len(h.contributed))
	for c := range h.contributed {
		conns = append(conns, c)
	}
	sort.Strings(conns)

	// Pass 1: the winning SHAPE per spoken identity — highest sequence, with the
	// connection id breaking exact ties so the result never depends on map order.
	type winner struct {
		cmd  contributedCommand
		seq  uint64
		conn string
	}
	best := make(map[string]winner, 128)
	for _, c := range conns {
		cat := h.contributed[c]
		for _, cmd := range cat.cmds {
			id := spokenIdentity(cmd)
			cur, ok := best[id]
			if !ok {
				best[id] = winner{cmd: cmd, seq: cat.seq, conn: c}
				continue
			}
			if !sameCommand(cur.cmd, cmd) {
				conflicts++
			}
			if cat.seq > cur.seq || (cat.seq == cur.seq && c > cur.conn) {
				best[id] = winner{cmd: cmd, seq: cat.seq, conn: c}
			}
		}
	}

	// Pass 2: emit each identity once, at its first position in sorted-conn order.
	emitted := make(map[string]bool, len(best))
	union = make([]contributedCommand, 0, len(best))
	for _, c := range conns {
		for _, cmd := range h.contributed[c].cmds {
			id := spokenIdentity(cmd)
			if emitted[id] {
				continue
			}
			emitted[id] = true
			union = append(union, best[id].cmd)
		}
	}
	return union, conflicts
}

// contributedPhrase is the spoken form a connected extension contributed for
// `action` in the default context, capture-free, or `fallback` when none has
// — for a sentence that tells the person what to say. The extension owns its
// vocabulary; a phrase copied into this plugin would outlive a rename.
func (h *Host) contributedPhrase(action, fallback string) string {
	for _, c := range h.contributedUnion() {
		if c.Action == action && c.Context == "" && !strings.Contains(c.Pattern, "{") {
			return c.Pattern
		}
	}
	return fallback
}

// sameCommand reports whether two contributions describe the same behaviour.
// Marshal-compare so every field counts, including ones added later; an
// unmarshalable pair is treated as differing, which costs a log line rather
// than hiding a real divergence.
func sameCommand(a, b contributedCommand) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return string(ab) == string(bb)
}

// parsePatternSlots turns a contributed pattern string into SDK pattern slots:
// "{text}" -> free-text capture, "{name}" -> list capture <name>, else a
// literal word. Mirrors the word/capture/text forms the deleted Go builders
// used (e.g. "scroll down {number}" -> Word, Word, Capture("","number")).
func parsePatternSlots(pattern string) []branchkit.PatternSlot {
	fields := strings.Fields(pattern)
	slots := make([]branchkit.PatternSlot, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 && strings.HasPrefix(f, "{") && strings.HasSuffix(f, "}") {
			name := f[1 : len(f)-1]
			if name == "text" {
				slots = append(slots, branchkit.Text(""))
			} else {
				slots = append(slots, branchkit.Capture("", name))
			}
			continue
		}
		slots = append(slots, branchkit.Word(f))
	}
	return slots
}

// extensionPatternToPlatform rewrites an extension-authored phrase (with
// {name}/{text} capture braces) into the platform's canonical capture notation
// (<name>/<text>) — the same mapping parsePatternSlots applies when compiling a
// contributed pattern, so a translated default_pattern equals the platform's
// display form and the override keys correctly. Literal words pass through.
//
// {hint}/{hint+}/{palette} are deliberately NOT translated: their platform
// display form is a multi-slot expansion (see buildContributedHintCommand and
// buildContributedPaletteCommands) — and for the palette there are two of them
// — so such an override wouldn't match and simply falls back to the default,
// outside V1 phrase-override scope. See the passthrough handlers in
// bridge.go.
func extensionPatternToPlatform(pattern string) string {
	fields := strings.Fields(pattern)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == hintSlotToken || f == hintRepeatSlotToken || f == paletteSlotToken {
			out = append(out, f) // leave badge slots verbatim (not V1 scope)
			continue
		}
		if len(f) >= 2 && strings.HasPrefix(f, "{") && strings.HasSuffix(f, "}") {
			out = append(out, "<"+f[1:len(f)-1]+">")
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// platformPatternToExtension is the inverse of extensionPatternToPlatform:
// rewrites the platform's <name> capture notation back to the extension's
// {name} braces so a read-back override prefills the catalog-authored editor.
// Literal words (and untranslated hint slots) pass through.
func platformPatternToExtension(pattern string) string {
	fields := strings.Fields(pattern)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 && strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") {
			out = append(out, "{"+f[1:len(f)-1]+"}")
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// hintSlotToken marks the compound hint codeword in a contributed pattern.
// The extension can't express the dependent-capture pair (collection names and
// platform tags are plugin-owned), so it writes `{hint}` and this registrar
// expands it into the same shape as the skeleton's bare activate command
// (collections.go): prefix capture + strict-viewport dependent suffix capture,
// hints-tag gating, SetsOnPartial, and the suffix identity params.
//
// Why {hint} does NOT expand to the platform's hint_pair capture macro: a
// macro instance can't yet bridge-resume across an utterance chop, and the
// single-target verbs rely on that bridge ("blank arch" … "same"). The plain
// prefix/suffix expansion keeps full bridge semantics. The pair shape is
// still single-sourced in plugin.json's capture_macros — the drift test in
// contribute_test.go pins this expansion to the manifest declaration.
const hintSlotToken = "{hint}"

// hintRepeatSlotToken marks a repeated hint target list ("stash {hint+}").
// Expands to a reference to the plugin's manifest-declared `hint_pair`
// capture macro (`<targets:hint_pair>+`): the matcher consumes one-or-more
// pairs greedily and delivers `targets` as an ordered array of projected
// instances. Tail position only, like every open capture.
const hintRepeatSlotToken = "{hint+}"

// hintPairMacro is the manifest capture-macro these expansions reference; the
// declaration itself (slots, fields, cap) lives in plugin.json.
const hintPairMacro = "hint_pair"

// paletteSlotToken marks a command-palette row badge in a contributed pattern
// ("{palette}", "blank {palette}"). Like {hint} it exists because the badge's
// match shape is plugin-owned: the extension knows it wants "the row the user
// spoke", not that a one-word badge is a flat named-entity lookup while a
// two-word badge is a prefix index plus a dependent suffix capture.
//
// That split is the point. A single `{browser_palette}` capture — which is
// what the extension used to contribute — has no step boundary, so the matcher
// never emitted capture.progress for the palette and the whole narrowing
// transport downstream of it sat inert.
const paletteSlotToken = "{palette}"

// patternHasHintSlot reports whether a contributed pattern targets hints in
// either form.
func patternHasHintSlot(pattern string) bool {
	return strings.Contains(pattern, hintSlotToken) || strings.Contains(pattern, hintRepeatSlotToken)
}

// patternHasPaletteSlot reports whether a contributed pattern targets a
// palette row.
func patternHasPaletteSlot(pattern string) bool {
	return strings.Contains(pattern, paletteSlotToken)
}

// buildContributedPaletteCommands expands one {palette} contribution into a
// single `<row:@palette>` surface-bound spec.
// The PLATFORM desugars it into the atomic + pair variants — the disjoint
// projection, the pair tier's capture.progress step boundary, and the
// exclusive gating that used to be hand-built here all moved with it. The
// matched capture delivers the ROW RECORD, so `{row.row_id}` rides the
// action back to the extension exactly as before.
//
// Deliberately no ClearsTags: the surface's lifecycle is its rows, not a
// match — the extension closes the palette on dispatch, drains the rows,
// and the platform clears the tag.
//
// Only literal words may precede the slot, and it must be last — same rule
// and same failure handling as {hint}: dropped with a log line, not
// half-built.
func buildContributedPaletteCommands(pluginID string, c contributedCommand) ([]branchkit.CommandSpec, bool) {
	fields := strings.Fields(c.Pattern)
	lead := make([]branchkit.PatternSlot, 0, len(fields))
	for i, f := range fields {
		if f == paletteSlotToken {
			if i != len(fields)-1 {
				branchkit.Logf(pluginID, "contributed command %q: %s must be the last slot, dropping", c.Action, f)
				return nil, false
			}
			break
		}
		if strings.HasPrefix(f, "{") {
			branchkit.Logf(pluginID, "contributed command %q: only literal words may precede the palette slot, dropping", c.Action)
			return nil, false
		}
		lead = append(lead, branchkit.Word(f))
	}

	params := map[string]any{
		"row_id": "{row.row_id}",
	}
	for k, v := range c.Params {
		params[k] = v
	}
	b := branchkit.Command(append(append([]branchkit.PatternSlot{}, lead...),
		branchkit.Word("<row:@palette>"))...).
		Action(pluginID+"."+c.Action, params)
	if c.Category != "" {
		b = b.Category(c.Category)
	}
	if c.Description != "" {
		b = b.Description(c.Description)
	}
	return []branchkit.CommandSpec{b.Build()}, true
}

// buildContributedHintCommand expands one hint contribution into a
// CommandSpec — {hint} to the bridgeable prefix/suffix pair, {hint+} to the
// repeated hint_pair macro reference. Only literal words followed by a
// tail-position hint slot are supported ("blank {hint}", "stash {hint+}") —
// anything else is dropped with a log line, not half-built. Gates on the
// hints tag (not app-active, matching the skeleton activate), and clears it
// unless the contribution declared retains_hints.
func buildContributedHintCommand(pluginID string, c contributedCommand) (branchkit.CommandSpec, bool) {
	hintsTag := "plugin." + pluginID + ".hints"

	fields := strings.Fields(c.Pattern)
	slots := make([]branchkit.PatternSlot, 0, len(fields)+1)
	repeated := false
	for i, f := range fields {
		if f == hintSlotToken || f == hintRepeatSlotToken {
			if i != len(fields)-1 {
				branchkit.Logf(pluginID, "contributed command %q: %s must be the last slot, dropping", c.Action, f)
				return branchkit.CommandSpec{}, false
			}
			if f == hintRepeatSlotToken {
				// Repeated form: reference the manifest-declared capture
				// macro. Raw slot syntax because the SDK builder has no
				// repeat-quantifier sugar yet — a pattern slot is just a
				// string, and this is the platform's documented form.
				repeated = true
				slots = append(slots, branchkit.Word(fmt.Sprintf("<targets:%s>+", hintPairMacro)))
				continue
			}
			slots = append(slots,
				branchkit.Capture("prefix", "plugin.browser.alpha"),
				branchkit.Capture("suffix", fmt.Sprintf("plugin.%s.hints_${prefix.codeword}_strict", pluginID)),
			)
			continue
		}
		if strings.HasPrefix(f, "{") {
			branchkit.Logf(pluginID, "contributed command %q: only literal words may precede the hint slot, dropping", c.Action)
			return branchkit.CommandSpec{}, false
		}
		slots = append(slots, branchkit.Word(f))
	}

	var params map[string]any
	if repeated {
		// The macro's `fields` projection (plugin.json) shapes each instance;
		// the action gets the ordered array under `targets`.
		params = map[string]any{
			"targets": "{targets}",
		}
	} else {
		params = sealedHintFields()
	}
	for k, v := range c.Params {
		params[k] = v
	}
	// Hybrid pair (see buildAllHintCommands): sealed prefix, STRICT suffix —
	// the second word matches only the per-prefix live set, so a not-shown
	// codeword is a silenced non-continuation, not a no-op completion. The
	// prefix DisplaySource still points the HUD at the live prefixes (the
	// prefix match is sealed browser_alpha); the suffix DisplaySource is now
	// congruent with the suffix MATCH collection. SetsOnPartial is permitted
	// again on this dependent shape but left off — capture.progress already
	// drives the mid-pair narrowing.
	b := branchkit.Command(slots...).
		Action(pluginID+"."+c.Action, params).
		RequiresTags(hintsTag).
		DisplaySource("prefix", hintCollection(pluginID, "prefix")).
		DisplaySource("suffix", fmt.Sprintf("plugin.%s.hints_${prefix.codeword}_strict", pluginID))
	if !c.RetainsHints {
		b = b.ClearsTags(hintsTag)
	}
	if c.Category != "" {
		b = b.Category(c.Category)
	}
	if c.Description != "" {
		b = b.Description(c.Description)
	}
	return b.Build(), true
}

// contributedHintTagEffects reports whether subAction belongs to a contributed
// {hint} verb and, if so, its hint-tag lifecycle — the contributed twin of
// hintTagSideEffects (collections.go), consulted by handleOnAction so the gate
// cache mirrors the manifest ClearsTags of contributed verbs too. A clearing
// verb behaves exactly like activate (clear + re-arm in always mode); a
// retains_hints verb leaves tag and gate untouched.
func (h *Host) contributedHintTagEffects(subAction, hintVisibility string) (clearActive, rePut, ok bool) {
	for _, c := range h.contributedUnion() {
		if c.Action != subAction || !patternHasHintSlot(c.Pattern) {
			continue
		}
		if c.RetainsHints {
			return false, false, true
		}
		return true, hintVisibility == "always", true
	}
	return false, false, false
}

// buildContributedCommands builds CommandSpecs from the extension-contributed
// set, applying THIS plugin's defaults: the action-id prefix, the app-active
// context gate, and the category. {hint} patterns take the dependent-capture
// expansion path instead. Returns nil when nothing has been contributed
// yet (the startup window before the extension connects). Called from
// pushAllCommands under its push lock.
func (h *Host) buildContributedCommands(pluginID string) []branchkit.CommandSpec {
	union := h.contributedUnion()
	if len(union) == 0 {
		return nil
	}
	appTag := "plugin." + pluginID + ".active"
	out := make([]branchkit.CommandSpec, 0, len(union))
	sawVideoContext := false
	for _, c := range union {
		if c.Action == "" || strings.TrimSpace(c.Pattern) == "" {
			continue
		}
		if patternHasHintSlot(c.Pattern) {
			if spec, ok := buildContributedHintCommand(pluginID, c); ok {
				// Every hint verb also works inside caret/visual selection
				// (caretHintTwin) — hints don't force a mode exit.
				out = append(out, spec, caretHintTwin(pluginID, spec))
			}
			continue
		}
		if patternHasPaletteSlot(c.Pattern) {
			if specs, ok := buildContributedPaletteCommands(pluginID, c); ok {
				out = append(out, specs...)
			}
			continue
		}
		slots := parsePatternSlots(c.Pattern)
		if len(slots) == 0 {
			continue
		}
		if c.Context == "video" {
			// Media transport commands ship as TWO variants (see video.go):
			//  - presence-gated: eligible whenever the focused tab has a large
			//    video (non-exclusive augment — normal browsing untouched);
			//  - mode-gated: eligible inside the exclusive "video" discovery
			//    mode, refreshing the mode tag like snap's directionals so a
			//    run of commands ("faster" … "faster" … "pause") holds it.
			// Exclusive filtering suppresses commands that don't require an
			// active exclusive tag, so one variant can't serve both states.
			sawVideoContext = true
			mediaTag := mediaActiveTagName(pluginID)
			modeTag := videoModeTagName(pluginID)
			// NO app-active gate on the bare variant: the media-active union
			// (which includes background audible tabs) is the whole
			// eligibility story — "pause" from an unrelated app is the
			// feature. Exposure is bounded by the union: nothing playing or
			// present anywhere → tag down → words not hearable.
			// The mode variants no longer refresh the tag (no sets): since Wave 3
			// C4b the tag is a sticky, introducer-written mirror of the
			// extension's layer — there is no hold-scoped clear to outrun.
			for _, gate := range []struct {
				requires, sets []string
				mode           bool
			}{
				{requires: []string{mediaTag}},
				{requires: []string{modeTag}, mode: true},
			} {
				b := branchkit.Command(slots...)
				if len(c.Params) > 0 {
					b = b.Action(pluginID+"."+c.Action, anyParams(c.Params))
				} else {
					b = b.Action(pluginID + "." + c.Action)
				}
				b = b.RequiresTags(gate.requires...)
				if len(gate.sets) > 0 {
					b = b.SetsTags(gate.sets...)
				}
				if c.Category != "" {
					b = b.Category(c.Category)
				}
				// Mode variants carry NO description: the video-mode HUD lists
				// all ~9 words in a fixed-height panel, and a repeated full
				// command description per row overflows it (clipping random
				// rows). The words are the guessable-vocabulary kind; bare
				// word rows read cleaner and all fit. The presence variants
				// keep descriptions — they surface mixed into general
				// discovery, where the context helps.
				if !gate.mode && c.Description != "" {
					b = b.Description(c.Description)
				}
				out = append(out, b.Build())
			}
			continue
		}
		var b *branchkit.CommandBuilder
		if len(c.Params) > 0 {
			b = branchkit.Command(slots...).Action(pluginID+"."+c.Action, anyParams(c.Params))
		} else {
			b = branchkit.Command(slots...).Action(pluginID + "." + c.Action)
		}
		if c.Context == "palette" {
			// Gate on the PLATFORM's surface tag; no ClearsTags — the
			// surface's lifecycle is its rows (the extension drains them on
			// dispatch), never a match.
			b = b.RequiresTags(paletteSurfaceTag(pluginID))
		} else if c.Context == "caret" {
			// Gate on the caret tag but do NOT clear it at match: caret mode is
			// sustained (many selection commands per session), and the tag is a
			// pure mirror of the extension's mode — the extension clears it on
			// exit (its session's caret going false), never the matcher. See caret.go.
			b = b.RequiresTags(caretTagName(pluginID))
		} else if c.Context == "auto_scroll" {
			// Keep-scrolling's in-mode words ("faster", "slower", "stop"):
			// eligible only while the run holds the exclusive auto_scroll tag
			// (autoscroll.go). Not cleared at match — many "faster"s per run;
			// the tag is a pure mirror of the extension's run, cleared when it
			// ends however it ends.
			b = b.RequiresTags(autoScrollTagName(pluginID))
		} else if c.Context == "find" {
			// Find-jump: eligible only while a find session is live AND this
			// browser is focused. app-active carries the focus half because the
			// find tag deliberately survives blur (find.go) — the session does
			// too. Not cleared at match: many jumps per session.
			b = b.RequiresTags(appTag, findTagName(pluginID))
		} else {
			b = b.RequiresTags(appTag)
		}
		// Prefix-discovery pass-through — no per-mode branch. The platform
		// desugars `discovery: exclusive` into a mode-entry + gated capture and
		// mints its own exclusive tag, so the plugin just forwards the semantic.
		if c.Discovery != "" {
			b = b.Discovery(c.Discovery)
		}
		if c.Category != "" {
			b = b.Category(c.Category)
		}
		if c.Description != "" {
			b = b.Description(c.Description)
		}
		out = append(out, b.Build())
	}
	if sawVideoContext {
		// The "video" mode entry — an ACTION command since Wave 3 C4b: it
		// drives the extension's video key layer (the same entry `w` takes),
		// and the exclusive tag is the mode stack's MIRROR of that one
		// lifetime, not a matcher-side sets_tags. Presence-gated so "video" is
		// only speakable where a video exists. In one breath, "video pause"
		// still decodes: the media words carry presence-gated variants in the
		// free-context grammar, so no sets_tags chain is needed. The global
		// "over" clears the tag and the forwarder (caret.go modeMirrors) exits
		// the key layer with it.
		out = append(out, branchkit.Command(branchkit.Word("video")).
			RequiresTags(mediaActiveTagName(pluginID)).
			Action(pluginID+".video_mode").
			Category("Video Mode").
			Description("Enter video mode — pause, play, faster, slower, skip, mute and restart stay live without prefixes; say \"over\" to exit.").
			Build())
	}
	return out
}

// handleCommandsContributePost receives the extension's command vocabulary,
// stores it under the posting CONNECTION, and re-pushes the union so the
// contributed commands go live. Idempotent: a re-POST of the same set yields
// the same push.
//
// `conn_id` scopes the store so one browser's contribution can never delete
// another's — see the note on `contributed`. It is required: an unstamped
// catalog would share one bucket across every browser, the last-writer-wins
// loss the per-connection store exists to prevent.
func (h *Host) handleCommandsContributePost(_ *http.Request, body *wire.ContributeRequest) (wire.Ack, error) {
	if body.ConnID == "" {
		return wire.Ack{}, badRequest("missing conn_id")
	}
	h.storeContributedCommands(body.ConnID, body.Commands)
	h.mu.Lock()
	h.contribMu.Lock()
	pruned := h.pruneContributionsLocked(body.ConnID)
	h.contribMu.Unlock()
	h.mu.Unlock()
	pluginID := h.state.PluginID
	union, conflicts := h.contributedUnionWithConflicts()
	// Conflicts are logged rather than absorbed: they mean two live connections
	// disagree about what a phrase does, which in development is a browser that
	// hasn't reloaded, and in the field would be a genuine version skew. Silence
	// here is what made a retired descriptor look like a code change that didn't
	// take (2026-07-26).
	if conflicts > 0 {
		branchkit.Logf(pluginID, "commands/contribute: %d spoken form(s) contributed with conflicting shapes — newest contribution wins (a browser running an older build?)", conflicts)
	}
	branchkit.Logf(pluginID, "commands/contribute: conn=%q commands=%d union=%d pruned=%d conflicts=%d",
		body.ConnID, len(body.Commands), len(union), pruned, conflicts)
	if err := h.pushCommandGroup(pluginID, groupContributed); err != nil {
		branchkit.Logf(pluginID, "commands/contribute: push failed: %v", err)
		return wire.Ack{}, statusError(http.StatusBadGateway, "push failed")
	}
	return ack()
}

// handleCommandOverridePost forwards an extension override request to the
// platform. Body: {action, default_pattern, new_pattern} where `action` is the
// bare command id (as contributed) and the patterns use the extension's {..}
// capture notation. The plugin prefixes the action with its own id and
// translates the patterns to the platform's <..> notation so the key matches
// the platform's stored (action, default_pattern) identity. Validation is the
// platform's job; its error message is relayed to the extension for display.
func (h *Host) handleCommandOverridePost(_ *http.Request, req *wire.PhraseEdit) (wire.Ack, error) {
	if req.Action == "" || req.DefaultPattern == "" {
		return wire.Ack{}, badRequest("missing action or default_pattern")
	}
	pluginID := h.state.PluginID
	if err := h.plugin.CommandsSetOverride(branchkit.CommandsSetOverrideRequest{
		Action:         pluginID + "." + req.Action,
		DefaultPattern: extensionPatternToPlatform(req.DefaultPattern),
		NewPattern:     extensionPatternToPlatform(req.NewPattern),
	}); err != nil {
		branchkit.Logf(pluginID, "commands/override: set failed: %v", err)
		return wire.Ack{}, badRequest(err.Error())
	}
	return ack()
}

// handleCommandOverrideResetPost reverts an override to the plugin default.
// Body: {action, default_pattern}. Same prefixing/translation as the set path.
func (h *Host) handleCommandOverrideResetPost(_ *http.Request, req *wire.OverrideResetRequest) (wire.Ack, error) {
	if req.Action == "" || req.DefaultPattern == "" {
		return wire.Ack{}, badRequest("missing action or default_pattern")
	}
	pluginID := h.state.PluginID
	if _, err := h.plugin.CommandsResetOverride(branchkit.CommandsResetOverrideRequest{
		Action:         pluginID + "." + req.Action,
		DefaultPattern: extensionPatternToPlatform(req.DefaultPattern),
	}); err != nil {
		branchkit.Logf(pluginID, "commands/override: reset failed: %v", err)
		return wire.Ack{}, badRequest(err.Error())
	}
	return ack()
}

// handleCommandOverridesGet returns this plugin's current phrase overrides so
// the extension editor can prefill fields and mark changed rows. Filters the
// platform's full set to this plugin's action prefix and translates back to the
// extension's bare-id + {..} notation.
func (h *Host) handleCommandOverridesGet(_ *http.Request, _ *wire.Empty) (wire.OverridesResponse, error) {
	pluginID := h.state.PluginID
	prefix := pluginID + "."
	all, err := h.plugin.CommandsListOverrides()
	if err != nil {
		branchkit.Logf(pluginID, "commands/overrides: list failed: %v", err)
		return wire.OverridesResponse{}, statusError(http.StatusBadGateway, err.Error())
	}
	return wire.OverridesResponse{Overrides: phraseEditViews(all, prefix)}, nil
}

// phraseEditView is the extension-facing shape for a stored override or alias:
// bare action id + {..} capture notation (wire.PhraseEdit).
type phraseEditView = wire.PhraseEdit

// phraseEditViews filters the platform's full override/alias list to this
// plugin's action prefix and translates each back to the extension's notation.
func phraseEditViews(all []branchkit.CommandOverride, prefix string) []phraseEditView {
	views := make([]phraseEditView, 0, len(all))
	for _, o := range all {
		if !strings.HasPrefix(o.Action, prefix) {
			continue
		}
		views = append(views, phraseEditView{
			Action:         strings.TrimPrefix(o.Action, prefix),
			DefaultPattern: platformPatternToExtension(o.DefaultPattern),
			NewPattern:     platformPatternToExtension(o.NewPattern),
		})
	}
	return views
}

// handleCommandAliasPost adds an extra spoken form (alias) for one of this
// plugin's commands. Same prefixing + {..}->.. translation as the override
// path; the platform clones the base behavior and appends the new phrase.
func (h *Host) handleCommandAliasPost(_ *http.Request, req *wire.PhraseEdit) (wire.Ack, error) {
	if req.Action == "" || req.DefaultPattern == "" || req.NewPattern == "" {
		return wire.Ack{}, badRequest("missing action, default_pattern, or new_pattern")
	}
	pluginID := h.state.PluginID
	if err := h.plugin.CommandsAddAlias(branchkit.CommandsAddAliasRequest{
		Action:         pluginID + "." + req.Action,
		DefaultPattern: extensionPatternToPlatform(req.DefaultPattern),
		NewPattern:     extensionPatternToPlatform(req.NewPattern),
	}); err != nil {
		branchkit.Logf(pluginID, "commands/alias: add failed: %v", err)
		return wire.Ack{}, badRequest(err.Error())
	}
	return ack()
}

// handleCommandAliasRemovePost removes one added spoken form (exact triple).
func (h *Host) handleCommandAliasRemovePost(_ *http.Request, req *wire.PhraseEdit) (wire.Ack, error) {
	if req.Action == "" || req.DefaultPattern == "" || req.NewPattern == "" {
		return wire.Ack{}, badRequest("missing action, default_pattern, or new_pattern")
	}
	pluginID := h.state.PluginID
	if _, err := h.plugin.CommandsRemoveAlias(branchkit.CommandsRemoveAliasRequest{
		Action:         pluginID + "." + req.Action,
		DefaultPattern: extensionPatternToPlatform(req.DefaultPattern),
		NewPattern:     extensionPatternToPlatform(req.NewPattern),
	}); err != nil {
		branchkit.Logf(pluginID, "commands/alias: remove failed: %v", err)
		return wire.Ack{}, badRequest(err.Error())
	}
	return ack()
}

// handleCommandAliasesGet returns this plugin's added spoken forms so the
// editor can list + remove them.
func (h *Host) handleCommandAliasesGet(_ *http.Request, _ *wire.Empty) (wire.AliasesResponse, error) {
	pluginID := h.state.PluginID
	all, err := h.plugin.CommandsListAliases()
	if err != nil {
		branchkit.Logf(pluginID, "commands/aliases: list failed: %v", err)
		return wire.AliasesResponse{}, statusError(http.StatusBadGateway, err.Error())
	}
	return wire.AliasesResponse{Aliases: phraseEditViews(all, pluginID+".")}, nil
}
