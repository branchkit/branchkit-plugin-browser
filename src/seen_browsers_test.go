package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRANCHKIT_PLUGIN_DATA", dir)
	return dir
}

// The cold-start case this exists for: BranchKit starts with an extension
// already wedged. In memory nothing has ever connected, so without persistence
// the tripwire never arms and the failure is silent.
func TestAPreviouslyConnectedBrowserSurvivesRestart(t *testing.T) {
	withDataDir(t)
	saveSeenBrowser("com.google.Chrome")

	// A fresh process: the in-memory set is rebuilt from disk alone.
	if !loadSeenBrowsers()["com.google.Chrome"] {
		t.Fatal("a browser that connected before must still be expected after a restart")
	}
}

// The distinction the whole tripwire rests on: a browser that never connected
// is not "vanished", it is standalone by choice, and must stay silent.
func TestABrowserThatNeverConnectedIsNeverExpected(t *testing.T) {
	withDataDir(t)
	saveSeenBrowser("com.google.Chrome")

	if loadSeenBrowsers()["org.mozilla.firefox"] {
		t.Fatal("never-connected browser must not be expected — that is the false alarm")
	}
}

// "It worked last week" is actionable; "it worked eight months ago" is not,
// and is also what a deliberate uninstall looks like from here.
func TestAStaleEntryExpiresRatherThanWarningForever(t *testing.T) {
	dir := withDataDir(t)
	old := time.Now().Add(-(seenBrowserTTL + time.Hour)).UnixMilli()
	raw, _ := json.Marshal(map[string]int64{"com.google.Chrome": old})
	if err := os.WriteFile(filepath.Join(dir, "seen-browsers.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if loadSeenBrowsers()["com.google.Chrome"] {
		t.Fatal("an entry past the TTL must not keep arming the nudge")
	}
}

func TestConnectingRefreshesTheTTL(t *testing.T) {
	dir := withDataDir(t)
	old := time.Now().Add(-(seenBrowserTTL + time.Hour)).UnixMilli()
	raw, _ := json.Marshal(map[string]int64{"com.google.Chrome": old})
	os.WriteFile(filepath.Join(dir, "seen-browsers.json"), raw, 0o600)

	saveSeenBrowser("com.google.Chrome")
	if !loadSeenBrowsers()["com.google.Chrome"] {
		t.Fatal("a browser back in use must not stay expired")
	}
}

// Read-modify-write, not a dump of the in-memory set: a bundle whose entry
// aged out of THIS session's memory keeps its stored timestamp.
func TestSavingOneBrowserDoesNotForgetAnother(t *testing.T) {
	withDataDir(t)
	saveSeenBrowser("com.google.Chrome")
	saveSeenBrowser("org.mozilla.firefox")

	got := loadSeenBrowsers()
	if !got["com.google.Chrome"] || !got["org.mozilla.firefox"] {
		t.Fatalf("both must survive, got %v", got)
	}
}

// Persistence is a convenience. It must never be why the plugin fails to
// start, and a plugin run by hand has no data dir at all.
func TestNoDataDirIsSilentNotFatal(t *testing.T) {
	t.Setenv("BRANCHKIT_PLUGIN_DATA", "")
	saveSeenBrowser("com.google.Chrome") // must not panic
	if len(loadSeenBrowsers()) != 0 {
		t.Fatal("no data dir means no persisted set")
	}
}

func TestCorruptFileIsTreatedAsEmpty(t *testing.T) {
	dir := withDataDir(t)
	os.WriteFile(filepath.Join(dir, "seen-browsers.json"), []byte("{not json"), 0o600)
	if len(loadSeenBrowsers()) != 0 {
		t.Fatal("unreadable state must degrade to empty, not crash or resurrect")
	}
}
