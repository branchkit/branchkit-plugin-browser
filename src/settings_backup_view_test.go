package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// everythingSync / everythingLocal are the extension's own summary fixture
// (settings-backup.test.ts seedEverything), as exportBackup writes it: only
// the durable keys, in registry order.
const everythingSync = `{
	"domainRules": {"rules": [
		{"id": "r1", "pattern": "*.quickbase.com", "enabled": true, "entries": [{"id": "e1", "kind": "exclude", "matcher": {"type": "css", "selector": ".gear"}}]},
		{"id": "r2", "pattern": "mail.google.com", "enabled": false, "entries": [], "badgeSizePx": 16}
	]},
	"keyboardRules": [{"pattern": "mail.google.com", "passKeys": "jk"}],
	"keymap": {"close_tab": [{"keys": "KeyQ"}]},
	"keyTiming": {"sequenceTimeoutMs": 5000, "bounceMs": 100},
	"bareKeyShortcuts": "off",
	"badgeSettings": {"scale": 1.1, "highVisibility": true},
	"searchEngine": "https://duckduckgo.com/?q=%s",
	"paletteOpenDefault": "here",
	"voiceNewTabPalette": false,
	"hintVisibility": "manual",
	"tabMarkersEnabled": false,
	"showGuideButton": false
}`

const everythingLocal = `{
	"branchkit_references": {"a.com": {"references": {"home": {"selector": "#home", "tag": "a", "createdAt": 1, "lastUsedAt": 1}}, "marks": {}}},
	"mark:global:a": {"url": "https://a.com/", "scrollX": 0, "scrollY": 10, "hash": ""},
	"autoScrollStep": 6
}`

func fullBackupDoc(browser, sync, local string) json.RawMessage {
	return json.RawMessage(`{"schemaVersion":1,"exportedAt":"2026-10-04T12:00:00.000Z","extensionVersion":"0.4.2","browser":"` + browser +
		`","settings":{"sync":` + sync + `,"local":` + local + `}}`)
}

