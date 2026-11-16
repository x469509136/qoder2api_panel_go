package accounts_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xzs/bridge_qoder/internal/accounts"
)

// TestCheckinFallsBackToLegacyWhenCampaignsFail pins the fallback boundary: the
// legacy endpoint is only consulted when the campaign list could not be
// fetched. If the campaign leg answered (even with "nothing to claim"), a second
// claim attempt against the legacy API would be contradictory and, worse, could
// double-claim on an account where both mechanisms are live.
func TestCheckinFallsBackToLegacyWhenCampaignsFail(t *testing.T) {
	var legacyHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sash/api/v1/me/campaigns":
			// Simulate an upstream outage, not an empty list: a 500 must not be
			// retried and must fall through.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		case r.URL.Path == "/sash/api/v1/me/daily-check-in/status":
			atomic.AddInt32(&legacyHits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "CLAIMABLE", "currentStreakDays": 3,
			})
		case r.URL.Path == "/sash/api/v1/me/daily-check-in/claim":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "rewardCredits": 50})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if atomic.LoadInt32(&legacyHits) == 0 {
		t.Fatal("legacy status endpoint was never consulted after a campaign outage")
	}
	if !res.OK {
		t.Fatalf("legacy fallback should succeed here: %+v", res)
	}
	if res.RewardCredits != 50 {
		t.Errorf("RewardCredits = %d, want 50 from the legacy claim", res.RewardCredits)
	}
}

// TestCheckinDoesNotConsultLegacyWhenCampaignsAnswer pins the other half: a
// successful campaign answer (including "nothing to claim") must not touch the
// legacy endpoint at all.
func TestCheckinDoesNotConsultLegacyWhenCampaignsAnswer(t *testing.T) {
	var legacyHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"showCampaign": true,
				"campaigns": []any{map[string]any{
					"campaign_id": "c1", "claim_status": "CLAIMED",
					"actionType": "CLAIM_BENEFIT",
					"benefit":    map[string]any{"kind": "CREDITS", "amount": 100},
				}},
			})
		case "/sash/api/v1/me/daily-check-in/status",
			"/sash/api/v1/me/daily-check-in/claim":
			// The legacy check-in endpoint is the only thing this test counts.
			atomic.AddInt32(&legacyHits, 1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			// The balance refresh probes several quota paths. Those are
			// unrelated to the check-in decision and must not be counted;
			// 404 is their normal "not this endpoint" answer.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if !res.OK {
		t.Fatalf("check-in failed: %+v", res)
	}
	if n := atomic.LoadInt32(&legacyHits); n != 0 {
		t.Errorf("legacy check-in consulted %d times despite a valid campaign answer", n)
	}
}
