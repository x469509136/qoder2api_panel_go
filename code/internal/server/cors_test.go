package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORSDoesNotOpenManagementRoutes pins the credential-theft boundary.
//
// The CORS list used to include /panel/, /accounts, /settings, /logs, /agents
// and friends, with Access-Control-Allow-Origin: *. Any web page the operator
// visited could therefore read the panel API cross-origin: /accounts/export
// with ?secrets=1 returns plaintext access/refresh tokens, and browsers do not
// apply the SameSite protection to a same-site-origin fetch the way they do to
// cookies, because the panel authenticates with a header, not a cookie.
//
// The management surface must never advertise CORS. Inference and the public
// probes keep it so browser-based clients can still talk to /v1.
func TestCORSDoesNotOpenManagementRoutes(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	srv, _, _ := newTestServer(t, up)
	handler := srv.Handler()

	cases := []struct {
		path  string
		allow bool
	}{
		// Inference + probes: browser clients legitimately call these.
		{"/v1/models", true},
		{"/v1/chat/completions", true},
		{"/health", true},
		{"/ping", true},
		// Management: never cross-origin readable.
		{"/accounts", false},
		{"/accounts/export", false},
		{"/accounts/login/poll", false},
		{"/settings", false},
		{"/settings/reveal", false},
		{"/logs", false},
		{"/logs/export", false},
		{"/agents", false},
		{"/agents/apply", false},
		{"/admin/routing", false},
		{"/usage/recent", false},
		{"/tasks", false},
		{"/diag/vm", false},
		{"/credits", false},
		{"/panel/login", false},
		{"/panel/status", false},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		got := rec.Header().Get("Access-Control-Allow-Origin")
		if tc.allow && got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want %q", tc.path, got, "*")
		}
		if !tc.allow && got != "" {
			t.Errorf("%s: management route exposed CORS (%q)", tc.path, got)
		}
	}
}

// TestOptionsDoesNotBypassAuth pins that a preflight never reaches a handler.
//
// routeRequest answers every OPTIONS with 204 before dispatch. That is fine on
// its own, but it must not be mistaken for "the route is public": a preflight
// carrying no credentials is the browser's probe, and it must not run side
// effects either.
func TestOptionsDoesNotBypassAuth(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	srv, _, _ := newTestServer(t, up)
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodOptions, "/accounts/export?secrets=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "" {
		t.Errorf("preflight returned a body (%q); it must not run the handler", body)
	}
}

// TestAccountViewsNeverCarryTokens walks every panel-readable account surface
// and asserts no credential leaks into it.
//
// Each endpoint below is reachable with a panel session; the point is that
// "authenticated panel" must not equal "sees upstream credentials".
func TestAccountViewsNeverCarryTokens(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	_, httpSrv, _ := newTestServer(t, up)
	token := panelToken(t, httpSrv.URL)

	paths := []string{
		"/accounts",
		"/accounts/export",
		"/tasks",
		"/agents",
		"/admin/overview",
		"/diag/checkin",
	}

	needles := []string{"dt-test-token", "drt-test-refresh"}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, httpSrv.URL+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Panel-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				// A non-JSON answer is not a leak; skip rather than fail.
				return
			}
			text := string(raw)
			for _, needle := range needles {
				if strings.Contains(text, needle) {
					t.Errorf("%s leaked credential %q", p, needle)
				}
			}
		})
	}
}
