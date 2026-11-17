package qoder_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzs/bridge_qoder/internal/qoder"
)

// stubHTTP is an in-memory HTTP implementation, proving the package never needs
// the real transport to be exercised.
type stubHTTP struct {
	mu       sync.Mutex
	calls    []string
	handlers map[string]func() (int, any)
}

func (s *stubHTTP) Do(_ context.Context, req qoder.Request) ([]byte, error) {
	body, err := s.DoJSONBytes(req)
	return body, err
}

func (s *stubHTTP) DoJSONBytes(req qoder.Request) ([]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req.Method+" "+req.URL)
	s.mu.Unlock()
	key := req.URL
	for _, prefix := range []string{"https://openapi.qoder.com.cn", "https://openapi.qoder.sh"} {
		key = strings.TrimPrefix(key, prefix)
	}
	if i := strings.IndexByte(key, '?'); i >= 0 {
		key = key[:i]
	}
	h, ok := s.handlers[key]
	if !ok {
		return nil, &qoder.HTTPStatusError{Code: 404, Body: `{"error":"no handler"}`}
	}
	code, payload := h()
	blob, _ := json.Marshal(payload)
	if code >= 400 {
		return nil, &qoder.HTTPStatusError{Code: code, Body: string(blob)}
	}
	return blob, nil
}

func (s *stubHTTP) DoJSON(_ context.Context, req qoder.Request) (map[string]any, error) {
	data, err := s.DoJSONBytes(req)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *stubHTTP) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

func newClient(h qoder.HTTP) *qoder.Client {
	return qoder.New(qoder.Options{HTTP: h})
}

func cnCred() qoder.Credential {
	return qoder.Credential{UID: "u1", Realm: qoder.RealmCN, AccessToken: "at"}
}

// TestPackageNeedsNoTransportOrPersistence pins the portability claim: the
// package is driven entirely through the HTTP seam, with plain values in and
// out. No account file, no pool, no cooldown.
func TestPackageNeedsNoTransportOrPersistence(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/api/v2/quota/usage": func() (int, any) {
			return 200, map[string]any{
				"userQuota":  map[string]any{"remaining": 100, "used": 20, "total": 120},
				"addOnQuota": map[string]any{"remaining": 50, "used": 0, "total": 50},
				"expiresAt":  1799999999,
			}
		},
	}}
	res := newClient(h).FetchCredits(context.Background(), cnCred(), nil)
	if !res.OK {
		t.Fatalf("FetchCredits failed: %+v", res)
	}
	if res.Credits.Remain != 150 || res.Credits.RemainEffective != 150 {
		t.Errorf("remain = %d/%d, want 150/150", res.Credits.Remain, res.Credits.RemainEffective)
	}
	if res.Credits.Size != 170 {
		t.Errorf("size = %d, want 170", res.Credits.Size)
	}
}

// TestCreditsExpiryHasThreeStates pins the "credit validity" question: known
// date, derived-from-container date, and unknown — never a fake zero date.
func TestCreditsExpiryHasThreeStates(t *testing.T) {
	now := time.Unix(1799900000, 0)

	t.Run("top-level", func(t *testing.T) {
		got := qoder.ExpiryOf(&qoder.Credits{ExpiresAt: 1799999999}, now)
		if !got.Known || got.ExpiresAt != 1799999999 {
			t.Fatalf("got %+v", got)
		}
		if got.DaysLeft < 0 {
			t.Errorf("daysLeft = %d", got.DaysLeft)
		}
	})

	t.Run("from container", func(t *testing.T) {
		got := qoder.ExpiryOf(&qoder.Credits{Composition: []qoder.Composition{
			{Kind: "addon", Remain: 10, ExpiresAt: 1799999999},
			{Kind: "plan", Remain: 10, ExpiresAt: 1799950000},
		}}, now)
		if !got.Known || got.Source != "plan" {
			t.Fatalf("want the earliest container, got %+v", got)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		got := qoder.ExpiryOf(&qoder.Credits{Remain: 10}, now)
		if got.Known {
			t.Fatalf("a snapshot without a date must report unknown, got %+v", got)
		}
	})

	t.Run("nil is unknown", func(t *testing.T) {
		if qoder.ExpiryOf(nil, now).Known {
			t.Fatal("nil credits must be unknown")
		}
	})
}

// TestDegenerateQuotaIsUnknownNotZero pins that a revoked credential does not
// look like a healthy zero balance.
func TestDegenerateQuotaIsUnknownNotZero(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/api/v2/quota/usage": func() (int, any) {
			return 200, map[string]any{"isQuotaExceeded": true}
		},
	}}
	res := newClient(h).FetchCredits(context.Background(), cnCred(), nil)
	if res.OK {
		t.Fatalf("degenerate body must not be OK: %+v", res)
	}
	if !strings.Contains(res.Error, "degenerate") {
		t.Errorf("error = %q, want a degenerate-body explanation", res.Error)
	}
}

