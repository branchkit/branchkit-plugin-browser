package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Calibration fixture: hydrates `browser_hints_prefix` and
// `browser_hints_<X>` (×26) to the alphabet × alphabet maximum (26
// prefixes + 676 prefix/suffix combos), asserts the hints tag, and
// fires `vocabulary.commit` so the dynamic recognizer picks up
// the newly-pushed codewords.
//
// vocabulary.commit is a Path A *refresh* of the union vocab,
// not a narrow. The engine keeps
// the full grammar; the matcher's `plugin.browser.hints` tag + entity-
// cache filter does the contextual narrowing that the realistic trial
// is actually measuring.
//
// Used by `plugin.trial_apply_fixture`. The platform forwards
// the trial host's `trial.enter_context` RPC here when the host
// asks to calibrate the hint command. On `release`, all the writes
// reverse — per-prefix collections are wiped, the prefix list goes
// empty, the tag clears, and vocabulary.commit fires again so the
// recognizer rebuilds without the hint codewords.
//
// While the fixture is applied, `handleGrammarFramePut` short-circuits
// — incoming extension batches would otherwise REPLACE the hydrated
// collections with whatever happens to be on the user's screen,
// stomping the maximal vocabulary the calibration trial wants to
// measure against. The user is in the calibration UI; we don't need
// live hints from the browser tab during a trial.

type calibrationFixture struct {
	mu     sync.Mutex
	active bool
	handle string
}

// IsActive reports whether a calibration fixture is currently applied.
// Used by `handleGrammarFramePut` to skip extension-driven writes
// during a trial.
func (c *calibrationFixture) IsActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// applyHintsFixture hydrates the hint collections to the full alphabet ×
// alphabet maximum. When no fixture is active it's a fresh apply (mints a
// handle, asserts the gate); when one is already active it's a *refresh in
// place* — the codeword retune loop re-applies on the same open trial so a
// mid-walk override flows into the grammar without a release→reapply toggle
// (whose release is async and would race the re-apply). A refresh keeps the existing
// handle and leaves the gate asserted; only the collection hydration + vocab
// commit re-run.
func (h *Host) applyHintsFixture(pluginID string) (string, error) {
	h.calibration.mu.Lock()
	defer h.calibration.mu.Unlock()

	refresh := h.calibration.active

	// Always re-read the alphabet so a mid-walk codeword override (retune) is
	// picked up. refreshCodewords no-ops when the mirror isn't wired
	// (tests run with plugin==nil and pre-populate the codewords global).
	h.refreshCodewords()
	words := h.snapshotCodewords()
	if words == nil {
		return "", fmt.Errorf("alphabet not yet loaded — voice plugin must populate it before calibration")
	}

	// On refresh, a retuned codeword renames its prefix collection
	// (browser_hints_<old> → browser_hints_<new>). The re-hydrate below only
	// diffs surviving prefixes, so evict any prefix collection whose codeword
	// is gone from the fresh snapshot — otherwise the old one is orphaned in
	// the grammar. Keyed off what the PLATFORM says we currently hold records
	// under — not the snapshot — so the stale name is the one removed, and a
	// fixture the PREVIOUS process hydrated is still reachable here.
	if refresh {
		current := make(map[string]bool, len(words))
		for _, w := range words {
			current[w] = true
		}
		stale, err := h.livePrefixes(pluginID)
		if err != nil {
			// A refresh that can't see what it holds may leave a renamed-away
			// prefix in the grammar; the next refresh or the release catches it.
			branchkit.Logf(pluginID, "calibration refresh: live prefixes unavailable: %v", err)
		}
		for prefix := range stale {
			if !current[prefix] {
				if err := h.syncPerPrefix(pluginID, prefix, map[string]any{}); err != nil {
					branchkit.Logf(pluginID, "calibration refresh: evict stale prefix %s failed: %v", prefix, err)
				}
			}
		}
	}

	// 1. Sync all 26 codewords into the prefix list. Entries are
	//    object-shaped ({codeword, letter}) so the discovery HUD can
	//    render the letter as a subtitle (secondary role) while the
	//    activate pattern's `${prefix.codeword}` template still
	//    resolves to the codeword-named per-prefix collection.
	prefixData := make(map[string]any, 26)
	for i, w := range words {
		prefixData[w] = map[string]string{
			"codeword": w,
			"letter":   string(byte('a' + i)),
		}
	}
	if err := h.syncPrefixList(pluginID, prefixData); err != nil {
		return "", fmt.Errorf("sync prefix list: %w", err)
	}

	// 2. For each of 26 prefixes, sync the per-prefix collection
	//    containing all 26 suffix codewords with synthetic payloads
	//    matching the shape the activate command's action template
	//    references (letter / id / frame_id / codeword / type).
	for prefixIdx, prefixWord := range words {
		suffixData := make(map[string]any, 26)
		for suffixIdx, suffixWord := range words {
			letter := string([]byte{byte('a' + prefixIdx), byte('a' + suffixIdx)})
			suffixData[suffixWord] = map[string]any{
				"letter":   letter,
				"id":       "0",
				"frame_id": "0",
				"codeword": prefixWord + " " + suffixWord,
				"type":     "calibration",
			}
		}
		if err := h.syncPerPrefix(pluginID, prefixWord, suffixData); err != nil {
			return "", fmt.Errorf("sync %s: %w", hintCollection(pluginID, prefixWord), err)
		}
	}

	// 3. Assert the hint gate so the activate command's RequiresTag
	//    matches during the trial. Skipped on refresh — the gate is already
	//    grammar-owned from the original apply. Tests run without a live
	//    `state` global — guard so the unit tests can exercise the push-shape
	//    work without setting up the full plugin runtime.
	if !refresh && h.state != nil && h.state.HintGate != nil {
		h.state.HintGate.transitionToGrammarOwned("calibration_apply")
	}

	// 4. Refresh the vocabulary so the newly-hydrated codewords enter
	//    the union grammar. Not narrowing — vocabulary.commit broadcasts
	//    the full union vocab + plugin HWMs (see §5d). The matcher's
	//    plugin.browser.hints tag + entity-cache filter does the actual
	//    contextual narrowing the trial is measuring.
	if err := h.commitVocabulary(); err != nil {
		return "", fmt.Errorf("vocabulary.commit: %w", err)
	}

	// Refresh keeps the existing handle (the trial registered it once); a fresh
	// apply mints a new one.
	handle := h.calibration.handle
	if !refresh {
		handle = fmt.Sprintf("cal_hints_%d", time.Now().UnixNano())
	}
	h.calibration.active = true
	h.calibration.handle = handle
	verb := "applied"
	if refresh {
		verb = "refreshed"
	}
	branchkit.Logf(pluginID, "calibration fixture %s: handle=%s, prefixes=%d, per_prefix_size=%d",
		verb, handle, len(words), len(words))
	return handle, nil
}

