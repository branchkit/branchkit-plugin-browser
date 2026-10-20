package main

// Hint-diagnostics snapshot endpoint. Two routes:
//
//   POST /debug-snapshot              JSON state half
//   POST /debug-snapshot/screenshot   viewport PNG (or capture error)
//
// The two-endpoint split mirrors the design's §2.5(c) decision:
// content-script writes the structured data immediately, the
// service-worker attaches the PNG as a follow-up keyed by snapshot_id.
// Simpler than multipart, lets content+SW run in parallel.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// snapshotRetentionCount caps total stored snapshots; Q5 of the design.
// On each new snapshot write, anything older than the 20th most recent
// is deleted. Avoids unbounded growth without forcing user cleanup.
const snapshotRetentionCount = 20

// maxSnapshotIDLen caps id length so a malformed request can't produce
// a pathologically deep filesystem entry.
const maxSnapshotIDLen = 128

// snapshotIDPattern: filesystem-safe characters only — alphanumeric,
// hyphen, dot, underscore. The content script generates ISO timestamps
// like "2026-05-20T15-30-45-123Z" (colons replaced with hyphens for
// Windows-fs safety). Validated server-side so the id can be tampered
// with in transit and still not escape the snapshots/ directory.
var snapshotIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func isSafeSnapshotID(id string) bool {
	if id == "" || len(id) > maxSnapshotIDLen {
		return false
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return false
	}
	return snapshotIDPattern.MatchString(id)
}

func snapshotsDir() string {
	return filepath.Join(branchkit.PluginDir(), "snapshots")
}

// handleDebugSnapshot persists the structured-state half of a debug
// snapshot. Accepts any JSON shape; the only required field is
// `snapshot_id` (validated for filesystem safety). Companion endpoint
// /debug-snapshot/screenshot attaches the PNG keyed by the same id.
//
// On success: writes snapshot.json into snapshots/<id>/, prunes older
// directories beyond the retention count, and emits a BK_SNAPSHOT_WRITTEN
// per-plugin log line with the directory path so the user can grep+share.
func (h *Host) handleDebugSnapshot(_ *http.Request, snap *wire.DebugSnapshot) (wire.Ack, error) {
	body := []byte(*snap)

	// First pass: extract snapshot_id without committing to a payload shape.
	// The rest of the JSON is pretty-printed and persisted as-is.
	var idEnvelope struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(body, &idEnvelope); err != nil {
		return wire.Ack{}, badRequest("bad json")
	}
	if !isSafeSnapshotID(idEnvelope.SnapshotID) {
		return wire.Ack{}, badRequest("bad snapshot_id")
	}

	// Pretty-print for human-readable artifacts.
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return wire.Ack{}, badRequest("bad json")
	}
	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return wire.Ack{}, statusError(http.StatusInternalServerError, "marshal failed")
	}

	dir := filepath.Join(snapshotsDir(), idEnvelope.SnapshotID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wire.Ack{}, statusError(http.StatusInternalServerError, "mkdir failed")
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), pretty, 0o600); err != nil {
		return wire.Ack{}, statusError(http.StatusInternalServerError, "write failed")
	}

	pruneSnapshotsIn(snapshotsDir())

	h.plugin.Info("BK_SNAPSHOT_WRITTEN", map[string]any{
		"snapshot_id": idEnvelope.SnapshotID,
		"path":        dir,
		"bytes":       len(pretty),
	})
	return ack()
}

// handleDebugSnapshotScreenshot attaches the viewport PNG (or records a
// capture error) to a previously-created snapshot. Body shape:
//
//	{snapshot_id: string, png_base64?: string, error?: string}
//
// Exactly one of png_base64 / error must be present. On success: writes
// viewport.png alongside snapshot.json. On error (chrome.tabs.captureVisibleTab
// returned a permission/throttle failure, restricted page, etc.): patches
// snapshot.json with a top-level `screenshot_error` field and skips the PNG.
// See §2.5(e) of the design — partial data beats no data when the structured
// state is the more useful half.
func (h *Host) handleDebugSnapshotScreenshot(_ *http.Request, req *wire.DebugScreenshotRequest) (wire.Ack, error) {
	if !isSafeSnapshotID(req.SnapshotID) {
		return wire.Ack{}, badRequest("bad snapshot_id")
	}
	dir := filepath.Join(snapshotsDir(), req.SnapshotID)
	if _, err := os.Stat(dir); err != nil {
		return wire.Ack{}, statusError(http.StatusNotFound, "snapshot not found")
	}

	if req.Error != "" {
		jsonPath := filepath.Join(dir, "snapshot.json")
		if err := patchScreenshotError(jsonPath, req.Error); err != nil {
			return wire.Ack{}, statusError(http.StatusInternalServerError, "patch failed")
		}
		return ack()
	}

	if req.PngBase64 == "" {
		return wire.Ack{}, badRequest("missing png_base64 or error")
	}
	png, err := base64.StdEncoding.DecodeString(req.PngBase64)
	if err != nil {
		return wire.Ack{}, badRequest("bad base64")
	}
	if err := os.WriteFile(filepath.Join(dir, "viewport.png"), png, 0o600); err != nil {
		return wire.Ack{}, statusError(http.StatusInternalServerError, "write failed")
	}
	return ack()
}

// patchScreenshotError adds (or replaces) a top-level `screenshot_error`
// field on an existing snapshot.json. Used when captureVisibleTab failed —
// the structured state is already persisted, this just annotates why the
// PNG is missing. Read-modify-write is fine; this path runs at most once
// per snapshot.
func patchScreenshotError(jsonPath, reason string) error {
	body, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return err
	}
	if obj == nil {
		obj = map[string]any{}
	}
	obj["screenshot_error"] = reason
	pretty, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(jsonPath, pretty, 0o600)
}

// pruneSnapshotsIn enforces Q5: keep the N most recent directories,
// delete the rest. Sort is lexicographic on directory name — works
// because the content script generates ISO-timestamp-shaped ids, which
// sort chronologically when compared as strings. Extracted with an
// explicit base-dir argument so tests can drive it against a tempdir.
func pruneSnapshotsIn(baseDir string) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) <= snapshotRetentionCount {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-snapshotRetentionCount] {
		_ = os.RemoveAll(filepath.Join(baseDir, name))
	}
}