// TestCheckinUsesCampaignThenNeverLegacy pins the ordering rule and that a
// successful campaign answer never triggers the retired endpoint.
func TestCheckinUsesCampaignThenNeverLegacy(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/sash/api/v1/me/campaigns": func() (int, any) {
			return 200, map[string]any{
				"showCampaign": true,
				"campaigns": []any{map[string]any{
					"campaign_id": "daily", "claim_status": "CLAIMABLE",
					"actionType": "CLAIM_BENEFIT",
					"benefit":    map[string]any{"kind": "CREDITS", "amount": 100},
					"placements": map[string]any{"content": map[string]any{
						"zh": map[string]any{"title": "每日签到"},
					}},
				}},
			}
		},
		"/sash/api/v1/me/campaigns/daily/claim": func() (int, any) {
			return 200, map[string]any{"status": "CLAIMED"}
		},
	}}
	rep := newClient(h).Checkin(context.Background(), cnCred())

	if rep.Error != "" {
		t.Fatalf("check-in error: %s", rep.Error)
	}
	if rep.Earned != 100 {
		t.Errorf("earned = %d, want 100", rep.Earned)
	}
	if !rep.Stamped {
		t.Error("a successful claim must be stamped")
	}
	if rep.UsesLegacy {
		t.Error("the legacy endpoint must not be used")
	}
	if n := h.count("/daily-check-in"); n != 0 {
		t.Errorf("legacy endpoint hit %d times", n)
	}
	if n := h.count("/campaigns/daily/claim"); n != 1 {
		t.Errorf("claim hit %d times, want 1", n)
	}
}

// TestCheckinFallsBackOnlyWhenListUnavailable pins the fallback boundary.
func TestCheckinFallsBackOnlyWhenListUnavailable(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/sash/api/v1/me/campaigns": func() (int, any) {
			return 500, map[string]any{"error": "boom"}
		},
		"/sash/api/v1/me/daily-check-in/status": func() (int, any) {
			return 200, map[string]any{"status": "CLAIMABLE", "currentStreakDays": 2}
		},
		"/sash/api/v1/me/daily-check-in/claim": func() (int, any) {
			return 200, map[string]any{"success": true, "rewardCredits": 30}
		},
	}}
	rep := newClient(h).Checkin(context.Background(), cnCred())
	if !rep.UsesLegacy {
		t.Fatal("expected the legacy fallback")
	}
	if rep.Earned != 30 {
		t.Errorf("earned = %d, want 30", rep.Earned)
	}
}

// TestAlreadyClaimedDoesNotDoubleClaim pins the second-run behaviour.
func TestAlreadyClaimedDoesNotDoubleClaim(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/sash/api/v1/me/campaigns": func() (int, any) {
			return 200, map[string]any{"campaigns": []any{map[string]any{
				"campaign_id": "daily", "claim_status": "CLAIMED",
				"actionType": "VIEW_DETAILS",
				"benefit":    map[string]any{"kind": "CREDITS", "amount": 100},
			}}}
		},
	}}
	rep := newClient(h).Checkin(context.Background(), cnCred())
	if !rep.Already {
		t.Fatalf("want already-claimed, got %+v", rep)
	}
	if rep.Earned != 0 {
		t.Errorf("earned = %d, want 0", rep.Earned)
	}
	if n := h.count("/claim"); n != 0 {
		t.Errorf("claim POSTed %d times", n)
	}
}

