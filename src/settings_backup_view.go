package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// The Backup settings tab: the copy of the extension's settings this plugin
// keeps (settings_backup.go), shown so the owner can see that it exists, what
// it holds, and take it away as a file.
//
// READ-ONLY on purpose. Restoring belongs to the extension, which writes each
// setting back through its own validating helpers and asks first; an edit or a
// delete here would be a second writer to a copy whose whole value is that
// nothing touches it but the extension. The one action is a download, and it
// does not change the copy.
//
// Everything shown is untrusted: site patterns and selectors are text the user
// typed into the extension. It reaches the page only as templ-escaped text.

// backupBrowsers is the card order and each family's name.
var backupBrowsers = []struct{ key, label string }{
	{"chrome", "Chrome"},
	{"firefox", "Firefox"},
}

// BackupSettingsData is the Backup tab's whole view.
type BackupSettingsData struct {
	// Why the file could not be read, when it could not.
	Error string
	Slots []BackupSlotView
}

// BackupSlotView is one browser family's card.
type BackupSlotView struct {
	Browser string // "chrome", the download method's argument
	Label   string // "Chrome"
	Found   bool
	// Unreadable: a slot exists but its document is not the extension's
	// shape, so only the download is offered.
	Unreadable       bool
	SavedAt          string
	ExtensionVersion string
	Summary          string
	Rules            []BackupRuleView
	// The last download's outcome, until the next one.
	ExportStatus string
}

// BackupRuleView is one domain rule: the site pattern and what it does there.
type BackupRuleView struct {
	Pattern   string
	Off       bool
	BadgeSize string
	Entries   []BackupEntryView
}

// BackupEntryView is one entry of a domain rule, worded as the extension's
// options page words it (rule-ui.ts KIND_META, matcherSummary, nudgeSummary).
type BackupEntryView struct {
	Kind    string // "Exclude"
	Does    string // what the kind does, for the tooltip
	Matcher string
	Detail  string // nudge offset or reveal method
	Note    string // the user's own label for the entry
	Off     bool
}

// --- The extension's document ------------------------------------------------

// backupDocument is the part of the extension's SettingsBackup this tab reads.
type backupDocument struct {
	ExportedAt       string `json:"exportedAt"`
	ExtensionVersion string `json:"extensionVersion"`
	Settings         *struct {
		Sync  json.RawMessage `json:"sync"`
		Local json.RawMessage `json:"local"`
	} `json:"settings"`
}

// durableSetting mirrors one entry of the extension's DURABLE_SETTINGS
// registry (settings-backup.ts), as far as the summary needs it: which stored
// key it is, its label, and how to count its items. Kept in registry order.
type durableSetting struct {
	area   string
	key    string
	prefix bool
	label  string
	count  func(v json.RawMessage) int
}

var durableSettings = []durableSetting{
	{area: "sync", key: "domainRules", label: "domain rules", count: func(v json.RawMessage) int {
		var d struct {
			Rules []json.RawMessage `json:"rules"`
		}
		if json.Unmarshal(v, &d) != nil {
			return 0
		}
		return len(d.Rules)
	}},
	{area: "sync", key: "keyboardRules", label: "key rules", count: func(v json.RawMessage) int {
		var rules []json.RawMessage
		if json.Unmarshal(v, &rules) != nil {
			return 0
		}
		return len(rules)
	}},
	{area: "sync", key: "keymap", label: "keymap", count: func(v json.RawMessage) int {
		var m map[string]json.RawMessage
		if json.Unmarshal(v, &m) != nil {
			return 0
		}
		return len(m)
	}},
	{area: "sync", key: "keyTiming", label: "key timing"},
	{area: "sync", key: "bareKeyShortcuts", label: "single-key shortcuts"},
	{area: "sync", key: "badgeSettings", label: "badge appearance"},
	{area: "sync", key: "searchEngine", label: "search engine"},
	{area: "sync", key: "paletteOpenDefault", label: "palette pick"},
	{area: "sync", key: "voiceNewTabPalette", label: "new-tab palette"},
	{area: "sync", key: "hintVisibility", label: "badge persistence"},
	{area: "sync", key: "tabMarkersEnabled", label: "tab markers"},
	{area: "sync", key: "showGuideButton", label: "guide button"},
	{area: "local", key: "branchkit_references", label: "saved references", count: func(v json.RawMessage) int {
		var hosts map[string]json.RawMessage
		if json.Unmarshal(v, &hosts) != nil {
			return 0
		}
		n := 0
		for _, raw := range hosts {
			var host struct {
				References map[string]json.RawMessage `json:"references"`
			}
			if json.Unmarshal(raw, &host) == nil {
				n += len(host.References)
			}
		}
		return n
	}},
	{area: "local", key: "mark:global:", prefix: true, label: "global marks", count: func(json.RawMessage) int { return 1 }},
	{area: "local", key: "autoScrollStep", label: "auto-scroll pace"},
}

