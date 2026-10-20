package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Stale-disconnect tripwire. The 2026-07-02 incident: an
// extension sat dead for 20+ minutes while every focus event logged
// `is_browser=false` and nothing surfaced it. This side of the wire is the
// one that CAN tell "standalone by choice" from "extension vanished":
// a bundle that held a live connection this session but has none now is a
// vanished extension, not a browser that never had one.
//
// Focus-driven, not polled: the nudge arms when the user focuses the broken
// browser — exactly the moment a "reload it" hint is actionable — and costs
// nothing at steady state. One nudge per disconnect episode; a reconnect
// clears the episode so the next genuine disconnect nudges again.

// disconnectNudgeGrace is how long a focused-but-connectionless browser gets
// before the nudge fires. Generous enough for the extension's SSE retry
// ladder and the 30s-alarm respawn to win a routine reconnect race (plugin
// restarts land well under this; the wedge sat for 20+ minutes).
const disconnectNudgeGrace = 10 * time.Second

// coldStartNudgeAllowance is how long after plugin startup the frontmost
// probe waits before it may arm the nudge. At boot a HEALTHY extension may
// legitimately take up to its 30s service-worker alarm to reconnect, so the
// per-focus grace above is not enough on its own — arming immediately would
// false-alarm on every restart with a browser frontmost. If the extension
// were coming back by itself, it will have done so within this window.
const coldStartNudgeAllowance = 30 * time.Second

// emitDisconnectNudge is the side-effect half (Warn log + bus event) — a var
// so tests can capture invocations without an RPC connection, same seam
// pattern as syncPerPrefix / replaceTabsCollection.
func (h *Host) emitDisconnectNudgeDefault(pluginID, bundle string) {
	// Warn-level so the plugin-logging-v2 threshold cross-posts it into
	// branchkit.log, right next to the is_browser=false lines it explains.
	h.plugin.LogAt("warn", "FOCUS", map[string]any{
		"msg":       "extension disconnected — its browser is focused but holds no live connection",
		"bundle_id": bundle,
		"grace_s":   int(disconnectNudgeGrace / time.Second),
		"remedy":    extensionRemedy(bundle) + ", or check its service worker",
	})
	branchkit.Logf(pluginID,
		"[FOCUS] extension disconnected bundle=%q for >%s — %s",
		bundle, disconnectNudgeGrace, extensionRemedy(bundle))
	h.emitEvent("browser.extension_disconnected", map[string]any{
		"bundle_id": bundle,
		"grace_s":   int(disconnectNudgeGrace / time.Second),
	})
}

// reportHealth is a var so tests can capture it without an RPC connection,
// matching the emitDisconnectNudge seam above.
func (h *Host) reportHealthDefault(degraded bool, reason string) {
	args := map[string]any{"degraded": degraded}
	if degraded {
		args["reason"] = reason
	}
	h.plugin.Notify(branchkit.MethodPluginReportHealth, args)
}

// browserLabel turns a bundle id into something worth showing a user —
// "com.google.Chrome" is the plugin's vocabulary, not theirs.
func browserLabel(bundle string) string {
	if i := strings.LastIndex(bundle, "."); i >= 0 && i+1 < len(bundle) {
		return bundle[i+1:]
	}
	return bundle
}

// extensionRemedy is the fix for a vanished extension, in the words of the
// browser it vanished from. Chrome's advice sent a Firefox user to a page
// Firefox does not have, and "reload" is the wrong verb there
// anyway: an extension loaded as a temporary add-on is not disabled after a
// Firefox restart, it is GONE, and about:debugging is where it is loaded
// again. The Chromium family keeps its extension across restarts, so reload
// is right, each at its own page (edge:// and brave:// open the same list
// chrome:// would, under the name the person sees). Matched on the lowered
// id so a non-macOS bundle ("firefox", "org.mozilla.firefox" as a flatpak,
// "msedge.exe") reads the same as the macOS one.
func extensionRemedy(bundle string) string {
	b := strings.ToLower(bundle)
	switch {
	case strings.Contains(b, "mozilla") || strings.Contains(b, "firefox"):
		return "load it again from about:debugging"
	case strings.Contains(b, "edge"):
		return "reload it at edge://extensions"
	case strings.Contains(b, "brave"):
		return "reload it at brave://extensions"
	}
	return "reload it at chrome://extensions"
}

// armDisconnectNudgeLocked arms the grace timer for a frontmost bundle that
// previously held a connection but has none. No-ops for bundles that never
// connected (ordinary non-browser apps), for live bundles, and within an
// already-nudged episode. Must be called under `mu`.
func (h *Host) armDisconnectNudgeLocked(bundle string) {
	if bundle == "" || !h.state.SeenBundles[bundle] {
		return
	}
	if h.connForAssertedBundleLocked(bundle) != "" {
		return
	}
	if h.state.DisconnectNudged[bundle] {
		return
	}
	if h.state.DisconnectNudgeTimer != nil {
		if h.state.DisconnectNudgeBundle == bundle {
			return // already pending for this bundle
		}
		h.state.DisconnectNudgeTimer.Stop()
	}
	h.state.DisconnectNudgeBundle = bundle
	h.state.DisconnectNudgeTimer = time.AfterFunc(disconnectNudgeGrace, func() {
		h.fireDisconnectNudge(bundle)
	})
}

