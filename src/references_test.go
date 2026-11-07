package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/branchkit/plugin-sdk-go"
)

// refsSeen records what the references source last published: the name
// records written to plugin.browser.refs and whether the references group
// carried the `reference <name>` command.
type refsSeen struct {
	names        []string
	hasReference bool
	pushes       int
}

func captureReferences(t *testing.T, h *Host) *refsSeen {
	t.Helper()
	seen := &refsSeen{}
	h.replaceRefsCollection = func(_ string, records []branchkit.CollectionPutEntry) error {
		seen.names = seen.names[:0]
		for _, r := range records {
			seen.names = append(seen.names, r.ID)
		}
		return nil
	}
	h.pushCommandGroupRPC = func(group string, cmds []branchkit.CommandSpec) (int, error) {
		if group != groupReferences {
			return len(cmds), nil
		}
		seen.pushes++
		seen.hasReference = false
		for _, c := range cmds {
			if strings.Contains(string(c.Action), "resolve_reference") {
				seen.hasReference = true
			}
		}
		return len(cmds), nil
	}
	return seen
}

func postReferenceNames(t *testing.T, h *Host, conn string, names []string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"conn_id": conn, "names": names})
	req := httptest.NewRequest(http.MethodPost, "/references", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	serveRoute(h, rec, req)
	return rec.Code
}

// A second browser with no saved references reconnects and posts an empty
// list. It used to replace the one plugin-wide set, so the first browser's
// `reference <name>` stopped matching.
func TestReferenceNamesOfOneBrowserSurviveAnothersEmptyPost(t *testing.T) {
	h, _ := newRoutingHost(t)
	seen := captureReferences(t, h)
	connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)

	if code := postReferenceNames(t, h, "chrome", []string{"bravo", "alpha"}); code != http.StatusOK {
		t.Fatalf("chrome post status = %d", code)
	}
	if code := postReferenceNames(t, h, "fox", nil); code != http.StatusOK {
		t.Fatalf("fox post status = %d", code)
	}

	if want := []string{"alpha", "bravo"}; !reflect.DeepEqual(seen.names, want) {
		t.Errorf("refs collection = %v, want %v", seen.names, want)
	}
	if !seen.hasReference {
		t.Error("the reference command was dropped by another browser's empty post")
	}
}

// Both browsers' names are matchable, de-duplicated.
func TestReferenceNamesAreTheUnionOfConnections(t *testing.T) {
	h, _ := newRoutingHost(t)
	seen := captureReferences(t, h)
	connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)

	postReferenceNames(t, h, "chrome", []string{"alpha", "bravo"})
	postReferenceNames(t, h, "fox", []string{"bravo", "charlie"})

	if want := []string{"alpha", "bravo", "charlie"}; !reflect.DeepEqual(seen.names, want) {
		t.Errorf("refs collection = %v, want %v", seen.names, want)
	}
}

// A closed browser's names go with its stream, and with the last names gone
// the reference command goes too.
func TestClosedConnectionsReferenceNamesDrop(t *testing.T) {
	h, _ := newRoutingHost(t)
	seen := captureReferences(t, h)
	chromeClient, _ := connectBrowser(h, "chrome", chromeBundle)
	foxClient, _ := connectBrowser(h, "fox", firefoxBundle)
	postReferenceNames(t, h, "chrome", []string{"alpha"})
	postReferenceNames(t, h, "fox", []string{"bravo"})

	h.disconnectSSEClient("browser", "fox", foxClient)
	if want := []string{"alpha"}; !reflect.DeepEqual(seen.names, want) {
		t.Errorf("after fox closed, refs collection = %v, want %v", seen.names, want)
	}
	if !seen.hasReference {
		t.Error("chrome's names remain, so the reference command must too")
	}

	h.disconnectSSEClient("browser", "chrome", chromeClient)
	if len(seen.names) != 0 {
		t.Errorf("after every browser closed, refs collection = %v, want empty", seen.names)
	}
	if seen.hasReference {
		t.Error("the reference command outlived every saved name")
	}
}

// A same-conn_id reconnect is the same connection: the old stream closing
// after the new one registered must leave the names alone.
func TestSameConnIDReconnectKeepsReferenceNames(t *testing.T) {
	h, _ := newRoutingHost(t)
	seen := captureReferences(t, h)
	oldClient, _ := connectBrowser(h, "chrome", chromeBundle)
	postReferenceNames(t, h, "chrome", []string{"alpha"})
	connectBrowser(h, "chrome", chromeBundle)
	pushesBefore := seen.pushes

	h.disconnectSSEClient("browser", "chrome", oldClient)

	if want := []string{"alpha"}; !reflect.DeepEqual(seen.names, want) {
		t.Errorf("refs collection = %v, want %v", seen.names, want)
	}
	if seen.pushes != pushesBefore {
		t.Errorf("the old stream's close republished the references group (%d pushes, want %d)", seen.pushes, pushesBefore)
	}
	if got := h.referenceNamesUnion(); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("stored union = %v, want [alpha]", got)
	}
}

// An unstamped post would share one set across browsers: refused.
func TestReferenceNamesPostRequiresConnID(t *testing.T) {
	h, _ := newRoutingHost(t)
	seen := captureReferences(t, h)
	if code := postReferenceNames(t, h, "", []string{"alpha"}); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if seen.pushes != 0 || len(h.referenceNamesUnion()) != 0 {
		t.Error("a refused post changed the reference names")
	}
}
