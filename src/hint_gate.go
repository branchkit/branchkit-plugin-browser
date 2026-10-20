package main

import (
	"sync"

	"github.com/branchkit/plugin-sdk-go"
)

// hintGateState encodes the lifecycle of `plugin.<id>.hints`. The tag
// gates browser hint codeword matching; without it codewords like
// "arch" fall through to the alphabet command and type letters.
//
// Under the read-time projection the lifecycle
// is two states:
//
//   - Idle:         tag is not set
//   - GrammarOwned: tag is set after the first batch of a scan lands
//
// The EagerlyArmed third state + 2s self-clear timer existed under the
// earlier push-time architecture as a prediction layer bridging the gap
// between focus arrival and grammar push completion (which could be
// 200ms–2s on a complex page). Per-batch grammar lands the first
// batch within ~100ms of focus and the gate fires from that batch's
// transitionToGrammarOwned — the prediction window is gone, so the
// prediction layer goes with it.
type hintGateState int

const (
	hintGateIdle hintGateState = iota
	hintGateGrammarOwned
)

func (s hintGateState) String() string {
	switch s {
	case hintGateIdle:
		return "idle"
	case hintGateGrammarOwned:
		return "grammar_owned"
	}
	return "unknown"
}

// hintGate is the explicit state machine for plugin.<id>.hints. It
// owns the Put/Delete RPCs and state transitions — callers signal
// events ("grammar arrived," "session ended," "tag externally
// cleared") via method calls.
//
// Concurrency: the gate's mutex is held across the Put/Delete RPC so
// that concurrent transitions can't interleave their state update
// with another transition's RPC (which would leave the cache and
// the platform's view of the tag disagreeing). The RPCs are local
// JSON-RPC to the platform and don't synchronously re-enter the
// plugin, so holding the lock during them is safe.
type hintGate struct {
	host     *Host
	mu       sync.Mutex
	state    hintGateState
	pluginID string
}

func newHintGate(h *Host, pluginID string) *hintGate {
	return &hintGate{host: h, pluginID: pluginID}
}

func (g *hintGate) tag() string {
	return hintsTagName(g.pluginID)
}

func hintsTagName(pluginID string) string {
	return "plugin." + pluginID + ".hints"
}

// Test seams, same pattern as caret's putCaretTag/deleteCaretTag. The gate's
// own Put and Delete go through them.
func (h *Host) putHintsTagDefault(pluginID string) error {
	return h.plugin.Put(hintsTagName(pluginID), "singleton", struct{}{})
}
func (h *Host) deleteHintsTagDefault(pluginID string) error {
	_, err := h.plugin.Delete(hintsTagName(pluginID), "singleton")
	return err
}

// IsActive reports whether the tag is currently held.
func (g *hintGate) IsActive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state == hintGateGrammarOwned
}

// transitionToGrammarOwned sets the tag (idempotent on re-call). The
// only non-Idle state under the per-batch protocol — there's no eager-arm
// distinction anymore.
//
// RPC first, state on success. The state used to be set before the Put, so a
// Put that failed left the gate believing it held a tag the platform never
// got, and every later arm SET-skipped: codewords painted, voice-dead, until
// something cleared the gate. Left Idle, the next arm retries.
func (g *hintGate) transitionToGrammarOwned(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == hintGateGrammarOwned {
		branchkit.Logf(g.pluginID, "hints tag SET-skipped reason=%s grammar_already_owns", reason)
		return
	}
	if err := g.host.putHintsTag(g.pluginID); err != nil {
		branchkit.Logf(g.pluginID, "hints tag PUT failed reason=%s (gate stays idle — next arm retries): %v", reason, err)
		return
	}
	g.state = hintGateGrammarOwned
	branchkit.Logf(g.pluginID, "hints tag SET reason=%s", reason)
}

// transitionToIdle clears the tag. Always issues the Delete
// (idempotent in the platform) so cache drift from prior bugs can't
// leak a platform-side tag past a clear signal.
func (g *hintGate) transitionToIdle(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = hintGateIdle
	if err := g.host.deleteHintsTag(g.pluginID); err != nil {
		branchkit.Logf(g.pluginID, "hints tag DELETE failed reason=%s: %v", reason, err)
		return
	}
	branchkit.Logf(g.pluginID, "hints tag CLEARED reason=%s", reason)
}

// markCleared updates internal state to Idle without issuing an RPC.
// Used when an external actor cleared the tag (e.g., a matched
// command's manifest `clearsTags` fired) — we just need to mirror
// the external clear in our state.
func (g *hintGate) markCleared(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = hintGateIdle
	branchkit.Logf(g.pluginID, "hints tag marked CLEARED reason=%s (external)", reason)
}
