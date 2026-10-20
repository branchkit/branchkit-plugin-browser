package main

import (
	"github.com/branchkit/plugin-sdk-go"
)

// BrowserConfig rides the settings preset: plugin.json declares the fields
// with defaults, the platform materializes the composed view, this plugin
// only reads it. The mirror's OnChange
// re-broadcasts the badge mode to connected extensions — so an edit from the
// GENERIC settings form reaches the extension exactly like one from the
// browser tab.
type BrowserConfig struct {
	HintDisplay string `json:"hint_display"` // "letter", "word", "expand"
}

const configCollection = "plugin.browser.config"

// initConfig wires the typed mirror. Must run before plugin.Run(). The
// OnChange callback needs `state`, so main wires it after state exists.
func (h *Host) initConfig(p *branchkit.Plugin, onChange func(BrowserConfig)) {
	h.configMirror = branchkit.Settings[BrowserConfig](p, configCollection)
	h.configMirror.OnChange(onChange)
}

// DefaultBrowserConfig mirrors the manifest defaults — the pre-Ready
// fallback only; plugin.json is the authoritative copy.
func DefaultBrowserConfig() BrowserConfig {
	return BrowserConfig{HintDisplay: "letter"}
}

func (h *Host) LoadBrowserConfig() BrowserConfig {
	if h.configMirror != nil && h.configMirror.Ready() {
		return h.configMirror.Get()
	}
	return DefaultBrowserConfig()
}

// setUserConfigField relays one user gesture into the user band (plugins
// never write settings; writers: platform_only). SetUser refreshes the
// mirror in the same operation, so re-renders and OnChange observers see
// the write immediately.
func (h *Host) setUserConfigField(key string, value any) error {
	if h.configMirror == nil {
		return nil
	}
	return h.configMirror.SetUser(key, value)
}
