package main

import (
	_ "embed"
	"fmt"
	"net/http"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

//go:embed settings.css
var browserCSS string

// renderBrowserSettings draws the one tab. The SDK's render hook has
// already refreshed the config mirror, so LoadBrowserConfig is current.
func (h *Host) renderBrowserSettings(_ *branchkit.RenderSettingsRequest) (string, error) {
	conf := h.LoadBrowserConfig()
	data := BrowserSettingsData{
		IsLetter: conf.HintDisplay == "letter",
		IsWord:   conf.HintDisplay == "word",
		IsExpand: conf.HintDisplay == "expand",
	}
	if words := h.snapshotCodewords(); len(words) >= 2 {
		data.WordA, data.WordB = words[0], words[1]
	}
	return branchkit.RenderComponent(BrowserSettings(data))
}

// handleHintDisplayPost is the extension popup's relay onto the same user-band
// write the settings tab performs. Bridge endpoint rather than a settings
// method because the caller is the extension, not the settings UI; both land
// in setUserConfigField, so both propagate through the mirror's OnChange.
func (h *Host) handleHintDisplayPost(_ *http.Request, req *SetHintDisplayRequest) (wire.Ack, error) {
	if !validHintDisplay(req.HintDisplay) {
		return wire.Ack{}, badRequest("invalid hint_display value")
	}
	if err := h.setUserConfigField("hint_display", req.HintDisplay); err != nil {
		branchkit.Logf("browser", "hint_display relay error: %v", err)
		return wire.Ack{}, err
	}
	return ack()
}

// validHintDisplay is the one gate both write paths (settings method, bridge
// relay) share, so the extension can never store a mode the tab cannot render.
func validHintDisplay(v string) bool {
	switch v {
	case "letter", "word", "expand":
		return true
	}
	return false
}

// SetHintDisplayRequest is both write paths' body (wire.SetHintDisplayRequest).
type SetHintDisplayRequest = wire.SetHintDisplayRequest

func (h *Host) handleSetHintDisplay(req *SetHintDisplayRequest) error {
	if !validHintDisplay(req.HintDisplay) {
		return fmt.Errorf("invalid hint_display value")
	}

	// Relay into the user band; the mirror's OnChange (wired in main) owns
	// the state update + extension broadcast, so this path and the generic
	// settings form converge on one propagation route. The tab redraws
	// through the re-render stream — the proxy answers 204 and discards
	// whatever a method returns.
	if err := h.setUserConfigField("hint_display", req.HintDisplay); err != nil {
		return fmt.Errorf("config relay: %w", err)
	}
	return nil
}
