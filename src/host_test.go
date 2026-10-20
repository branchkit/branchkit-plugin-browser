package main

import (
	"errors"

	"github.com/branchkit/plugin-sdk-go"
)

// newTestHost is a host on a detached plugin — no platform behind it, so a
// handler that reaches for one fails at once instead of being skipped by a
// nil guard — plus the floor TestMain used to set once for the whole
// package: the alphabet (production reads it from the alphabet store) and
// no-op seams for the collection writes and the prefix query, so a test
// that forgets to install one gets an empty platform rather than an error.
// Tests that assert on what a seam received still override it locally.
func newTestHost() *Host {
	h := newHost(branchkit.NewDetachedPlugin())
	h.livePrefixes = func(string) (map[string]bool, error) { return nil, nil }
	h.commitVocabulary = func() error { return nil }
	h.codewords = testCodewords
	h.replaceTabsCollection = func([]branchkit.CollectionPutEntry) error { return nil }
	h.replacePaletteRows = func([]branchkit.CollectionPutEntry) error { return nil }
	h.replacePaletteReserves = func([]branchkit.CollectionPutEntry) error { return nil }
	// A test never presses a real key: a test that wants the press installs
	// its own recorder.
	h.pressShortcut = func(osShortcut) error { return errors.New("test host presses no keys") }
	h.frontmostBundle = func() (string, error) { return "", errors.New("test host has no OS") }
	h.reportProblem = func(actionProblem) {}
	return h
}