// durableFor is the extension's durableFor: the registry entry a stored key
// belongs to.
func durableFor(area, key string) *durableSetting {
	for i := range durableSettings {
		d := &durableSettings[i]
		if d.area != area {
			continue
		}
		if d.prefix {
			if strings.HasPrefix(key, d.key) && key != d.key {
				return d
			}
		} else if key == d.key {
			return d
		}
	}
	return nil
}

// objectInOrder decodes a JSON object keeping its key order, which Go's maps
// lose and the summary's order depends on. Not an object: no keys.
func objectInOrder(raw json.RawMessage) ([]string, map[string]json.RawMessage) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil
		}
		if _, dup := vals[key]; !dup {
			keys = append(keys, key)
		}
		vals[key] = v
	}
	return keys, vals
}

// summarizeBackup is the extension's summarizeBackup, word for word, so this
// tab and the extension's restore banner describe one copy the same way:
// counted entries first, then the rest by label.
func summarizeBackup(sync, local json.RawMessage) string {
	type tally struct {
		d *durableSetting
		n int
	}
	var counted []*tally
	var plain []string
	for _, area := range []struct {
		name string
		raw  json.RawMessage
	}{{"sync", sync}, {"local", local}} {
		keys, vals := objectInOrder(area.raw)
		for _, key := range keys {
			d := durableFor(area.name, key)
			if d == nil {
				continue
			}
			if d.count == nil {
				if !containsString(plain, d.label) {
					plain = append(plain, d.label)
				}
				continue
			}
			var t *tally
			for _, c := range counted {
				if c.d == d {
					t = c
				}
			}
			if t == nil {
				t = &tally{d: d}
				counted = append(counted, t)
			}
			t.n += d.count(vals[key])
		}
	}
	var parts []string
	for _, c := range counted {
		if c.n <= 0 {
			continue
		}
		switch {
		case c.d.key == "keymap":
			parts = append(parts, fmt.Sprintf("keymap (%d changed %s)", c.n, plural(c.n, "command", "commands")))
		case c.n == 1:
			parts = append(parts, fmt.Sprintf("1 %s", strings.TrimSuffix(c.d.label, "s")))
		default:
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.d.label))
		}
	}
	parts = append(parts, plain...)
	if len(parts) == 0 {
		return "no settings"
	}
	if len(parts) <= 5 {
		return strings.Join(parts, ", ")
	}
	more := len(parts) - 4
	return fmt.Sprintf("%s and %d more %s", strings.Join(parts[:4], ", "), more, plural(more, "setting", "settings"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- Domain rules ----------------------------------------------------------

// backupDomainRule is the extension's DomainRule (rules/domain-rules.ts), the
// fields this tab shows.
type backupDomainRule struct {
	Pattern     string            `json:"pattern"`
	Enabled     bool              `json:"enabled"`
	Entries     []json.RawMessage `json:"entries"`
	BadgeSizePx *float64          `json:"badgeSizePx"`
}

type backupRuleEntry struct {
	Kind    string `json:"kind"`
	Matcher struct {
		Type          string `json:"type"`
		Selector      string `json:"selector"`
		Value         string `json:"value"`
		CaseSensitive bool   `json:"caseSensitive"`
		Mode          string `json:"mode"`
		Name          string `json:"name"`
	} `json:"matcher"`
	Reveal string `json:"reveal"`
	Nudge  *struct {
		Dx float64 `json:"dx"`
		Dy float64 `json:"dy"`
	} `json:"nudge"`
	Label   string `json:"label"`
	Enabled *bool  `json:"enabled"`
}

// ruleKinds is the extension's KIND_META label for each kind, and what the
// kind does to a matching element.
var ruleKinds = map[string]struct{ label, does string }{
	"exclude": {"Exclude", "No badge on matching elements"},
	"include": {"Include", "A badge on matching elements that would not get one"},
	"reveal":  {"Reveal", "Shows matching elements the page keeps hidden"},
	"nudge":   {"Nudge", "Moves the badge on matching elements"},
}

func formatNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func signed(v float64) string {
	if v >= 0 {
		return "+" + formatNumber(v)
	}
	return formatNumber(v)
}

// matcherSummary is the extension's matcherSummary.
func (e *backupRuleEntry) matcherSummary() string {
	m := e.Matcher
	switch m.Type {
	case "css":
		return m.Selector
	case "text":
		ci := " (case-insensitive)"
		if m.CaseSensitive {
			ci = ""
		}
		if m.Mode == "contains" {
			return `text contains "` + m.Value + `"` + ci
		}
		return `text="` + m.Value + `"` + ci
	case "class":
		return "." + m.Name
	}
	return m.Type
}

func entryView(raw json.RawMessage) BackupEntryView {
	var e backupRuleEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return BackupEntryView{Kind: "?", Matcher: "(an entry this page cannot read)"}
	}
	v := BackupEntryView{Kind: e.Kind, Matcher: e.matcherSummary(), Note: e.Label}
	if k, ok := ruleKinds[e.Kind]; ok {
		v.Kind, v.Does = k.label, k.does
	}
	if e.Kind == "nudge" && e.Nudge != nil {
		v.Detail = "(" + signed(e.Nudge.Dx) + ", " + signed(e.Nudge.Dy) + ")"
	}
	if e.Kind == "reveal" && e.Reveal != "" {
		v.Detail = "by " + e.Reveal
	}
	v.Off = e.Enabled != nil && !*e.Enabled
	return v
}

// domainRuleViews reads the rule list out of the sync settings. A rule that
// does not decode is listed as unreadable rather than dropped, so the count
// beside the list stays the summary's count.
func domainRuleViews(sync json.RawMessage) []BackupRuleView {
	_, vals := objectInOrder(sync)
	raw, ok := vals["domainRules"]
	if !ok {
		return nil
	}
	var d struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil
	}
	out := make([]BackupRuleView, 0, len(d.Rules))
	for _, r := range d.Rules {
		var rule backupDomainRule
		if err := json.Unmarshal(r, &rule); err != nil {
			out = append(out, BackupRuleView{Pattern: "(a rule this page cannot read)"})
			continue
		}
		v := BackupRuleView{Pattern: rule.Pattern, Off: !rule.Enabled}
		if rule.BadgeSizePx != nil {
			v.BadgeSize = "Badge size " + formatNumber(*rule.BadgeSizePx) + " px"
		}
		for _, e := range rule.Entries {
			v.Entries = append(v.Entries, entryView(e))
		}
		out = append(out, v)
	}
	return out
}

