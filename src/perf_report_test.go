package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendPerfReportLineRotates(t *testing.T) {
	h := newTestHost()
	dir := t.TempDir()
	path := filepath.Join(dir, "extension-perf.jsonl")

	line := []byte(`{"ts":"x","snapshot":{}}`)
	// Cap chosen so the third write trips rotation: two lines (+newlines) fit,
	// the third would exceed it.
	maxBytes := int64(2 * (len(line) + 1))

	for range 5 {
		if err := h.appendPerfReportLine(path, line, maxBytes); err != nil {
			t.Fatalf("append failed: %v", err)
		}
	}

	// Active file must stay within the cap (never accumulates all 5 lines).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if fi.Size() > maxBytes {
		t.Errorf("active file %d bytes exceeds cap %d", fi.Size(), maxBytes)
	}

	// Exactly one backup generation is kept.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated backup .1 to exist: %v", err)
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Errorf("expected no .2 backup (one generation only), got err=%v", err)
	}
}

func TestAppendPerfReportLineCreatesFile(t *testing.T) {
	h := newTestHost()
	dir := t.TempDir()
	path := filepath.Join(dir, "extension-perf.jsonl")

	if err := h.appendPerfReportLine(path, []byte(`{"a":1}`), perfReportMaxBytes); err != nil {
		t.Fatalf("append failed: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "{\"a\":1}\n" {
		t.Errorf("unexpected contents: %q", b)
	}
}