func (h *Host) releaseHintsFixture(pluginID, handle string) error {
	h.calibration.mu.Lock()
	defer h.calibration.mu.Unlock()

	if !h.calibration.active {
		return fmt.Errorf("no calibration fixture active")
	}
	if h.calibration.handle != handle {
		return fmt.Errorf("handle mismatch: expected %s, got %s", h.calibration.handle, handle)
	}

	// 1. Wipe per-prefix collections by syncing empty maps. syncPerPrefix
	//    declares an empty desired state against the alphabet × alphabet set
	//    apply built up, so the platform clears each prefix's collection.
	//    Iterate what the PLATFORM says we still hold records under — not the
	//    live codeword snapshot — so a codeword retuned mid-walk has its
	//    (renamed-away) prefix collection wiped here too rather than orphaned,
	//    and so does one a PREVIOUS process hydrated. Best-effort: on
	//    partial failure, surface the first error but continue wiping so we
	//    leave as little residue as possible.
	var firstErr error
	held, err := h.livePrefixes(pluginID)
	if err != nil {
		// Surfaced, not swallowed: without the enumeration this release wipes
		// nothing, and reporting success would hide the residue it left behind.
		branchkit.Logf(pluginID, "release: live prefixes unavailable: %v", err)
		firstErr = fmt.Errorf("enumerate held prefixes: %w", err)
	}
	for prefixWord := range held {
		if err := h.syncPerPrefix(pluginID, prefixWord, map[string]any{}); err != nil {
			branchkit.Logf(pluginID, "release: wipe %s failed: %v", hintCollection(pluginID, prefixWord), err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// 2. Wipe prefix list. Same diff pattern at the prefix-list scope —
	//    an empty desired state clears whatever the collection currently
	//    holds, so this one needs no enumeration of its own.
	if err := h.syncPrefixList(pluginID, map[string]any{}); err != nil {
		branchkit.Logf(pluginID, "release: wipe prefix list failed: %v", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	// 3. Clear the hint gate. Guard for tests as in apply.
	if h.state != nil && h.state.HintGate != nil {
		h.state.HintGate.transitionToIdle("calibration_release")
	}

	// 4. Refresh the vocabulary so the recognizer rebuilds without
	//    the released codewords. Symmetric with apply's step 4.
	if err := h.commitVocabulary(); err != nil {
		branchkit.Logf(pluginID, "release: vocabulary.commit failed: %v", err)
	}

	h.calibration.active = false
	h.calibration.handle = ""
	branchkit.Logf(pluginID, "calibration fixture released: handle=%s", handle)
	return firstErr
}

func (h *Host) handleTrialApplyFixture(_ *branchkit.TrialApplyFixtureRequest) (any, error) {
	// Today the browser plugin owns exactly one dynamic command (the
	// hint activate). When more dynamic commands appear, dispatch on
	// command_id here.
	handle, err := h.applyHintsFixture(h.state.PluginID)
	if err != nil {
		return nil, err
	}
	return branchkit.TrialApplyFixtureResponse{FixtureHandle: handle}, nil
}

func (h *Host) handleTrialReleaseFixture(req *branchkit.TrialReleaseFixtureRequest) (any, error) {
	if err := h.releaseHintsFixture(h.state.PluginID, req.FixtureHandle); err != nil {
		return nil, err
	}
	// Codegen omits empty-result types; return a literal empty object.
	return struct{}{}, nil
}
