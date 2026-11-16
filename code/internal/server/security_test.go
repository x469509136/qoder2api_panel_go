package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// These tests pin the two credential-exposure fixes made on 2026-11-16.
//
// Both bugs shared a root cause: an authorization decision was made from
// "is checking enabled?" instead of "is this caller identified?". They are
// cheap to reintroduce, so each is asserted from the wire.

// TestAccountsExportHidesTokensByDefault pins that a bare GET never hands out
// access/refresh/personal tokens, and that ?secrets=1 still does (the panel's
// export button needs the tokens to migrate an account between instances).
func TestAccountsExportHidesTokensByDefault(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	_, httpSrv, _ := newTestServer(t, up)
	token := panelToken(t, httpSrv.URL)

	t.Run("no secrets", func(t *testing.T) {
		body := panelGet(t, httpSrv.URL+"/accounts/export", token)
		accounts, _ := body["accounts"].([]any)
		if len(accounts) != 1 {
			t.Fatalf("got %d accounts, want 1", len(accounts))
		}
		row, _ := accounts[0].(map[string]any)
		for _, key := range []string{"accessToken", "refreshToken", "personalToken"} {
			if _, present := row[key]; present {
				t.Errorf("bare export leaked %q", key)
			}
		}
		// Non-secret metadata must survive, or the file cannot be re-imported.
		if row["uid"] != "test-uid-0001" {
			t.Errorf("uid missing from export: %v", row["uid"])
		}
		if row["nickname"] != "tester" {
			t.Errorf("nickname missing from export: %v", row["nickname"])
		}
	})

	t.Run("secrets=1", func(t *testing.T) {
		body := panelGet(t, httpSrv.URL+"/accounts/export?secrets=1", token)
		accounts, _ := body["accounts"].([]any)
		if len(accounts) != 1 {
			t.Fatalf("got %d accounts, want 1", len(accounts))
		}
		row, _ := accounts[0].(map[string]any)
		if row["accessToken"] != "dt-test-token" {
			t.Errorf("accessToken = %v, want dt-test-token", row["accessToken"])
		}
		if row["refreshToken"] != "drt-test-refresh" {
			t.Errorf("refreshToken = %v, want drt-test-refresh", row["refreshToken"])
		}
	})

	t.Run("document shape", func(t *testing.T) {
		body := panelGet(t, httpSrv.URL+"/accounts/export", token)
		if body["format"] != "qoder-accounts" {
			t.Errorf("format = %v, want qoder-accounts", body["format"])
		}
		if body["version"] != float64(1) {
			t.Errorf("version = %v, want 1", body["version"])
		}
		if body["count"] != float64(1) {
			t.Errorf("count = %v, want 1", body["count"])
		}
		if body["exportedAt"] == nil {
			t.Error("exportedAt missing")
		}
	})

	t.Run("uid filter", func(t *testing.T) {
		body := panelGet(t, httpSrv.URL+"/accounts/export?uid=someone-else", token)
		if body["count"] != float64(0) {
			t.Errorf("uid filter ignored: count = %v", body["count"])
		}
	})
}

// TestHealthHidesIdentityFromRemoteCallers pins that the account uid is only
// shown to an identified caller.
//
// The server here has no API key configured — exactly the leaking case, where
// "no key configured" used to be read as "everyone is authenticated".
//
// The request is driven through the handler directly so RemoteAddr can name a
// non-loopback address: httptest's client always connects over loopback, and
// a loopback caller is legitimately allowed to see the uid, so going over the
// wire would make this test pass for the wrong reason.
func TestHealthHidesIdentityFromRemoteCallers(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	srv, httpSrv, _ := newTestServer(t, up)
	handler := srv.Handler()

	remoteURL, err := url.Parse("/health")
	if err != nil {
		t.Fatal(err)
	}

	anon := &http.Request{
		Method:     http.MethodGet,
		URL:        remoteURL,
		RemoteAddr: "203.0.113.9:5555",
		Header:     http.Header{},
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, anon)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous /health: %d, want 200", rec.Code)
	}
	var anonBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anonBody); err != nil {
		t.Fatalf("health is not JSON: %v", err)
	}
	if anonBody["ok"] != true {
		t.Errorf("ok = %v, want true (health must stay usable anonymously)", anonBody["ok"])
	}
	for _, key := range []string{"uid", "domain", "issuer", "expires_at"} {
		if _, present := anonBody[key]; present {
			t.Errorf("anonymous /health leaked %q", key)
		}
	}
	for _, key := range []string{"version", "realm", "accounts", "accounts_ready"} {
		if _, present := anonBody[key]; !present {
			t.Errorf("anonymous /health lost %q", key)
		}
	}

	// A panel session on the same remote address sees the identity again.
	token := panelToken(t, httpSrv.URL)
	identified := &http.Request{
		Method:     http.MethodGet,
		URL:        remoteURL,
		RemoteAddr: "203.0.113.9:5555",
		Header:     http.Header{"X-Panel-Token": []string{token}},
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, identified)
	var identifiedBody map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &identifiedBody); err != nil {
		t.Fatalf("identified health is not JSON: %v", err)
	}
	if identifiedBody["uid"] != "test-uid-0001" {
		t.Errorf("uid = %v, want test-uid-0001", identifiedBody["uid"])
	}
}

// panelToken logs in with the default panel password and returns a token.
func panelToken(t *testing.T, base string) string {
	t.Helper()
	resp := postJSON(t, base+"/panel/login", map[string]any{"password": "admin"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("panel login: %d %.200s", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("panel login decode: %v", err)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("panel login returned no token")
	}
	return token
}

// panelGet fetches a panel endpoint and decodes the JSON object.
func panelGet(t *testing.T, rawURL, token string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %.300s", rawURL, resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("GET %s is not JSON: %v (%.200s)", rawURL, err, raw)
	}
	return body
}
