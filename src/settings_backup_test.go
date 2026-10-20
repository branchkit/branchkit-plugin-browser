package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func backupDoc(sync map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "exportedAt": "2026-10-04T12:00:00Z", "extensionVersion": "0.1.0", "browser": "chrome",
		"settings": map[string]any{"sync": sync, "local": map[string]any{}},
	})
	return string(b)
}

func putBackup(t *testing.T, h *Host, conn, browser, doc string, force bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"conn_id": conn, "browser": browser, "backup": json.RawMessage(doc), "force": force})
	rec := httptest.NewRecorder()
	serveRoute(h, rec, httptest.NewRequest(http.MethodPut, "/settings-backup", strings.NewReader(string(body))))
	return rec
}

func getBackup(t *testing.T, h *Host, browser string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	target := "/settings-backup"
	if browser != "" {
		target += "?browser=" + browser
	}
	serveRoute(h, rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status %d body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSettingsBackupRoundTrip(t *testing.T) {
	withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)

	if got := getBackup(t, h, "chrome"); got["found"] != false {
		t.Fatalf("no copy yet: %v", got)
	}
	doc := backupDoc(map[string]any{"searchEngine": "https://example.test/?q=%s"})
	if rec := putBackup(t, h, "chrome", "chrome", doc, false); rec.Code != http.StatusOK {
		t.Fatalf("PUT status %d body %s", rec.Code, rec.Body.String())
	}
	got := getBackup(t, h, "chrome")
	if got["found"] != true || got["browser"] != "chrome" {
		t.Fatalf("GET = %v", got)
	}
	back, _ := json.Marshal(got["backup"])
	var want, have any
	_ = json.Unmarshal([]byte(doc), &want)
	_ = json.Unmarshal(back, &have)
	wb, _ := json.Marshal(want)
	hb, _ := json.Marshal(have)
	if string(wb) != string(hb) {
		t.Errorf("round trip changed the document:\n got %s\nwant %s", hb, wb)
	}
}

// A family with no copy of its own is offered the newest copy of any.
func TestSettingsBackupFallsBackToTheNewestFamily(t *testing.T) {
	withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "ff", firefoxBundle)
	if rec := putBackup(t, h, "ff", "firefox", backupDoc(map[string]any{"keymap": map[string]any{}}), false); rec.Code != http.StatusOK {
		t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
	}
	if got := getBackup(t, h, "chrome"); got["found"] != true || got["browser"] != "firefox" {
		t.Errorf("chrome with no slot should get firefox's copy: %v", got)
	}
	if got := getBackup(t, h, ""); got["browser"] != "firefox" {
		t.Errorf("no family named: the newest copy, got %v", got)
	}
}

func TestSettingsBackupSizeCap(t *testing.T) {
	withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	big := backupDoc(map[string]any{"branchkit_references": strings.Repeat("x", maxSettingsBackupBytes)})
	rec := putBackup(t, h, "chrome", "chrome", big, false)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if got := getBackup(t, h, "chrome"); got["found"] != false {
		t.Errorf("an oversized copy must not be stored: %v", got)
	}
}

// Defence in depth for the extension's own rule: an empty copy does not
// replace one that has settings unless forced.
func TestSettingsBackupRefusesEmptyOverNonEmptyUnlessForced(t *testing.T) {
	withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	if rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{"searchEngine": "a %s"}), false); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{}), false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("empty over non-empty: status %d, want 409", rec.Code)
	}
	if got := getBackup(t, h, "chrome"); !strings.Contains(mustJSON(got["backup"]), "searchEngine") {
		t.Fatalf("the good copy must survive: %v", got)
	}
	if rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{}), true); rec.Code != http.StatusOK {
		t.Fatalf("forced: status %d", rec.Code)
	}
	if got := getBackup(t, h, "chrome"); strings.Contains(mustJSON(got["backup"]), "searchEngine") {
		t.Errorf("force should have replaced it: %v", got)
	}
}

func TestSettingsBackupRefusals(t *testing.T) {
	withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	doc := backupDoc(map[string]any{"a": 1})
	cases := []struct {
		name, conn, browser, doc string
		want                     int
	}{
		{"no conn", "", "chrome", doc, http.StatusBadRequest},
		{"bad family", "chrome", "safari", doc, http.StatusBadRequest},
		{"not a backup", "chrome", "chrome", `{"hello":1}`, http.StatusBadRequest},
		{"not an object", "chrome", "chrome", `[1,2]`, http.StatusBadRequest},
		{"no live stream", "ghost", "chrome", doc, http.StatusConflict},
	}
	for _, c := range cases {
		if rec := putBackup(t, h, c.conn, c.browser, c.doc, false); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

// A failed save leaves the previous copy whole and no temp file behind.
func TestSettingsBackupWriteIsAtomic(t *testing.T) {
	dir := withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	if rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{"v": "one"}), false); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	before, _ := os.ReadFile(filepath.Join(dir, settingsBackupFileName))

	backupRename = func(string, string) error { return errors.New("disk on fire") }
	t.Cleanup(func() { backupRename = os.Rename })
	if rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{"v": "two"}), false); rec.Code == http.StatusOK {
		t.Fatal("a failed rename must fail the request")
	}
	after, _ := os.ReadFile(filepath.Join(dir, settingsBackupFileName))
	if string(before) != string(after) {
		t.Error("a failed save changed the stored copy")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != settingsBackupFileName {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	// Windows has no Unix mode bits (Go reports 0666 for any writable
	// file); there the plugin's data folder's ACL is what keeps it private.
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(filepath.Join(dir, settingsBackupFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v (err %v), want 0600", info.Mode().Perm(), err)
	}
}

// An unreadable file is not silently replaced: it might be the only copy.
func TestSettingsBackupDoesNotReplaceAnUnreadableFile(t *testing.T) {
	dir := withDataDir(t)
	h, _ := newRoutingHost(t)
	connectBrowser(h, "chrome", chromeBundle)
	path := filepath.Join(dir, settingsBackupFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := putBackup(t, h, "chrome", "chrome", backupDoc(map[string]any{"a": 1}), false); rec.Code == http.StatusOK {
		t.Fatal("must refuse to overwrite an unreadable file")
	}
	if b, _ := os.ReadFile(path); string(b) != "{not json" {
		t.Error("the unreadable file was replaced")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