func renderBackupHTML(t *testing.T, d BackupSettingsData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := BrowserBackup(d).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// The summary is the extension's summarizeBackup, case for case — the same
// copy must read the same here and on the extension's restore banner.
func TestSummarizeBackupMatchesTheExtension(t *testing.T) {
	cases := []struct{ name, sync, local, want string }{
		{"everything", everythingSync, everythingLocal, "2 domain rules, 1 key rule, keymap (1 changed command), 1 saved reference and 11 more settings"},
		{"rules only", `{"domainRules":{"rules":[{"pattern":"a"},{"pattern":"b"}]}}`, `{}`, "2 domain rules"},
		{"empty", `{}`, `{}`, "no settings"},
		{"one rule singular", `{"domainRules":{"rules":[{"pattern":"a"}]}}`, `{}`, "1 domain rule"},
		{"zero counts drop out", `{"domainRules":{"rules":[]},"keymap":{}}`, `{}`, "no settings"},
		{"plain labels in document order", `{"searchEngine":"x","keymap":{"a":1,"b":2}}`, `{"autoScrollStep":3}`, "keymap (2 changed commands), search engine, auto-scroll pace"},
		{"marks sum across keys", `{}`, `{"mark:global:a":{},"mark:global:b":{},"mark:global:":{}}`, "2 global marks"},
		{"unknown keys ignored", `{"badgeDisplayMode":"word"}`, `{"alphabet":["a"]}`, "no settings"},
		{"wrong shapes count zero", `{"domainRules":[1,2],"keyboardRules":{"a":1}}`, `{"branchkit_references":{"a.com":{"references":[1]}}}`, "no settings"},
		{"exactly five", `{"domainRules":{"rules":[{}]},"keyboardRules":[{}],"keyTiming":{},"searchEngine":"x","bareKeyShortcuts":"on"}`, `{}`, "1 domain rule, 1 key rule, key timing, search engine, single-key shortcuts"},
		{"six names one more", `{"domainRules":{"rules":[{}]},"keyboardRules":[{}],"keyTiming":{},"searchEngine":"x","bareKeyShortcuts":"on","tabMarkersEnabled":true}`, `{}`, "1 domain rule, 1 key rule, key timing, search engine and 2 more settings"},
	}
	for _, c := range cases {
		if got := summarizeBackup(json.RawMessage(c.sync), json.RawMessage(c.local)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// rfc3339 stands in for the platform's locale formatting (native.format_date),
// which the tab calls through the host; the view only places what it returns.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Both families get a card; one with a copy shows when, which extension, what
// it holds and its rules, and offers the download for that family.
func TestBackupTabShowsBothSlots(t *testing.T) {
	saved := time.Date(2026, 10, 4, 16, 38, 0, 0, time.UTC)
	file := settingsBackupFile{Slots: map[string]settingsBackupSlot{
		"chrome":  {SavedAt: saved.UnixMilli(), Backup: fullBackupDoc("chrome", everythingSync, everythingLocal)},
		"firefox": {SavedAt: saved.Add(-24 * time.Hour).UnixMilli(), Backup: fullBackupDoc("firefox", `{"searchEngine":"x"}`, `{}`)},
	}}
	data := buildBackupSettingsData(file, nil, nil, rfc3339)
	if len(data.Slots) != 2 || data.Slots[0].Browser != "chrome" || data.Slots[1].Browser != "firefox" {
		t.Fatalf("slots = %+v", data.Slots)
	}
	html := renderBackupHTML(t, data)
	for _, want := range []string{
		`label="Chrome"`, `label="Firefox"`,
		"2026-10-04T16:38:00Z", "2026-10-03T16:38:00Z",
		"0.4.2",
		"2 domain rules, 1 key rule, keymap (1 changed command), 1 saved reference and 11 more settings",
		"search engine",
		"Domain rules (2)", "*.quickbase.com", "mail.google.com", "Badge size 16 px", ".gear", "Exclude",
		"/methods/download_settings_backup",
		"Download backup file",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(html, "Download backup file"); n != 2 {
		t.Errorf("one download button per copy, got %d", n)
	}
	// The download names its family; the payload is JSON in an attribute.
	if !strings.Contains(html, `{&#34;browser&#34;:&#34;firefox&#34;}`) {
		t.Errorf("firefox download payload missing: %s", html)
	}
	// The disabled rule says so; Firefox has no rules, so no list.
	if strings.Count(html, ">Off<") != 1 {
		t.Errorf("exactly one rule is off")
	}
	if strings.Count(html, "<details") != 1 {
		t.Errorf("only the copy with rules gets a rule list")
	}
	// Read-only: no method but the download.
	if strings.Count(html, "/methods/") != 2 {
		t.Errorf("the only methods are the two downloads; got %s", html)
	}
}

func TestBackupTabEmptyState(t *testing.T) {
	data := buildBackupSettingsData(settingsBackupFile{Slots: map[string]settingsBackupSlot{}}, nil, nil, rfc3339)
	html := renderBackupHTML(t, data)
	if n := strings.Count(html, "No backup yet — the extension sends one a few seconds after any settings change while connected."); n != 2 {
		t.Errorf("both cards empty, got %d empty states", n)
	}
	if strings.Contains(html, "Download backup file") || strings.Contains(html, "/methods/") {
		t.Error("nothing to download without a copy")
	}
	// One family backed up, the other not.
	one := settingsBackupFile{Slots: map[string]settingsBackupSlot{"firefox": {SavedAt: 1, Backup: fullBackupDoc("firefox", `{}`, `{}`)}}}
	html = renderBackupHTML(t, buildBackupSettingsData(one, nil, nil, rfc3339))
	if strings.Count(html, "No backup yet") != 1 || strings.Count(html, "Download backup file") != 1 {
		t.Errorf("one empty card and one download expected: %s", html)
	}
	if !strings.Contains(html, "no settings") {
		t.Error("an empty copy summarises as no settings")
	}
}

// Site patterns, selectors and labels are text the user typed into the
// extension. They reach the page escaped, never as markup.
func TestBackupTabEscapesWhatTheExtensionSent(t *testing.T) {
	const hostile = `<img src=x onerror=alert(1)>`
	sync := `{"domainRules":{"rules":[{"pattern":` + jsonString(hostile) + `,"enabled":true,"entries":[` +
		`{"kind":"exclude","matcher":{"type":"css","selector":` + jsonString(`"><script>alert(2)</script>`) + `},"label":` + jsonString(hostile) + `},` +
		`{"kind":` + jsonString(`<b>kind</b>`) + `,"matcher":{"type":"text","value":` + jsonString(hostile) + `,"caseSensitive":true}}` +
		`]}]}}`
	doc := json.RawMessage(`{"schemaVersion":1,"exportedAt":"2026-10-04","extensionVersion":` + jsonString(hostile) + `,"browser":"chrome","settings":{"sync":` + sync + `,"local":{}}}`)
	file := settingsBackupFile{Slots: map[string]settingsBackupSlot{"chrome": {SavedAt: 1, Backup: doc}}}
	status := map[string]string{"chrome": "Download failed: " + hostile}
	html := renderBackupHTML(t, buildBackupSettingsData(file, errors.New(hostile), status, rfc3339))
	for _, bad := range []string{"<img", "<script", "<b>kind"} {
		if strings.Contains(html, bad) {
			t.Errorf("unescaped %q in %s", bad, html)
		}
	}
	if strings.Count(html, "&lt;img src=x onerror=alert(1)&gt;") < 5 {
		t.Errorf("the hostile text should be shown, escaped, wherever it was sent: %s", html)
	}
}

func TestBackupTabUnreadableCopy(t *testing.T) {
	file := settingsBackupFile{Slots: map[string]settingsBackupSlot{"chrome": {SavedAt: 1, Backup: json.RawMessage(`{"no":"settings"}`)}}}
	html := renderBackupHTML(t, buildBackupSettingsData(file, nil, nil, rfc3339))
	if !strings.Contains(html, "cannot read") || !strings.Contains(html, "Download backup file") {
		t.Errorf("an unreadable copy is named and still downloadable: %s", html)
	}
}

// Each kind is worded as the extension's options page words it.
func TestBackupRuleEntriesReadLikeTheOptionsPage(t *testing.T) {
	sync := json.RawMessage(`{"domainRules":{"rules":[{"pattern":"a.com","enabled":true,"entries":[
		{"kind":"exclude","matcher":{"type":"class","name":"gear"}},
		{"kind":"include","matcher":{"type":"text","value":"Save","caseSensitive":false,"mode":"contains"},"enabled":false},
		{"kind":"reveal","matcher":{"type":"css","selector":".menu"},"reveal":"display"},
		{"kind":"nudge","matcher":{"type":"text","value":"Go","caseSensitive":true},"nudge":{"dx":12,"dy":-8.5},"label":"the go button"},
		"garbage"
	]}, 7]}}`)
	rules := domainRuleViews(sync)
	if len(rules) != 2 || rules[1].Pattern != "(a rule this page cannot read)" {
		t.Fatalf("rules = %+v", rules)
	}
	got := rules[0].Entries
	want := []BackupEntryView{
		{Kind: "Exclude", Matcher: ".gear"},
		{Kind: "Include", Matcher: `text contains "Save" (case-insensitive)`, Off: true},
		{Kind: "Reveal", Matcher: ".menu", Detail: "by display"},
		{Kind: "Nudge", Matcher: `text="Go"`, Detail: "(+12, -8.5)", Note: "the go button"},
		{Kind: "?", Matcher: "(an entry this page cannot read)"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v", got)
	}
	for i := range want {
		g := got[i]
		g.Does = ""
		if g != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, g, want[i])
		}
	}
}

// The file the download hands over is the extension's own format: the stored
// document, indented as the extension's Download button writes it, readable
// by the extension's parseBackup (schemaVersion 1, a settings object).
func TestSettingsBackupDownloadIsTheExtensionsFormat(t *testing.T) {
	doc := fullBackupDoc("chrome", everythingSync, everythingLocal)
	body, name, err := settingsBackupDownload("chrome", settingsBackupSlot{SavedAt: 1, Backup: doc})
	if err != nil {
		t.Fatal(err)
	}
	if name != "branchkit-settings-chrome-2026-10-04.json" {
		t.Errorf("name = %q", name)
	}
	var parsed struct {
		SchemaVersion    int    `json:"schemaVersion"`
		ExportedAt       string `json:"exportedAt"`
		ExtensionVersion string `json:"extensionVersion"`
		Browser          string `json:"browser"`
		Settings         struct {
			Sync  map[string]json.RawMessage `json:"sync"`
			Local map[string]json.RawMessage `json:"local"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if parsed.SchemaVersion != 1 || parsed.Browser != "chrome" || parsed.ExtensionVersion != "0.4.2" || parsed.ExportedAt == "" {
		t.Errorf("header = %+v", parsed)
	}
	if len(parsed.Settings.Sync) != 12 || len(parsed.Settings.Local) != 3 {
		t.Errorf("settings = %d sync, %d local", len(parsed.Settings.Sync), len(parsed.Settings.Local))
	}
	// Same document, byte for byte once compacted: nothing re-encoded.
	var a, b bytes.Buffer
	_ = json.Compact(&a, doc)
	_ = json.Compact(&b, body)
	if a.String() != b.String() {
		t.Errorf("the download changed the document")
	}
	if !strings.HasPrefix(string(body), "{\n  \"schemaVersion\": 1,") {
		t.Errorf("indented like JSON.stringify(backup, null, 2): %.60s", body)
	}
	// A date that is not a date never reaches the file name.
	_, name, _ = settingsBackupDownload("firefox", settingsBackupSlot{
		SavedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC).UnixMilli(),
		Backup:  json.RawMessage(`{"schemaVersion":1,"exportedAt":"../../etc/x","settings":{"sync":{},"local":{}}}`),
	})
	if name != "branchkit-settings-firefox-2026-01-02.json" {
		t.Errorf("fallback name = %q", name)
	}
}

// The download method stages the file in the data dir, hands it to the
// platform's export under the extension's file name, removes the stage, and
// shows the outcome on the card.
func TestDownloadSettingsBackupMethod(t *testing.T) {
	dir := withDataDir(t)
	h := newTestHost()
	refreshed := 0
	h.refreshSettings = func() { refreshed++ }
	var gotRel, gotName string
	var staged []byte
	h.exportDataFile = func(rel, name string) (string, error) {
		gotRel, gotName = rel, name
		staged, _ = os.ReadFile(filepath.Join(dir, rel))
		return "/Users/me/Downloads/" + name, nil
	}

	// Nothing stored yet.
	if err := h.handleDownloadSettingsBackup(&DownloadSettingsBackupRequest{Browser: "chrome"}); err != nil {
		t.Fatal(err)
	}
	if got := h.backupExportStatuses()["chrome"]; !strings.Contains(got, "no backup to download") {
		t.Errorf("status = %q", got)
	}

	doc := fullBackupDoc("chrome", everythingSync, everythingLocal)
	data, _ := json.Marshal(settingsBackupFile{Slots: map[string]settingsBackupSlot{"chrome": {SavedAt: 1, Backup: doc}}})
	if err := os.WriteFile(filepath.Join(dir, settingsBackupFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.handleDownloadSettingsBackup(&DownloadSettingsBackupRequest{Browser: "chrome"}); err != nil {
		t.Fatal(err)
	}
	if gotName != "branchkit-settings-chrome-2026-10-04.json" || gotRel != settingsBackupStageName("chrome") {
		t.Errorf("export(%q, %q)", gotRel, gotName)
	}
	var a, b bytes.Buffer
	_ = json.Compact(&a, doc)
	_ = json.Compact(&b, staged)
	if a.String() != b.String() {
		t.Errorf("staged file is not the stored document: %s", staged)
	}
	if _, err := os.Stat(filepath.Join(dir, gotRel)); !os.IsNotExist(err) {
		t.Errorf("the staged copy should be gone: %v", err)
	}
	if got := h.backupExportStatuses()["chrome"]; got != "Saved to /Users/me/Downloads/branchkit-settings-chrome-2026-10-04.json" {
		t.Errorf("status = %q", got)
	}
	if refreshed != 2 {
		t.Errorf("each download re-renders the tab; refreshed %d", refreshed)
	}

	// The platform refusing is shown, and the stage is still cleaned up.
	h.exportDataFile = func(string, string) (string, error) { return "", errors.New("no Downloads directory") }
	_ = h.handleDownloadSettingsBackup(&DownloadSettingsBackupRequest{Browser: "chrome"})
	if got := h.backupExportStatuses()["chrome"]; got != "Download failed: no Downloads directory" {
		t.Errorf("status = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, settingsBackupStageName("chrome"))); !os.IsNotExist(err) {
		t.Errorf("the staged copy should be gone after a failure too")
	}

	if err := h.handleDownloadSettingsBackup(&DownloadSettingsBackupRequest{Browser: "../x"}); err == nil {
		t.Error("an unknown family is refused")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
