package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// The focus pin: a dev-build-only way to make one extension connection the
// focused browser whatever app the OS has in front.
//
// Why it exists. Commands reach only the browser the OS reports as frontmost
// (recomputeFocusedSourceLocked matches each connection's socket-asserted
// bundle against app.focused). An end-to-end check against the running app
// therefore needed a browser window in FRONT — which took focus from whatever
// the person was doing. With a pin, a headless test browser is the focused
// source: hints tag, plugin.browser.active, reprojection, palette, restricted
// pages and action dispatch all run exactly as for a real frontmost browser,
// while the person keeps typing in their own app.
//
// What it must never do. A pinned browser is NOT the frontmost app, so any
// real keystroke or window operation would land in the person's app — Cmd+L
// into their terminal, a desk switch under their editor. While a pin is up,
// every platform dispatch this plugin makes (dispatchPlatformAction: the
// open_select and address-bar keystrokes, the tab-desk window moves) is
// RECORDED and not sent, and a problem is published but its window is not
// shown. GET /dev/focus-pin returns those records, so a check can assert what
// would have happened.
//
// The pin clears when its connection disconnects, and on unpin; both restore
// focus derived from the OS.

// devAppSupportName is the app-support folder of a dev build. The platform
// names the folder once, from its COMPILED-IN build type
// (installed_app_support_dir(is_dev_build()) in core/src/lib.rs), and
// hands it to every plugin it launches as BRANCHKIT_APP_SUPPORT, overwriting
// whatever the environment held. So the folder's name is the build type, not
// a setting: a person running a release build cannot turn this on short of
// building the app themselves, and no flag, env var or settings value reaches
// it. (Someone launching the plugin binary by hand controls its environment,
// but they already own the process.)
const devAppSupportName = "BranchKitDev"

// devBuild reports whether the platform that launched this plugin is a dev
// build. Read when the listener's routes are built.
func devBuild() bool {
	dir := os.Getenv("BRANCHKIT_APP_SUPPORT")
	return dir != "" && filepath.Base(dir) == devAppSupportName
}

// pinEffectCap bounds the suppressed-effect record.
const pinEffectCap = 50

// Kinds of suppressed effect.
const (
	pinEffectDispatch    = "platform_dispatch" // a dispatchPlatformAction call
	pinEffectProblemShow = "problem_show"      // a problem window that would have shown
)

// pinnedEffect is one real OS effect the pin kept from happening.
type pinnedEffect = wire.PinnedEffect

// focusPin holds the pin and its record. Its own lock, taken after h.mu when
// both are held (recompute reads the pin under h.mu) and on its own by the
// effect gates, which may run under other locks (problemSurface.mu) but
// never under h.mu. It takes no other lock.
type focusPin struct {
	mu      sync.Mutex
	connID  string
	since   time.Time
	effects []pinnedEffect
}

// current is the pinned connection, or "".
func (p *focusPin) current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connID
}

// set pins connID ("" unpins) and returns the previous pin.
func (p *focusPin) set(connID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.connID
	p.connID = connID
	if connID != prev {
		p.since = time.Now()
	}
	return prev
}

// clearIf unpins when connID is the pinned connection; reports whether it did.
func (p *focusPin) clearIf(connID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if connID == "" || p.connID != connID {
		return false
	}
	p.connID = ""
	p.since = time.Now()
	return true
}

// suppress records an effect and reports true when a pin is up; the caller
// then skips the effect. False, and nothing recorded, without a pin.
func (p *focusPin) suppress(kind, action string, params map[string]any) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.connID == "" {
		return false
	}
	p.effects = append(p.effects, pinnedEffect{
		Kind: kind, Action: action, Params: params, PinnedConn: p.connID, At: time.Now(),
	})
	if n := len(p.effects); n > pinEffectCap {
		p.effects = append([]pinnedEffect(nil), p.effects[n-pinEffectCap:]...)
	}
	return true
}

// snapshot copies the pin and the record.
func (p *focusPin) snapshot() (connID string, since time.Time, effects []pinnedEffect) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connID, p.since, append([]pinnedEffect{}, p.effects...)
}

// focusPinConn is one live connection, so a caller can find the conn_id to pin.
type focusPinConn = wire.FocusPinConn

// focusPinState is the answer to GET and POST /dev/focus-pin.
type focusPinState = wire.FocusPinState

