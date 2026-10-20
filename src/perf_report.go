package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
	"github.com/branchkit/plugin-sdk-go"
)

// perfReportMaxBytes caps the active trail file; past it the file rotates to
// `.jsonl.1` (one backup), bounding disk to ~2x this. The trail is gitignored
// debug data, so a bounded tail is all we need — left uncapped it grew to
// hundreds of MB. Mirrors the platform audit-log's size-rotation convention.
const perfReportMaxBytes = 64 << 20 // 64 MiB

// perfReportMu serializes rotation + append. Multiple tabs ship every 5s, and a
// bare O_APPEND write racing a rename-then-recreate would drop or duplicate
// lines around the rotation boundary.

// appendPerfReportLine appends line (plus a trailing newline) to path, rotating
// path -> path+".1" first if this write would push the active file past
// maxBytes. Exactly one backup generation is kept.
func (h *Host) appendPerfReportLine(path string, line []byte, maxBytes int64) error {
	h.perfReportMu.Lock()
	defer h.perfReportMu.Unlock()

	if fi, err := os.Stat(path); err == nil && fi.Size()+int64(len(line))+1 > maxBytes {
		// Overwrite any prior backup; we keep exactly one generation.
		_ = os.Rename(path, path+".1")
	}

	// Owner-only: each line carries a page URL. The mode argument only
	// applies when the file is created, so a trail left 0644 by an older
	// build is tightened here too.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// PerfReportEntry is one line appended to extension-perf.jsonl.
//
// The extension's content script publishes a perf snapshot every 5s; the
// background script forwards it here so we have a forensic trail to
// correlate against Firefox's unresponsive-script reports (the dialog
// fires too late to capture; we need a continuous record we can grep
// post-hoc against the wall-clock timestamp).
type PerfReportEntry struct {
	TS       string          `json:"ts"`
	URL      string          `json:"url"`
	TabID    int             `json:"tab_id"`
	Browser  string          `json:"browser"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// handlePerfReportPost appends one extension perf snapshot to the per-plugin
// JSONL trail, rotating at perfReportMaxBytes (see appendPerfReportLine). The
// file is gitignored debug data, not production state, and the recent tail is
// what diagnoses transient unresponsiveness.
func (h *Host) handlePerfReportPost(_ *http.Request, req *wire.PerfReportRequest) (wire.Ack, error) {
	if len(req.Snapshot) == 0 {
		return wire.Ack{}, badRequest("empty snapshot")
	}

	entry := PerfReportEntry{
		TS:       time.Now().UTC().Format(time.RFC3339Nano),
		URL:      req.URL,
		TabID:    req.TabID,
		Browser:  req.Browser,
		Snapshot: req.Snapshot,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return wire.Ack{}, err
	}

	path := filepath.Join(branchkit.PluginDir(), "extension-perf.jsonl")
	if err := h.appendPerfReportLine(path, line, perfReportMaxBytes); err != nil {
		branchkit.Logf("browser", "perf report write failed: %v", err)
		return wire.Ack{}, statusError(http.StatusInternalServerError, "io error")
	}
	return ack()
}
