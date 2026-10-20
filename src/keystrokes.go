package main

import (
	"runtime"

	"github.com/branchkit/plugin-sdk-go"
)

// Native keystrokes this plugin presses on the user's behalf, through the
// platform (input.shortcut_by_name → the keyboard plugin), for the jobs no
// extension API can do. A synthetic DOM event is untrusted, so the browser
// ignores it for exactly these jobs; a real OS keystroke is trusted.

// osShortcut is one keystroke in input.shortcut_by_name's vocabulary: a key
// name and modifier names from the platform's key-name table
// (actions.rs map_key_name — "down", "alt", "cmd", "ctrl").
type osShortcut struct {
	Name      string
	Modifiers []string
}

// osShortcuts is every keystroke this plugin presses, per OS. ONE table so a
// key that turns out wrong on one platform is a one-line change here.
type osShortcuts struct {
	// Focuses the browser's address bar: Cmd+L on macOS, Ctrl+L elsewhere
	// (Chrome and Firefox alike). Not "cmd" everywhere: off macOS the
	// platform maps "cmd" to the Meta key, and Meta+L locks the screen on
	// Windows and on GNOME.
	addressBar osShortcut
	// Opens the native picker of the <select> the extension just focused.
	// Alt+Down is what Rango presses. UNVERIFIED LIVE (2026-09-24) on every
	// OS: on some platforms Space may be the key that opens a picker.
	openSelect osShortcut
}

// shortcutsFor returns the keystroke table for one OS (runtime.GOOS).
func shortcutsFor(goos string) osShortcuts {
	switch goos {
	case "darwin":
		return osShortcuts{
			addressBar: osShortcut{Name: "l", Modifiers: []string{"cmd"}},
			openSelect: osShortcut{Name: "down", Modifiers: []string{"alt"}},
		}
	default:
		return osShortcuts{
			addressBar: osShortcut{Name: "l", Modifiers: []string{"ctrl"}},
			openSelect: osShortcut{Name: "down", Modifiers: []string{"alt"}},
		}
	}
}

// pressShortcutDefault presses one keystroke through the platform. Seam: tests
// swap it for a recorder and must never press a real key.
func (h *Host) pressShortcutDefault(s osShortcut) error {
	return h.dispatchPlatformAction("input.shortcut_by_name",
		map[string]any{"name": s.Name, "modifiers": s.Modifiers})
}

// runAsyncDefault runs f off the calling goroutine. Seam: tests run it inline
// so what it decided can be asserted without waiting.
func (h *Host) runAsyncDefault(f func()) { go f() }

// followupOpenSelect is the dispatch-result follow-up asking the plugin to
// open the picker of the <select> the extension just focused (voice on
// <select>). The extension can focus the element but cannot open
// it: showPicker() and synthetic key events both need a trusted user gesture.
const followupOpenSelect = "open_select"

// handleDispatchFollowup acts on a dispatch result's follow-up request. An
// unknown value is logged and ignored — the extension may be newer than this
// plugin. The keystroke itself runs off the HTTP handler: the on-the-wire
// dispatch must not hold up the extension's POST.
func (h *Host) handleDispatchFollowup(pluginID, followup, connID string) {
	switch followup {
	case "":
		return
	case followupOpenSelect:
	default:
		branchkit.Logf(pluginID, "dispatch-result followup=%q unknown — ignored", followup)
		return
	}
	corr := h.plugin.CurrentCorrelation()
	h.runAsync(func() {
		branchkit.RunWithCorrelation(corr, func() {
			h.pressOpenSelect(pluginID, connID)
		})
	})
}

// pressOpenSelect presses the open-picker keystroke — only while the browser
// that asked is the app in front and the connection that asked is the focused
// one, checked under mu immediately before the press. A keystroke goes to
// whatever app has focus, so a stale request (focus moved on while the POST
// was in flight) would type into another app or another browser. Returns
// whether it pressed.
func (h *Host) pressOpenSelect(pluginID, connID string) bool {
	h.mu.Lock()
	focused := h.state.BrowserFocused
	focusedConn := h.state.FocusedConnID
	h.mu.Unlock()
	switch {
	case !focused:
		branchkit.Logf(pluginID, "open_select dropped: no browser in front (conn=%q)", connID)
		return false
	case connID == "":
		branchkit.Logf(pluginID, "open_select dropped: request carries no conn_id")
		return false
	case connID != focusedConn:
		branchkit.Logf(pluginID, "open_select dropped: conn=%q is not the focused conn %q", connID, focusedConn)
		return false
	}
	key := shortcutsFor(runtime.GOOS).openSelect
	if err := h.pressShortcut(key); err != nil {
		branchkit.Logf(pluginID, "open_select: %v", err)
		return false
	}
	branchkit.Logf(pluginID, "open_select pressed %v+%s conn=%q", key.Modifiers, key.Name, connID)
	return true
}