func (h *Host) focusPinStateSnapshot() focusPinState {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn, since, effects := h.pin.snapshot()
	st := focusPinState{
		Pinned:            conn != "",
		ConnID:            conn,
		FocusedConnID:     h.state.FocusedConnID,
		BrowserFocused:    h.state.BrowserFocused,
		OSFrontmostBundle: h.state.OSFrontmostBundle,
		Connections:       []focusPinConn{},
		Suppressed:        effects,
	}
	if conn != "" {
		st.Since = &since
	}
	for id, c := range h.state.Conns {
		if c.Client == "" {
			continue
		}
		st.Connections = append(st.Connections, focusPinConn{
			ConnID: id, Bundle: c.AssertedBundle, ActiveTab: c.ActiveTab,
		})
	}
	sort.Slice(st.Connections, func(i, j int) bool { return st.Connections[i].ConnID < st.Connections[j].ConnID })
	return st
}

// handleFocusPinGet answers GET /dev/focus-pin: the pin, the live
// connections, and the effects the pin suppressed (newest last).
func (h *Host) handleFocusPinGet(_ *http.Request, _ *wire.Empty) (wire.FocusPinState, error) {
	return h.focusPinStateSnapshot(), nil
}

// handleFocusPinPost answers POST /dev/focus-pin. {"conn_id": "<id>"} pins a
// live connection; {"conn_id": ""} or {"off": true} unpins. Either way focus
// is recomputed and the browser tags follow, as for any other focus input.
func (h *Host) handleFocusPinPost(_ *http.Request, body *wire.FocusPinRequest) (wire.FocusPinState, error) {
	target := body.ConnID
	if body.Off {
		target = ""
	}

	var armBundle, armVis string
	h.mu.Lock()
	if target != "" {
		if h.state.clientOf(target) == "" {
			h.mu.Unlock()
			return wire.FocusPinState{}, statusError(http.StatusConflict, fmt.Sprintf("no live connection %q", target))
		}
	}
	prev := h.pin.set(target)
	if target == "" {
		h.restoreOSFrontmostLocked()
	}
	wasFocused := h.state.BrowserFocused
	isBrowser, needsRescan := h.recomputeFocusedSourceLocked("focus_pin")
	if isBrowser && !wasFocused {
		armBundle = h.state.FocusedBrowserBundleID
		armVis = h.state.HintVisibility
	}
	releaseBundle := h.state.FocusedBundleID
	focusedConn := h.state.FocusedConnID
	h.mu.Unlock()

	if target != "" {
		branchkit.Logf(h.state.PluginID, "[FOCUS-PIN] pinned conn=%q (was %q) focused_conn=%q", target, prev, focusedConn)
	} else {
		branchkit.Logf(h.state.PluginID, "[FOCUS-PIN] unpinned (was %q); focus follows the OS again, focused_conn=%q", prev, focusedConn)
	}
	h.applyFocusOutcome(isBrowser, needsRescan, armBundle, armVis, releaseBundle, "focus_pin")
	return h.focusPinStateSnapshot(), nil
}

// restoreOSFrontmostLocked puts the frontmost bundle back to what the OS last
// reported. While pinned, the pinned browser's own focus claims may have set
// FocusedBundleID to its bundle — a claim about a window the OS never put in
// front — and leaving that would keep it focused after the pin is gone. Must
// hold mu.
func (h *Host) restoreOSFrontmostLocked() {
	if h.state.OSFrontmostBundle != "" {
		h.state.FocusedBundleID = h.state.OSFrontmostBundle
	}
}

// applyFocusOutcome runs the tag transitions a recompute decided, as the
// other focus inputs do: the rescan, the arm on a focus-gain edge, the
// derived hints tag whenever a browser is focused (handleActiveTabPost's
// order), the release when none is. Must NOT hold mu.
func (h *Host) applyFocusOutcome(isBrowser, needsRescan bool, armBundle, armVis, releaseBundle, reason string) {
	if needsRescan {
		h.pushRescanToClients()
	}
	if armBundle != "" {
		h.armBrowserFocusUnlocked(h.state.PluginID, armBundle, armVis)
	}
	if isBrowser {
		h.applyDerivedHintsTag(reason)
	} else {
		h.releaseBrowserFocusUnlocked(h.state.PluginID, releaseBundle, reason)
	}
}