// fireDisconnectNudge re-checks under the lock (the extension may have
// reconnected during the grace window) and emits once.
func (h *Host) fireDisconnectNudge(bundle string) {
	h.mu.Lock()
	// Tear down only our OWN bookkeeping. A timer stopped too late can run
	// after a newer one was armed for another bundle; clearing that one's
	// slot here would let a duplicate arm alongside it.
	if h.state.DisconnectNudgeBundle == bundle {
		h.state.DisconnectNudgeTimer = nil
		h.state.DisconnectNudgeBundle = ""
	}
	// The nudged latch is part of the re-check, not just the arm guard:
	// two timers for one bundle (the stopped-too-late race above) must
	// still emit once.
	stillDead := h.state.SeenBundles[bundle] &&
		h.connForAssertedBundleLocked(bundle) == "" &&
		!h.state.DisconnectNudged[bundle]
	if stillDead {
		h.state.DisconnectNudged[bundle] = true
	}
	pluginID := h.state.PluginID
	h.mu.Unlock()

	if stillDead {
		h.emitDisconnectNudge(pluginID, bundle)
		// Tell the PLATFORM, not just the log. Without this the plugin reads
		// "Active" on the Plugins page while serving ~10 of its ~176 phrases:
		// the process is alive and answering, which is all the platform can
		// see from outside.
		h.reportHealth(true, fmt.Sprintf(
			"%s — extension disconnected; %s",
			browserLabel(bundle), extensionRemedy(bundle)))
	}
}

// listRunningBundles is the OS-truth half of the quit reaper (a var so tests
// can supply the running set without an RPC connection, same seam pattern as
// reportHealth above). Returns the bundle ids of every running app.
func (h *Host) listRunningBundlesDefault() (map[string]bool, error) {
	apps, err := h.plugin.NativeRunningApps()
	if err != nil {
		return nil, err
	}
	running := make(map[string]bool, len(apps))
	for _, a := range apps {
		if a.BundleID != nil && *a.BundleID != "" {
			running[*a.BundleID] = true
		}
	}
	return running, nil
}

// reapQuitEpisodes closes disconnect episodes whose browser is no longer
// running. A quit browser is "standalone by choice" — the very state this
// tripwire exists to NOT warn about — so a standing "reload the extension"
// complaint about it is a false alarm the user cannot act on. Reconnect
// closes an episode via noteBundleConnectedLocked; quit closes it here.
//
// Called from the focus handler (quitting an app always moves focus, so a
// focus event arrives at exactly the right moment) and only while an episode
// or grace timer is open, so the running-apps RPC costs nothing at steady
// state. Not polled: same doctrine as the rest of this file.
func (h *Host) reapQuitEpisodes() {
	running, err := h.listRunningBundles()
	if err != nil {
		return // OS truth unavailable; keep the episode rather than guess
	}
	h.mu.Lock()
	reaped := false
	for b := range h.state.DisconnectNudged {
		if !running[b] {
			delete(h.state.DisconnectNudged, b)
			reaped = true
		}
	}
	// A grace timer pending for a quit browser must not fire: the user
	// closed it, and a nudge about a browser that is gone answers nothing.
	if h.state.DisconnectNudgeTimer != nil && !running[h.state.DisconnectNudgeBundle] {
		h.state.DisconnectNudgeTimer.Stop()
		h.state.DisconnectNudgeTimer = nil
		h.state.DisconnectNudgeBundle = ""
	}
	// Same plugin-wide rule as reconnect: clear only when no nudged bundle
	// remains — quitting Chrome must not announce health while Firefox is
	// still broken.
	clearReport := reaped && len(h.state.DisconnectNudged) == 0
	h.mu.Unlock()
	if clearReport {
		h.reportHealth(false, "")
	}
}

// noteBundleConnectedLocked records a live assertion for `bundle` and closes
// any disconnect episode for it: the nudged flag resets (a future disconnect
// is a new episode) and a pending grace timer for it is cancelled. Must be
// called under `mu`.
func (h *Host) noteBundleConnectedLocked(bundle string) {
	if bundle == "" {
		return
	}
	h.state.SeenBundles[bundle] = true
	// Remember across restarts, and refresh the TTL — a browser in regular
	// use never ages out of "expected to be connected".
	saveSeenBrowser(bundle)
	// Closing a nudged episode may clear the platform health report, so the
	// Plugins page stops saying degraded once the extension is back — a
	// warning the user cannot clear is worse than none.
	//
	// But the report is PLUGIN-wide while an episode is per-bundle: with
	// Chrome and Firefox both dead, reconnecting Chrome must not announce
	// that the plugin is fine while Firefox is still broken. Clear only when
	// no nudged bundle remains — a plugin cannot flatter itself, and that
	// includes by omission.
	wasNudged := h.state.DisconnectNudged[bundle]
	delete(h.state.DisconnectNudged, bundle)
	if wasNudged && len(h.state.DisconnectNudged) == 0 {
		defer h.reportHealth(false, "")
	}
	if h.state.DisconnectNudgeTimer != nil && h.state.DisconnectNudgeBundle == bundle {
		h.state.DisconnectNudgeTimer.Stop()
		h.state.DisconnectNudgeTimer = nil
		h.state.DisconnectNudgeBundle = ""
	}
}