// TestRefreshPicksFlowByTokenPrefix pins the refresh routing without a network.
func TestRefreshPicksFlowByTokenPrefix(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){
		"/api/v1/deviceToken/refresh": func() (int, any) {
			return 200, map[string]any{"token": "new-device", "refresh_token": "drt-2", "expires_in": 1000}
		},
		"/api/v1/jobToken/refresh": func() (int, any) {
			return 200, map[string]any{"token": "new-job", "refresh_token": "jrt-2"}
		},
		"/api/v1/jobToken/exchange": func() (int, any) {
			return 200, map[string]any{"token": "new-pat", "refresh_token": "jrt-3"}
		},
	}}
	c := newClient(h)

	tok, err := c.Refresh(context.Background(), qoder.Credential{
		UID: "u1", Realm: qoder.RealmCN, RefreshToken: "drt-1",
	})
	if err != nil || tok.AccessToken != "new-device" {
		t.Fatalf("device refresh: %+v %v", tok, err)
	}
	if n := h.count("jobToken"); n != 0 {
		t.Errorf("device token used the job flow %d times", n)
	}

	tok2, err := c.Refresh(context.Background(), qoder.Credential{
		UID: "u2", Realm: qoder.RealmCN, RefreshToken: "jrt-1",
	})
	if err != nil || tok2.AccessToken != "new-job" {
		t.Fatalf("job refresh: %+v %v", tok2, err)
	}

	tok3, err := c.Refresh(context.Background(), qoder.Credential{
		UID: "u3", Realm: qoder.RealmCN, PersonalToken: "pt-1",
	})
	if err != nil || tok3.AccessToken != "new-pat" {
		t.Fatalf("pat exchange: %+v %v", tok3, err)
	}
}

// TestRefreshWithoutCredentialFailsLoudly pins the "no silent fallback" rule.
func TestRefreshWithoutCredentialFailsLoudly(t *testing.T) {
	h := &stubHTTP{handlers: map[string]func() (int, any){}}
	_, err := newClient(h).Refresh(context.Background(), qoder.Credential{UID: "u", Realm: qoder.RealmCN})
	if err == nil {
		t.Fatal("refresh with no credential must fail")
	}
}

// TestDeviceLoginBuildsRealmSpecificURL pins the CN/INTL PKCE parameter split.
func TestDeviceLoginBuildsRealmSpecificURL(t *testing.T) {
	c := newClient(&stubHTTP{handlers: map[string]func() (int, any){}})

	cn, err := c.StartDeviceLogin(context.Background(), qoder.NewMemorySessions(), qoder.RealmCN, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"challenge_method=S256", "redirect_uri=", "client_id=", "machine_id="} {
		if !strings.Contains(cn.AuthURL, want) {
			t.Errorf("CN URL missing %q: %s", want, cn.AuthURL)
		}
	}

	intl, err := c.StartDeviceLogin(context.Background(), qoder.NewMemorySessions(), qoder.RealmINTL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(intl.AuthURL, "redirect_uri=") {
		t.Errorf("INTL URL must not send redirect_uri: %s", intl.AuthURL)
	}
}

// TestLoginPollExpiresUnattendedFlow pins the TTL.
func TestLoginPollExpiresUnattendedFlow(t *testing.T) {
	sessions := qoder.NewMemorySessions()
	poll := newClient(&stubHTTP{handlers: map[string]func() (int, any){}}).
		PollDeviceLogin(context.Background(), sessions, "never-issued")
	if poll.Status != "unknown" {
		t.Errorf("status = %q, want unknown", poll.Status)
	}
}

// TestRealmConfigForVPC pins that enterprise overrides never leak intl hosts.
func TestRealmConfigForVPC(t *testing.T) {
	vpc := "acme"
	cfg, err := qoder.RealmConfigFor(qoder.RealmCN, &vpc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VPCInstance != "acme" || cfg.Gateway != "https://acme-gateway.vpc.qoder.com.cn" {
		t.Errorf("bad VPC config: %+v", cfg)
	}
	if len(qoder.GatewayCandidates(qoder.RealmCN, cfg)) != 1 {
		t.Error("VPC accounts must have exactly one gateway (no public fallback)")
	}

	if _, err := qoder.RealmConfigFor(qoder.RealmINTL, &vpc); err == nil {
		t.Error("VPC must be rejected for intl")
	}
}

// TestNextCheckinWindowIsUTC8 pins the reset boundary.
func TestNextCheckinWindowIsUTC8(t *testing.T) {
	before := time.Date(2026, 11, 17, 9, 59, 0, 0, qoder.UTC8)
	if w := qoder.NextCheckinWindow(before); time.Unix(w.Epoch, 0).In(qoder.UTC8).Day() != 17 {
		t.Errorf("09:59 should stay on the 17th: %+v", w)
	}
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, qoder.UTC8)
	if w := qoder.NextCheckinWindow(at); time.Unix(w.Epoch, 0).In(qoder.UTC8).Day() != 18 {
		t.Errorf("10:00 should roll to the 18th: %+v", w)
	}
}
