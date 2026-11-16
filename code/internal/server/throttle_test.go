package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPanelLoginLockout pins the brute-force protection.
//
// The panel password defaults to "admin"; before this throttle a LAN script
// could try thousands of passwords per second and then call
// /accounts/export?secrets=1 for the whole credential set.
func TestPanelLoginLockout(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	srv, httpSrv, _ := newTestServer(t, up)
	_ = srv

	login := func(password string) *http.Response {
		t.Helper()
		return postJSON(t, httpSrv.URL+"/panel/login", map[string]any{"password": password})
	}

	// The first five wrong guesses are answered 401; the sixth is refused.
	for i := 0; i < loginFailureLimit; i++ {
		resp := login("wrong-" + itoa(i))
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, resp.StatusCode)
		}
	}

	resp := login("wrong-one-more")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: status = %d, want 429", loginFailureLimit+1, resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 is missing Retry-After")
	}

	// Even the correct password is refused while locked out: otherwise an
	// attacker learns the answer from a 200 during the cooldown.
	resp2 := login("admin")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("correct password during lockout: status = %d, want 429", resp2.StatusCode)
	}
}

// TestPanelLoginLockoutIsPerClient pins that one noisy client cannot lock out
// everybody else.
func TestPanelLoginLockoutIsPerClient(t *testing.T) {
	up := &fakeUpstream{t: t, inner: []string{chunk("ok", "", "stop")}}
	srv, _, _ := newTestServer(t, up)
	handler := srv.Handler()

	do := func(remote, password string) int {
		t.Helper()
		body := strings.NewReader(`{"password":"` + password + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/panel/login", body)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 0; i < loginFailureLimit+2; i++ {
		do("198.51.100.7:1000", "nope")
	}
	if code := do("198.51.100.7:1000", "admin"); code != http.StatusTooManyRequests {
		t.Fatalf("locked-out client: %d, want 429", code)
	}
	// A different IP is unaffected.
	if code := do("198.51.100.8:1000", "admin"); code != http.StatusOK {
		t.Fatalf("other client: %d, want 200", code)
	}
}

// TestThrottleBackoffGrows pins the cooldown schedule without sleeping.
func TestThrottleBackoffGrows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := newLoginThrottle()
	tr.now = func() time.Time { return now }

	// Under the limit: no cooldown.
	for i := 1; i < tr.failures; i++ {
		if wait := tr.fail("10.0.0.1"); wait != 0 {
			t.Fatalf("failure %d produced a cooldown of %s, want none", i, wait)
		}
	}
	first := tr.fail("10.0.0.1")
	if first != loginLockout {
		t.Fatalf("first lockout = %s, want %s", first, loginLockout)
	}
	second := tr.fail("10.0.0.1")
	if second <= first {
		t.Fatalf("cooldown did not grow: %s then %s", first, second)
	}
	// Growth is capped.
	for i := 0; i < 20; i++ {
		tr.fail("10.0.0.1")
	}
	if ok, wait := tr.allowed("10.0.0.1"); ok || wait > loginLockoutMax {
		t.Fatalf("cap broken: allowed=%v wait=%s max=%s", ok, wait, loginLockoutMax)
	}

	// A success clears the counter.
	tr.succeed("10.0.0.1")
	if ok, _ := tr.allowed("10.0.0.1"); !ok {
		t.Fatal("success did not clear the lockout")
	}
}

// TestThrottleExpiresAfterCooldown pins recovery, so a legitimate operator is
// not locked out forever after a typo streak.
func TestThrottleExpiresAfterCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := newLoginThrottle()
	tr.now = func() time.Time { return now }

	for i := 0; i <= tr.failures; i++ {
		tr.fail("10.0.0.2")
	}
	if ok, _ := tr.allowed("10.0.0.2"); ok {
		t.Fatal("client should be locked out")
	}
	now = now.Add(loginLockoutMax + time.Second)
	if ok, _ := tr.allowed("10.0.0.2"); !ok {
		t.Fatal("client did not recover after the cooldown")
	}
}
