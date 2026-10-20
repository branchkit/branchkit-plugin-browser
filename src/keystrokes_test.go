package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The open_select follow-up: the extension focused a <select>
// and asks the plugin to press the OS keystroke that opens its picker. The
// keystroke goes to whatever app has focus, so it may only be pressed while
// the asking connection is the focused browser. No test presses a real key.

type pressRecorder struct {
	mu      sync.Mutex
	pressed []osShortcut
}

func (r *pressRecorder) press(s osShortcut) error {
	r.mu.Lock()
	r.pressed = append(r.pressed, s)
	r.mu.Unlock()
	return nil
}

func (r *pressRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pressed)
}

func newKeystrokeHost(t *testing.T) (*Host, *pressRecorder) {
	t.Helper()
	h, _ := newRoutingHost(t)
	rec := &pressRecorder{}
	h.pressShortcut = rec.press
	h.runAsync = func(f func()) { f() } // decide and press inline
	connectBrowser(h, "chrome", chromeBundle)
	connectBrowser(h, "fox", firefoxBundle)
	focusApp(h, chromeBundle)
	return h, rec
}

func postDispatchResult(t *testing.T, h *Host, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dispatch-result", strings.NewReader(body))
	w := httptest.NewRecorder()
	serveRoute(h, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/dispatch-result status = %d", w.Code)
	}
}

func TestOpenSelectPressedForFocusedConn(t *testing.T) {
	h, rec := newKeystrokeHost(t)
	postDispatchResult(t, h, `{"action":"activate","ok":true,"taken":"focus","followup":"open_select","conn_id":"chrome"}`)
	if rec.count() != 1 {
		t.Fatalf("pressed %d keystroke(s), want 1", rec.count())
	}
	want := shortcutsFor("darwin").openSelect
	got := rec.pressed[0]
	if got.Name != want.Name || strings.Join(got.Modifiers, "+") != strings.Join(want.Modifiers, "+") {
		t.Errorf("pressed %+v, want %+v", got, want)
	}
}

func TestOpenSelectNotPressedUnlessFocusedConnAsks(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		setup func(h *Host)
	}{
		{"browser not in front", `{"followup":"open_select","conn_id":"chrome"}`,
			func(h *Host) { focusApp(h, "com.apple.Terminal") }},
		{"another browser asks", `{"followup":"open_select","conn_id":"fox"}`, nil},
		{"unknown connection", `{"followup":"open_select","conn_id":"ghost"}`, nil},
		{"no followup", `{"action":"activate","ok":true,"conn_id":"chrome"}`, nil},
		{"unknown followup", `{"followup":"open_everything","conn_id":"chrome"}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, rec := newKeystrokeHost(t)
			if c.setup != nil {
				c.setup(h)
			}
			postDispatchResult(t, h, c.body)
			if rec.count() != 0 {
				t.Errorf("pressed %v; a keystroke must only go to the focused browser that asked", rec.pressed)
			}
		})
	}
}

// The keystroke table is per OS; every OS must name a key the platform's
// key-name table knows (actions.rs map_key_name).
func TestShortcutTableNamesKnownKeys(t *testing.T) {
	known := map[string]bool{"down": true, "alt": true, "cmd": true, "ctrl": true, "space": true, "l": true}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		table := shortcutsFor(goos)
		for label, s := range map[string]osShortcut{"openSelect": table.openSelect, "addressBar": table.addressBar} {
			if !known[s.Name] {
				t.Errorf("%s %s key %q is not in the platform's key-name table", goos, label, s.Name)
			}
			for _, m := range s.Modifiers {
				if !known[m] {
					t.Errorf("%s %s modifier %q is not in the platform's key-name table", goos, label, m)
				}
			}
		}
	}
}

// Off macOS "cmd" is the Meta key, and Meta+L locks Windows and GNOME: the
// address-bar shortcut must use Ctrl there.
func TestAddressBarShortcutPerOS(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "cmd", "linux": "ctrl", "windows": "ctrl"} {
		s := shortcutsFor(goos).addressBar
		if s.Name != "l" || len(s.Modifiers) != 1 || s.Modifiers[0] != want {
			t.Errorf("%s address bar = %v+%s, want %s+l", goos, s.Modifiers, s.Name, want)
		}
	}
}

// focus_address_bar presses the current OS's table entry, never forwarding.
func TestFocusAddressBarPressesTableShortcut(t *testing.T) {
	h, rec := newKeystrokeHost(t)
	dispatch(h, "focus_address_bar")
	if rec.count() != 1 {
		t.Fatalf("pressed %d keystroke(s), want 1", rec.count())
	}
	want := shortcutsFor(runtime.GOOS).addressBar
	if got := rec.pressed[0]; got.Name != want.Name || strings.Join(got.Modifiers, "+") != strings.Join(want.Modifiers, "+") {
		t.Errorf("pressed %+v, want %+v", got, want)
	}
}
