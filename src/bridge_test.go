package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- validateToken: Bearer-header auth for the extension listener ---

func TestValidateToken(t *testing.T) {
	const secret = "s3cret-token"

	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"valid bearer", "Bearer s3cret-token", true},
		{"wrong token", "Bearer wrong-token", false},
		{"no auth header", "", false},
		{"non-bearer scheme", "Basic s3cret-token", false},
		{"bearer prefix only", "Bearer ", false},
		{"raw token no scheme", "s3cret-token", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			if got := validateToken(req, secret); got != c.want {
				t.Errorf("validateToken(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// --- stream tickets: the only credential /events accepts, minted with the
// token in a header, one use, short window ---
func TestTicketIsOneUseAndShortLived(t *testing.T) {
	store := newTicketStore()
	tk, err := store.mint()
	if err != nil || tk == "" {
		t.Fatalf("mint: %v", err)
	}
	if !store.redeem(tk) {
		t.Fatal("a fresh ticket must redeem")
	}
	if store.redeem(tk) {
		t.Fatal("a ticket must redeem once")
	}
	if store.redeem("") || store.redeem("nope") {
		t.Fatal("an unknown or empty ticket must not redeem")
	}
	expired, _ := store.mint()
	store.mu.Lock()
	store.pending[expired] = time.Now().Add(-time.Second)
	store.mu.Unlock()
	if store.redeem(expired) {
		t.Fatal("an expired ticket must not redeem")
	}
}

func TestQueryTokenNoLongerAuthenticates(t *testing.T) {
	const secret = "s3cret-token"
	req := httptest.NewRequest(http.MethodGet, "/x?token="+secret, nil)
	if validateToken(req, secret) {
		t.Fatal("the pairing token must not be accepted from a URL")
	}
}

// --- corsMiddleware ---

func TestCorsMiddlewarePreflight(t *testing.T) {
	nextCalled := false
	h := corsMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		nextCalled = true
	}))

	// OPTIONS preflight short-circuits with 204 and never reaches next.
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
	if nextCalled {
		t.Error("preflight must not call the next handler")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("preflight must set the CORS allow-origin header")
	}
}

func TestCorsMiddlewarePassesThrough(t *testing.T) {
	nextCalled := false
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !nextCalled {
		t.Error("non-preflight request must reach the next handler")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS allow-origin header must be set on real requests too")
	}
}

// --- handleDebugLogPost request validation ---

func TestHandleDebugLogPostRejectsBadInput(t *testing.T) {
	h := newTestHost()
	h.state = &PluginState{PluginID: "browser"}

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", "not json"},
		{"missing tag", `{"data":{}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/debug-log", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			serveRoute(h, rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}
