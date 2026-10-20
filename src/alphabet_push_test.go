package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/branchkit/branchkit-plugin-browser/wire"
)

// streamRecorder is an http.ResponseWriter + Flusher safe to read while
// handleSSE writes to it from another goroutine (httptest.ResponseRecorder
// is not).
type streamRecorder struct {
	mu  sync.Mutex
	hdr http.Header
	buf bytes.Buffer
}

func (r *streamRecorder) Header() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *streamRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}
func (r *streamRecorder) WriteHeader(int) {}
func (r *streamRecorder) Flush()          {}

// alphabets returns every `alphabet` event's words, in stream order.
func (r *streamRecorder) alphabets(t *testing.T) [][]string {
	t.Helper()
	r.mu.Lock()
	text := r.buf.String()
	r.mu.Unlock()
	var out [][]string
	for _, frame := range strings.Split(text, "\n\n") {
		if !strings.HasPrefix(frame, "event: alphabet\n") {
			continue
		}
		var ev wire.AlphabetEvent
		data := strings.TrimPrefix(frame, "event: alphabet\ndata: ")
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("alphabet event data %q: %v", data, err)
		}
		out = append(out, ev.Words)
	}
	return out
}

func (r *streamRecorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(r.buf.String(), s)
}

// openTestStream runs handleSSE for conn until the returned cancel is called,
// and waits until the connect-time handshake (through `reactivate`) is written.
func openTestStream(t *testing.T, h *Host, conn string) (*streamRecorder, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events?conn_id="+conn, nil).WithContext(ctx)
	req.RemoteAddr = "" // no identity lookup goroutine
	rec := &streamRecorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handleSSE(rec, req, conn)
	}()
	waitUntil(t, func() bool { return rec.has(`"reactivate"`) })
	return rec, func() { cancel(); <-done }
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func retunedCodewords() [26]string {
	out := testCodewords
	out[0] = "air" // a calibration retune of letter a
	return out
}

// A mid-session alphabet change reaches every open stream as an `alphabet`
// event with the connect-time payload shape. Without it a connected extension
// keeps translating hint letters with the old words until it reconnects, and
// the plugin drops those codewords as codeword_not_in_alphabet.
func TestAlphabetChangeReachesEveryOpenStream(t *testing.T) {
	h, _ := newRoutingHost(t)
	chrome, closeChrome := openTestStream(t, h, "chrome")
	defer closeChrome()
	firefox, closeFirefox := openTestStream(t, h, "firefox")
	defer closeFirefox()

	for name, rec := range map[string]*streamRecorder{"chrome": chrome, "firefox": firefox} {
		if got := rec.alphabets(t); len(got) != 1 || got[0][0] != "arch" {
			t.Fatalf("%s: connect-time alphabet = %v, want one with the current words", name, got)
		}
	}

	want := retunedCodewords()
	h.applyAlphabet(want)

	for name, rec := range map[string]*streamRecorder{"chrome": chrome, "firefox": firefox} {
		waitUntil(t, func() bool { return len(rec.alphabets(t)) == 2 })
		got := rec.alphabets(t)[1]
		if strings.Join(got, ",") != strings.Join(want[:], ",") {
			t.Fatalf("%s: pushed alphabet = %v, want %v", name, got, want)
		}
	}
	if got := h.snapshotCodewords(); got[0] != "air" {
		t.Fatalf("plugin's own codewords = %v, want the new alphabet", got)
	}
}

// The mirror's OnChange fires on refreshes that change nothing; those must not
// reach the extension (each distinct push re-sends every frame's grammar).
func TestUnchangedAlphabetIsNotRepushed(t *testing.T) {
	h, _ := newRoutingHost(t)
	rec, closeStream := openTestStream(t, h, "chrome")
	defer closeStream()

	h.applyAlphabet(testCodewords) // same words the stream connected with
	h.applyAlphabet(retunedCodewords())
	waitUntil(t, func() bool { return len(rec.alphabets(t)) == 2 })
	h.applyAlphabet(retunedCodewords())
	time.Sleep(20 * time.Millisecond)
	if got := rec.alphabets(t); len(got) != 2 {
		t.Fatalf("alphabet events = %d, want 2 (connect + the one real change)", len(got))
	}
}

// A half-loaded alphabet (blank slots) is never pushed, as at connect.
func TestIncompleteAlphabetIsNotPushed(t *testing.T) {
	h, _ := newRoutingHost(t)
	rec, closeStream := openTestStream(t, h, "chrome")
	defer closeStream()

	partial := retunedCodewords()
	partial[25] = ""
	h.applyAlphabet(partial)
	time.Sleep(20 * time.Millisecond)
	if got := rec.alphabets(t); len(got) != 1 {
		t.Fatalf("alphabet events = %d, want only the connect-time one", len(got))
	}
}

// A stream that connected before the alphabet loaded got no alphabet at
// connect; the first load must reach it.
func TestFirstAlphabetLoadReachesEarlyStream(t *testing.T) {
	h, _ := newRoutingHost(t)
	h.codewords = [26]string{}
	rec, closeStream := openTestStream(t, h, "chrome")
	defer closeStream()
	if got := rec.alphabets(t); len(got) != 0 {
		t.Fatalf("connect-time alphabet with nothing loaded = %v", got)
	}
	h.applyAlphabet(testCodewords)
	waitUntil(t, func() bool { return len(rec.alphabets(t)) == 1 })
}

// A stream that has ended no longer receives alphabet pushes, and its
// registration goes with it.
func TestAlphabetPushSkipsClosedStream(t *testing.T) {
	h, _ := newRoutingHost(t)
	_, closeStream := openTestStream(t, h, "chrome")
	closeStream()
	h.applyAlphabet(retunedCodewords()) // must not block or panic
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.state.SSEAlphabet); n != 0 {
		t.Fatalf("SSEAlphabet holds %d entries after the stream closed", n)
	}
}