// --- The view --------------------------------------------------------------

// buildBackupSettingsData turns the stored file into the tab's view. Pure, so
// the tests drive it without a data dir; formatSaved renders when a copy was
// saved (the platform's locale formatting in the plugin, a stub in tests).
func buildBackupSettingsData(file settingsBackupFile, loadErr error, status map[string]string, formatSaved func(time.Time) string) BackupSettingsData {
	var data BackupSettingsData
	if loadErr != nil {
		data.Error = "The backup file could not be read: " + loadErr.Error()
	}
	for _, b := range backupBrowsers {
		v := BackupSlotView{Browser: b.key, Label: b.label, ExportStatus: status[b.key]}
		slot, ok := file.Slots[b.key]
		if ok {
			v.Found = true
			v.SavedAt = formatSaved(time.UnixMilli(slot.SavedAt))
			var doc backupDocument
			if json.Unmarshal(slot.Backup, &doc) != nil || doc.Settings == nil {
				v.Unreadable = true
			} else {
				v.ExtensionVersion = doc.ExtensionVersion
				v.Summary = summarizeBackup(doc.Settings.Sync, doc.Settings.Local)
				v.Rules = domainRuleViews(doc.Settings.Sync)
			}
		}
		data.Slots = append(data.Slots, v)
	}
	return data
}

func (h *Host) renderBackupSettings(_ *branchkit.RenderSettingsRequest) (string, error) {
	file := settingsBackupFile{Slots: map[string]settingsBackupSlot{}}
	var loadErr error
	if path := settingsBackupPath(); path != "" {
		settingsBackupMu.Lock()
		file, loadErr = loadSettingsBackups(path)
		settingsBackupMu.Unlock()
	}
	return branchkit.RenderComponent(BrowserBackup(buildBackupSettingsData(file, loadErr, h.backupExportStatuses(), h.formatSavedAt)))
}

// formatSavedAt renders an instant in the user's zone and locale, by the
// platform (native.format_date): a hand-rolled layout reads right only to an
// English reader. Falls back to the RFC 3339 instant, never to an invented
// pattern.
func (h *Host) formatSavedAt(t time.Time) string {
	when := t.UTC().Format(time.RFC3339)
	res, err := h.plugin.NativeFormatDate(branchkit.NativeFormatDateRequest{When: when, Style: "date_time"})
	if err != nil || res.Value == "" {
		return when
	}
	return res.Value
}

// --- Download ------------------------------------------------------------------

// DownloadSettingsBackupRequest is the Backup tab's download button.
type DownloadSettingsBackupRequest struct {
	Browser string `json:"browser"`
}

