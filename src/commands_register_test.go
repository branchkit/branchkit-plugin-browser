package main

import (
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// captureGroupPushes stubs the group-push seam and records, in order, which
// group each push targeted and how many commands it carried.
type groupPush struct {
	group string
	count int
}

func captureGroupPushes(h *Host, t *testing.T) *[]groupPush {
	t.Helper()
	var seen []groupPush
	orig := h.pushCommandGroupRPC
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		seen = append(seen, groupPush{group: group, count: len(cmds)})
		return len(cmds), nil
	}
	t.Cleanup(func() { h.pushCommandGroupRPC = orig })
	return &seen
}

func groupsIn(pushes []groupPush) []string {
	out := make([]string, 0, len(pushes))
	for _, p := range pushes {
		out = append(out, p.group)
	}
	return out
}

// The whole point of the migration: a source that changes re-pushes ONLY its
// own group. Under the old union every one of these rebuilt and re-pushed all
// five sources, so a reference rename re-derived the hint skeleton and the
// contributed catalog to say nothing new about either.
func TestPushCommandGroupTouchesOnlyThatGroup(t *testing.T) {
	h := newTestHost()
	for _, group := range []string{
		groupContributed, groupEscape, groupTabDesk, groupReferences, groupHints,
	} {
		t.Run(group, func(t *testing.T) {
			pushes := captureGroupPushes(h, t)
			if err := h.pushCommandGroup("browser", group); err != nil {
				t.Fatalf("pushCommandGroup(%q): %v", group, err)
			}
			if got := groupsIn(*pushes); len(got) != 1 || got[0] != group {
				t.Fatalf("pushed groups = %v, want exactly [%s]", got, group)
			}
		})
	}
}

// Startup determines the whole set in one pass. A group with nothing in it
// yet is pushed as EMPTY rather than skipped: skipping would leave whatever
// the previous process put there live, which for the hint skeleton means
// codewords outliving the alphabet that produced them.
func TestPushAllCommandGroupsCoversEverySource(t *testing.T) {
	h := newTestHost()
	pushes := captureGroupPushes(h, t)
	if err := h.pushAllCommandGroups("browser"); err != nil {
		t.Fatalf("pushAllCommandGroups: %v", err)
	}

	want := []string{
		groupContributed, groupEscape, groupTabDesk, groupReferences, groupHints,
	}
	got := groupsIn(*pushes)
	if len(got) != len(want) {
		t.Fatalf("pushed groups = %v, want all %d of %v", got, len(want), want)
	}
	for i, g := range want {
		if got[i] != g {
			t.Fatalf("pushed groups = %v, want %v", got, want)
		}
	}
}

// A source failing must not strand the sources after it. They are independent
// by construction now, so an error in one is not a reason to leave the rest of
// the plugin's command set unpublished.
func TestPushAllCommandGroupsContinuesPastAFailingSource(t *testing.T) {
	h := newTestHost()
	var seen []string
	orig := h.pushCommandGroupRPC
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		seen = append(seen, group)
		if group == groupEscape {
			return 0, errTestPushFailed
		}
		return len(cmds), nil
	}
	t.Cleanup(func() { h.pushCommandGroupRPC = orig })

	err := h.pushAllCommandGroups("browser")
	if err != errTestPushFailed {
		t.Fatalf("err = %v, want the failing source's error surfaced", err)
	}
	if len(seen) != 5 {
		t.Fatalf("pushed %v, want all 5 attempted despite the failure", seen)
	}
}

var errTestPushFailed = testPushError("push failed")

type testPushError string

func (e testPushError) Error() string { return string(e) }
