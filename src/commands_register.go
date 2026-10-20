package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// Browser has five independent command sources. They used to push
// independently, and commands.push replaced the plugin's WHOLE set on every
// call — so whichever push landed last was the only set the matcher saw. The
// hint skeleton routinely lost to reference-update re-pushes; the calibration
// UI's dynamic-command chip appeared only when the skeleton happened to be
// most recent.
//
// The workaround was to give up on independent pushes: one union rebuilt from
// all five builders on every call, serialized by a mutex. It worked, but it
// meant a reference rename re-derived the hint skeleton and the contributed
// catalog to say nothing new about either, and every new source had to be
// wired into the union or it would not survive anyone else's push.
//
// commands.push now takes a group. Each source owns one, replaces only its
// own, and needs to know nothing about the others — so the union is gone and
// each call site pushes just what it changed. The union was this plugin holding
// state (the other four sources' current contents) that the platform is in a
// better position to hold.
const (
	// Scroll / find / page-navigation phrases owned by the browser EXTENSION
	// and contributed over POST /commands/contribute; this plugin is a thin
	// registrar. Empty until an extension connects.
	groupContributed = "contributed"
	// Formerly "over" = the spoken Esc. The platform owns "over" now: it closes
	// the most recent mode, else presses Escape, and the extension's cascade
	// answers the Escape key exactly as it answered this command — one layer,
	// newest first, and plain Escape reaches the page when nothing is open.
	// One "over" means it can never tie with a mode's own exit (spelling). The
	// group stays and pushes nothing, so a running platform drops the old
	// command rather than keeping it until restart.
	groupEscape = "escape"
	// Tab-to-desk / tab-to-window surgery verbs (tab_desk.go). Plugin-built:
	// they orchestrate extension + placement-plugin work, not a bare SSE forward.
	groupTabDesk = "tab_desk"
	// "reference <name>" over the saved-reference list (references.go).
	groupReferences = "references"
	// Hint trigger / hide / activate / cancel. Empty until the alphabet has
	// loaded — before then these reference codewords that aren't in the
	// entity_cache yet.
	groupHints = "hints"
)

// pushMu serializes build-then-push so two near-simultaneous callers on the
// same group can't interleave into a stale result: each builder reads live
// plugin state, so without this, A could read, B read, B push, A push — and
// the group would settle on A's older snapshot.
//
// This is all that survives of the old union lock. It no longer exists to stop
// sources from clobbering each other — groups do that — so it can be held
// across one group's build and push instead of all five, and a source that
// wants its own lock can take one without consulting this file.

// Test seam, same pattern as replaceTabsCollection. Tests assert on which
// group a call site pushed — the thing this migration can plausibly get
// wrong is routing a source's change to the wrong group, which the compiler
// cannot catch since every group is a string.
func (h *Host) pushCommandGroupRPCDefault(group string, cmds []branchkit.CommandSpec) (int, error) {
	return branchkit.PushCommandGroup(h.plugin, group, cmds)
}

// pushCommandGroup rebuilds one source's commands and replaces just that
// group. The other four are untouched, whatever state they're in.
func (h *Host) pushCommandGroup(pluginID, group string) error {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()
	return h.pushCommandGroupLocked(pluginID, group)
}

func (h *Host) pushCommandGroupLocked(pluginID, group string) error {
	// A source with nothing to say pushes an EMPTY group rather than skipping.
	// Skipping would leave the previous contents live, which for the hint
	// skeleton means codewords surviving an alphabet that no longer has them.
	var cmds []branchkit.CommandSpec
	switch group {
	case groupContributed:
		cmds = h.buildContributedCommands(pluginID)
	case groupEscape:
		cmds = nil // see groupEscape
	case groupTabDesk:
		cmds = buildTabDeskCommands(pluginID, "plugin."+pluginID+".active")
	case groupReferences:
		cmds = h.buildReferenceCommands(pluginID)
	case groupHints:
		cmds = h.buildHintSkeletonCommands(pluginID)
	}

	total, err := h.pushCommandGroupRPC(group, cmds)
	if err != nil {
		branchkit.Logf(pluginID, "push command group %q: %v", group, err)
		return err
	}
	branchkit.Logf(pluginID, "push command group %q: %d commands (%d live for plugin)",
		group, len(cmds), total)
	return nil
}

// pushAllCommandGroups pushes every group, including the ones that are
// currently empty. Used at startup to make the plugin's whole command set
// determined in one pass rather than waiting for each source's first change.
func (h *Host) pushAllCommandGroups(pluginID string) error {
	h.pushMu.Lock()
	defer h.pushMu.Unlock()

	var firstErr error
	for _, group := range []string{
		groupContributed, groupEscape, groupTabDesk, groupReferences, groupHints,
	} {
		// Push every group even if one fails: they're independent, and
		// stopping early would leave the rest of the set unpublished over an
		// unrelated source's error.
		if err := h.pushCommandGroupLocked(pluginID, group); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
