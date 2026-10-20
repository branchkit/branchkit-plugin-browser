package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/branchkit/branchkit-plugin-browser/wire"
)

// Every route on the extension listener sits behind the pairing token (the
// event stream behind a ticket minted with it). The mux is built from
// wire.Routes, so the table is the list to check: a route that answers
// without the token answers any local process — and, past the Host check,
// any web page.

// registeredRoutes is every token-authenticated route's mux pattern.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	var routes []string
	for _, r := range wire.Routes {
		if r.Auth == wire.AuthToken {
			routes = append(routes, r.Key())
		}
	}
	sort.Strings(routes)
	return routes
}

func TestEveryExtensionRouteRequiresTheToken(t *testing.T) {
	routes := registeredRoutes(t)
	// A parser that found nothing would pass vacuously.
	if len(routes) < 30 {
		t.Fatalf("found only %d routes: %v", len(routes), routes)
	}

	// A dev-build app-support folder, so the dev-only routes register too.
	appSupport := filepath.Join(t.TempDir(), devAppSupportName)
	if err := os.MkdirAll(appSupport, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRANCHKIT_APP_SUPPORT", appSupport)
	h, _ := newRoutingHost(t)
	mux := h.extensionMux(listenerTestToken)

	for _, pattern := range routes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok || !strings.HasPrefix(path, "/") {
			t.Errorf("route %q has no method: it would answer any verb", pattern)
			continue
		}
		// No header, a wrong token, and the right token without "Bearer ".
		// The valid form is never sent, so no handler body runs here.
		for _, auth := range []string{"", "Bearer wrong-token", listenerTestToken} {
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			// The request must reach this route, not a 404/405 fallback.
			if _, got := mux.Handler(req); got != pattern {
				t.Errorf("%s: request routed to %q — is it registered on the extension mux?", pattern, got)
				break
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Errorf("%s with Authorization %q: status %d, want 401/403", pattern, auth, rec.Code)
			}
		}
	}
}
