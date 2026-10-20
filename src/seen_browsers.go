package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Which bundles have held a live extension connection, remembered ACROSS
// restarts.
//
// The disconnect tripwire can only fire for a bundle in `SeenBundles`, because
// that set is what separates "standalone by choice" from "the extension
// vanished" — the distinction the extension side cannot draw and this side can.
//
// In-memory that set starts empty every launch, which left the worst version
// of the failure silent: start BranchKit with an extension already wedged and
// no bundle is ever "seen", so nothing arms, and the Plugins page reads Active
// while the plugin serves ~10 of its ~176 phrases. That is the shape of the
// 2026-07-02 incident itself.
//
// Persisting the set fixes cold start without weakening the distinction: a
// browser that never connected is still never in the file, so a user who never
// installed the extension is never nudged.
//
// Entries carry a timestamp and expire. "It worked last week and does not now"
// is actionable; "it worked once, eight months ago" is not, and warning about
// it forever is the false alarm this design is otherwise careful to avoid — it
// is also what a deliberate uninstall looks like from here.

// seenBrowsersFile is the plugin's own data namespace — writable, sandboxed to
// this plugin, and not replaced on update. PluginDir is not a substitute: it
// lives inside the signed app bundle for a bundled plugin.
func seenBrowsersFile() string {
	dir := branchkit.PluginDataDir()
	if dir == "" {
		return "" // not launched by the platform; nothing to persist to
	}
	return filepath.Join(dir, "seen-browsers.json")
}

// seenBrowserTTL is how long a browser stays "expected to be connected" after
// its last live connection. Refreshed on every connect, so a browser in daily
// use never expires.
const seenBrowserTTL = 14 * 24 * time.Hour

// loadSeenBrowsers restores the set, dropping entries past the TTL. Any error
// yields an empty set: this is a convenience that makes one failure mode
// detectable, and it must never be the reason the plugin fails to start.
func loadSeenBrowsers() map[string]bool {
	out := make(map[string]bool)
	path := seenBrowsersFile()
	if path == "" {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var stamps map[string]int64
	if err := json.Unmarshal(raw, &stamps); err != nil {
		return out
	}
	cutoff := time.Now().Add(-seenBrowserTTL).UnixMilli()
	for bundle, ms := range stamps {
		if ms >= cutoff {
			out[bundle] = true
		}
	}
	return out
}

// saveSeenBrowser records `bundle` as connected now. Called on connect, which
// is a human-rate event — no debounce needed.
//
// Read-modify-write rather than dumping the in-memory set, so a bundle whose
// entry aged out of memory this session keeps its stored timestamp instead of
// being silently forgotten.
func saveSeenBrowser(bundle string) {
	path := seenBrowsersFile()
	if path == "" || bundle == "" {
		return
	}
	stamps := map[string]int64{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &stamps)
	}
	stamps[bundle] = time.Now().UnixMilli()
	// Drop entries already past the TTL so the file cannot grow without bound
	// across years of browsers tried once.
	cutoff := time.Now().Add(-seenBrowserTTL).UnixMilli()
	for b, ms := range stamps {
		if ms < cutoff {
			delete(stamps, b)
		}
	}
	data, err := json.Marshal(stamps)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}