// settingsBackupDownload is the file the download hands over: the stored
// document as the extension's own Download button writes it (two-space
// indent, key order and string escapes untouched), and its name. The name
// takes the day from the document's exportedAt, as the extension's
// backupFileName does, with the family added so a Chrome and a Firefox copy
// saved the same day do not overwrite each other in Downloads.
func settingsBackupDownload(browser string, slot settingsBackupSlot) ([]byte, string, error) {
	var body bytes.Buffer
	if err := json.Indent(&body, slot.Backup, "", "  "); err != nil {
		return nil, "", err
	}
	var doc backupDocument
	_ = json.Unmarshal(slot.Backup, &doc)
	day := time.UnixMilli(slot.SavedAt).UTC().Format("2006-01-02")
	// Only a well-formed date: exportedAt is the extension's text, and it is
	// about to become part of a file name.
	if len(doc.ExportedAt) >= 10 {
		if _, err := time.Parse("2006-01-02", doc.ExportedAt[:10]); err == nil {
			day = doc.ExportedAt[:10]
		}
	}
	return body.Bytes(), "branchkit-settings-" + browser + "-" + day + ".json", nil
}

// handleDownloadSettingsBackup hands one family's copy to the user in
// Downloads. The settings method proxy is the only way in: the button posts to
// it, it is the settings UI's authenticated route, and this method is not on
// the extension bridge. The file is staged in this plugin's data dir and the
// platform's plugin.data.export copies it out — the sandbox gives a plugin no
// way to write to Downloads itself, deliberately. The outcome is shown on the
// card; a settings method returns nothing.
func (h *Host) handleDownloadSettingsBackup(req *DownloadSettingsBackupRequest) error {
	if !validBackupBrowser(req.Browser) {
		return fmt.Errorf("browser must be chrome or firefox")
	}
	h.setBackupExportStatus(req.Browser, h.exportSettingsBackup(req.Browser))
	h.refreshSettings()
	return nil
}

// settingsBackupStageName is the staged copy's name inside the data dir; it is
// removed once the export has copied it out.
func settingsBackupStageName(browser string) string {
	return "settings-backup-download-" + browser + ".json"
}

// exportSettingsBackup does the download and returns the line the card shows.
func (h *Host) exportSettingsBackup(browser string) string {
	path := settingsBackupPath()
	if path == "" {
		return "Download failed: this plugin has no data directory."
	}
	settingsBackupMu.Lock()
	file, err := loadSettingsBackups(path)
	settingsBackupMu.Unlock()
	if err != nil {
		return "Download failed: " + err.Error()
	}
	slot, ok := file.Slots[browser]
	if !ok {
		return "Download failed: there is no backup to download yet."
	}
	body, name, err := settingsBackupDownload(browser, slot)
	if err != nil {
		return "Download failed: the stored copy is not JSON (" + err.Error() + ")."
	}
	stage := settingsBackupStageName(browser)
	stagePath := filepath.Join(filepath.Dir(path), stage)
	if err := writeFileAtomic(stagePath, body); err != nil {
		return "Download failed: " + err.Error()
	}
	// In Downloads or not, the staged copy has served its purpose.
	defer func() { _ = os.Remove(stagePath) }()
	saved, err := h.exportDataFile(stage, name)
	if err != nil {
		return "Download failed: " + err.Error()
	}
	return "Saved to " + saved
}

// exportDataFileDefault is plugin.data.export: copy one file of this plugin's
// data dir to Downloads under `name`, and reveal it.
func (h *Host) exportDataFileDefault(rel, name string) (string, error) {
	resp, err := h.plugin.PluginDataExport(branchkit.PluginDataExportRequest{Path: rel, Filename: &name})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", errors.New("no reply")
	}
	return resp.Path, nil
}

// refreshSettingsDefault re-renders this plugin's open settings tabs.
func (h *Host) refreshSettingsDefault() {
	if err := h.plugin.SettingsRefresh(); err != nil && !errors.Is(err, branchkit.ErrDetached) {
		branchkit.Logf("browser", "settings refresh: %v", err)
	}
}

func (h *Host) setBackupExportStatus(browser, status string) {
	h.backupViewMu.Lock()
	defer h.backupViewMu.Unlock()
	if h.backupExportStatus == nil {
		h.backupExportStatus = map[string]string{}
	}
	h.backupExportStatus[browser] = status
}

func (h *Host) backupExportStatuses() map[string]string {
	h.backupViewMu.Lock()
	defer h.backupViewMu.Unlock()
	out := make(map[string]string, len(h.backupExportStatus))
	for k, v := range h.backupExportStatus {
		out[k] = v
	}
	return out
}
