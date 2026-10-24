package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

func TestIsSafeSnapshotID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Happy path: ISO-timestamp shape the content script generates.
		{"2026-05-20T15-30-45-123Z", true},
		{"valid_id", true},
		{"a", true},
		{"with.dot", true},
		{"with-dash", true},

		// Empty / path-traversal-shaped.
		{"", false},
		{".", false},
		{"..", false},
		{"with/slash", false},
		{"with\\backslash", false},
		{"with..dots", false},
		{"with\x00null", false},
		{"../../etc/passwd", false},

		// Whitespace + non-ASCII.
		{"has space", false},
		{"emoji😀", false},
	}
	for _, tc := range cases {
		if got := isSafeSnapshotID(tc.in); got != tc.want {
			t.Errorf("isSafeSnapshotID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Length cap.
	tooLong := ""
	for range maxSnapshotIDLen + 1 {
		tooLong += "a"
	}
	if isSafeSnapshotID(tooLong) {
		t.Errorf("oversize id should be rejected (got %d chars)", len(tooLong))
	}
}

func TestPruneSnapshotsKeepsNewest(t *testing.T) {
	dir := t.TempDir()

	// Create 25 fake snapshot directories with sortable names.
	// Names sort lexicographically the same as chronologically thanks
	// to the zero-padding — same property the production ISO-timestamp
	// ids rely on.
	for i := range 25 {
		name := "2026-05-20T15-30-" + twoDigit(i) + "Z"
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pruneSnapshotsIn(dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != snapshotRetentionCount {
		t.Errorf("expected %d directories after prune, got %d", snapshotRetentionCount, len(entries))
	}

	// The 5 oldest (00..04) should be gone; 05..24 should remain.
	for _, e := range entries {
		// Substring 19..21 is the seconds field in the synthetic name.
		seconds := e.Name()[17:19]
		if seconds < "05" {
			t.Errorf("oldest directory %q should have been pruned", e.Name())
		}
	}
}

func TestPruneSnapshotsBelowThresholdNoops(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		_ = os.MkdirAll(filepath.Join(dir, "id_"+twoDigit(i)), 0o755)
	}
	pruneSnapshotsIn(dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 5 {
		t.Errorf("prune with %d dirs should be a no-op, got %d remaining", 5, len(entries))
	}
}

func TestPruneSnapshotsIgnoresFiles(t *testing.T) {
	dir := t.TempDir()
	// Mix files (which prune should ignore) with directories.
	for i := range 22 {
		_ = os.MkdirAll(filepath.Join(dir, "d_"+twoDigit(i)), 0o755)
	}
	_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("ignore me"), 0o644)

	pruneSnapshotsIn(dir)

	if _, err := os.Stat(filepath.Join(dir, "stray.txt")); err != nil {
		t.Errorf("file at root should not be deleted by prune: %v", err)
	}
	dirCount := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			dirCount++
		}
	}
	if dirCount != snapshotRetentionCount {
		t.Errorf("expected %d directories after prune, got %d", snapshotRetentionCount, dirCount)
	}
}

func TestPatchScreenshotErrorAddsField(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "snapshot.json")
	original := map[string]any{
		"snapshot_id": "test_id",
		"wrappers":    []any{},
	}
	body, _ := json.MarshalIndent(original, "", "  ")
	if err := os.WriteFile(jsonPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := patchScreenshotError(jsonPath, "RESTRICTED_PAGE"); err != nil {
		t.Fatal(err)
	}

	patched, _ := os.ReadFile(jsonPath)
	var obj map[string]any
	if err := json.Unmarshal(patched, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["screenshot_error"] != "RESTRICTED_PAGE" {
		t.Errorf("expected screenshot_error to be set, got %v", obj["screenshot_error"])
	}
	// Original fields should still be present.
	if obj["snapshot_id"] != "test_id" {
		t.Errorf("original field lost after patch: %v", obj["snapshot_id"])
	}
}

func TestPatchScreenshotErrorReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "snapshot.json")
	original := map[string]any{
		"snapshot_id":      "test_id",
		"screenshot_error": "OLD_REASON",
	}
	body, _ := json.MarshalIndent(original, "", "  ")
	_ = os.WriteFile(jsonPath, body, 0o644)

	if err := patchScreenshotError(jsonPath, "NEW_REASON"); err != nil {
		t.Fatal(err)
	}

	patched, _ := os.ReadFile(jsonPath)
	var obj map[string]any
	_ = json.Unmarshal(patched, &obj)
	if obj["screenshot_error"] != "NEW_REASON" {
		t.Errorf("expected replacement, got %v", obj["screenshot_error"])
	}
}

func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// The reconcile's matchable view is the platform's: the gate read back from
// its record (not the plugin's own belief), and only this plugin's commands
// whose tags that gate satisfies.
func TestDebugMatchableReadsThePlatformGate(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser"}
	cmds := []branchkit.EnumeratedCommand{
		{OwnerPlugin: "browser", RequiresTags: []string{"plugin.browser.hints"}},
		{OwnerPlugin: "browser", RequiresTags: []string{"plugin.browser.hints", "plugin.browser.caret"}},
		{OwnerPlugin: "browser"},
		{OwnerPlugin: "tiling", RequiresTags: []string{"plugin.browser.hints"}},
	}
	h.enumerateCommands = func() ([]branchkit.EnumeratedCommand, error) { return cmds, nil }

	h.hintsTagHeld = func(string) (bool, error) { return false, nil }
	off, err := h.handleDebugMatchableGet(nil, &wire.Empty{})
	if err != nil || len(off.ActiveTags) != 0 || len(off.Eligible) != 0 {
		t.Fatalf("gate not held: want nothing active or eligible, got %+v, %v", off, err)
	}

	h.hintsTagHeld = func(string) (bool, error) { return true, nil }
	on, err := h.handleDebugMatchableGet(nil, &wire.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(on.ActiveTags) != 1 || on.ActiveTags[0] != "plugin.browser.hints" {
		t.Fatalf("active tags: %v", on.ActiveTags)
	}
	if len(on.Eligible) != 1 || len(on.Eligible[0].RequiresTags) != 1 {
		t.Fatalf("eligible: want only the hints-gated browser command, got %+v", on.Eligible)
	}
}
