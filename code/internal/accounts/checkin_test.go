package accounts_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
)

// fakeCampaigns is a stand-in for the official campaign API.
type fakeCampaigns struct {
	mu sync.Mutex

	// campaigns is the list returned by GET /sash/api/v1/me/campaigns.
	campaigns []map[string]any
	// claims counts POST /campaigns/{id}/claim per campaign id.
	claims map[string]int
	// claimBody overrides the claim response per campaign id.
	claimBody map[string]map[string]any
	// listCalls counts campaign-list requests.
	listCalls int
	// emptyFirst makes the first N list calls return an empty list, to pin the
	// retry behaviour the Python original has (issue #130).
	emptyFirst int
}

func (f *fakeCampaigns) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	if strings.HasSuffix(r.URL.Path, "/campaigns") && r.Method == http.MethodGet {
		f.listCalls++
		if f.listCalls <= f.emptyFirst {
			_ = json.NewEncoder(w).Encode(map[string]any{"campaigns": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"showCampaign": true,
			"campaigns":    f.campaigns,
		})
		return
	}

	if strings.Contains(r.URL.Path, "/claim") {
		id := pathCampaignID(r.URL.Path)
		f.claims[id]++
		if body, ok := f.claimBody[id]; ok {
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "CLAIMED"})
		return
	}

	// The legacy daily check-in endpoint: answering 404 proves the campaign
	// path did not fall through to it.
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func pathCampaignID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "campaigns" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// withRealmServer points one realm at srv for the duration of the test.
func withRealmServer(t *testing.T, realm string, srv *httptest.Server) {
	t.Helper()
	orig := accounts.RealmConfigs[realm]
	cfg := orig
	cfg.OpenAPI = srv.URL
	cfg.VPCInstance = ""
	accounts.RealmConfigs[realm] = cfg
	t.Cleanup(func() { accounts.RealmConfigs[realm] = orig })
}

func newTestAccount(t *testing.T) *accounts.Account {
	t.Helper()
	acc, err := accounts.NewAccount(map[string]any{
		"uid": "checkin-uid", "nickname": "tester", "realm": accounts.RealmCN,
		"domain": "qoder.com.cn", "accessToken": "dt-test", "enabled": true,
	}, "")
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	return acc
}

// TestCheckinClaimsViaCampaignPlatform pins the actual bug: the daily check-in
// must go through the campaign platform, which is the mechanism the official
// client uses now. Before this fix it called only the retired /sash endpoint and
// every account reported "endpoint not open", so nothing was ever claimed.
func TestCheckinClaimsViaCampaignPlatform(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{{
			"campaign_id":  "daily-credits",
			"campaign_key": "daily_checkin",
			"claim_status": "CLAIMABLE",
			"actionType":   "CLAIM_BENEFIT",
			"benefit":      map[string]any{"kind": "CREDITS", "amount": 100},
			"placements": map[string]any{"content": map[string]any{
				"zh": map[string]any{"title": "每日签到"},
			}},
		}},
		claims:    map[string]int{},
		claimBody: map[string]map[string]any{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if !res.OK {
		t.Fatalf("check-in failed: %+v", res)
	}
	if res.RewardCredits != 100 {
		t.Errorf("RewardCredits = %d, want 100", res.RewardCredits)
	}
	if res.Campaign == nil {
		t.Fatal("campaign detail missing; the legacy path was probably taken")
	}
	if len(res.Campaign.Claimed) != 1 || res.Campaign.Claimed[0] != "每日签到" {
		t.Errorf("Claimed = %v, want [每日签到]", res.Campaign.Claimed)
	}
	if fake.claims["daily-credits"] != 1 {
		t.Errorf("claim requests = %d, want exactly 1", fake.claims["daily-credits"])
	}
	// The local stamp drives the scheduler and the panel, so it must move.
	if acc.CanCheckin() {
		t.Error("account was not stamped after a successful check-in")
	}
}

// TestCheckinDoesNotDoubleClaim pins that a second run the same day does not
// POST again. Re-claiming is harmless upstream (it replays) but it burns a
// request per account per scheduler cycle.
func TestCheckinDoesNotDoubleClaim(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{{
			"campaign_id": "daily-credits", "claim_status": "CLAIMED",
			"actionType": "VIEW_DETAILS",
			"benefit":    map[string]any{"kind": "CREDITS", "amount": 100},
		}},
		claims: map[string]int{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if !res.OK || !res.Already {
		t.Fatalf("want ok+already, got %+v", res)
	}
	if len(fake.claims) != 0 {
		t.Errorf("already-claimed campaign was POSTed again: %v", fake.claims)
	}
}

// TestCheckinClassifiesBeforeActionTypeFilter pins the subtle case the Python
// original documents: after claiming, upstream downgrades actionType to
// VIEW_DETAILS. Filtering on action type before classifying would report
// "nothing to claim" for an account that did claim today.
func TestCheckinClassifiesBeforeActionTypeFilter(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{
			{"campaign_id": "c1", "claim_status": "CLAIMED", "actionType": "VIEW_DETAILS",
				"benefit": map[string]any{"kind": "CREDITS", "amount": 100}},
			// A genuine details-only entry with no reward: must not be counted
			// as already-claimed.
			{"campaign_id": "c2", "claim_status": "CLAIMED", "actionType": "VIEW_DETAILS",
				"benefit": map[string]any{"kind": "", "amount": 0}},
		},
		claims: map[string]int{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if res.Campaign == nil {
		t.Fatal("campaign detail missing")
	}
	if len(res.Campaign.Already) != 1 {
		t.Fatalf("Already = %v, want exactly the rewarded one", res.Campaign.Already)
	}
	if !res.Already {
		t.Error("result should be marked already-claimed")
	}
}

// TestCheckinReportsPendingAndLocked pins that non-claimable-but-relevant
// campaigns are surfaced instead of being silently dropped.
func TestCheckinReportsPendingAndLocked(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{
			{"campaign_id": "sold-out", "claim_status": "UNAVAILABLE",
				"unavailableReason": "REDEMPTION_CODE_OUT_OF_STOCK",
				"benefit":           map[string]any{"kind": "CREDITS", "amount": 100}},
			{"campaign_id": "locked", "claim_status": "UNAVAILABLE",
				"unavailableReason": "ACHIEVEMENT_NOT_COMPLETED",
				"benefit":           map[string]any{"kind": "CREDITS", "amount": 100}},
		},
		claims: map[string]int{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if res.Campaign == nil {
		t.Fatal("campaign detail missing")
	}
	if len(res.Campaign.Pending) != 1 {
		t.Errorf("Pending = %v, want 1", res.Campaign.Pending)
	}
	if len(res.Campaign.Locked) != 1 {
		t.Errorf("Locked = %v, want 1", res.Campaign.Locked)
	}
	// Both are informative, not failures: the account is not broken.
	if !res.OK {
		t.Errorf("pending/locked should not be a failure: %+v", res)
	}
}

// TestCheckinRetriesEmptyCampaignList pins the issue-#130 behaviour: upstream
// intermittently returns an empty list, and one empty answer must not lose the
// day's reward.
func TestCheckinRetriesEmptyCampaignList(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{{
			"campaign_id": "daily-credits", "claim_status": "CLAIMABLE",
			"benefit": map[string]any{"kind": "CREDITS", "amount": 100},
		}},
		claims:     map[string]int{},
		emptyFirst: 1,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	res := acc.Checkin(t.TempDir())

	if fake.listCalls < 2 {
		t.Errorf("list calls = %d, want a retry after the empty answer", fake.listCalls)
	}
	if res.RewardCredits != 100 {
		t.Errorf("RewardCredits = %d, want 100 after retry", res.RewardCredits)
	}
}

// TestCheckinIgnoresCouponCampaignsForDailyButton pins only-kinds=CREDITS: the
// panel's daily button must not touch redemption-code campaigns.
func TestCheckinIgnoresCouponCampaignsForDailyButton(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{
			{"campaign_id": "credits", "claim_status": "CLAIMABLE",
				"benefit": map[string]any{"kind": "CREDITS", "amount": 100}},
			{"campaign_id": "coupon", "claim_status": "CLAIMABLE",
				"benefit": map[string]any{"kind": "REDEMPTION_CODE", "amount": 0}},
		},
		claims: map[string]int{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	_ = acc.Checkin(t.TempDir())

	if fake.claims["coupon"] != 0 {
		t.Error("daily check-in must not claim coupon campaigns")
	}
	if fake.claims["credits"] != 1 {
		t.Errorf("credits campaign claims = %d, want 1", fake.claims["credits"])
	}
}

// TestCampaignCheckinGapIsHonoured pins that an explicit zero gap means "no
// wait" and is not clamped: the scheduler passes 1.0, but tests and manual runs
// pass 0 and must not sleep a second per campaign.
func TestCampaignCheckinGapIsHonoured(t *testing.T) {
	fake := &fakeCampaigns{
		campaigns: []map[string]any{
			{"campaign_id": "a", "claim_status": "CLAIMABLE",
				"benefit": map[string]any{"kind": "CREDITS", "amount": 10}},
			{"campaign_id": "b", "claim_status": "CLAIMABLE",
				"benefit": map[string]any{"kind": "CREDITS", "amount": 10}},
		},
		claims: map[string]int{},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withRealmServer(t, accounts.RealmCN, srv)

	acc := newTestAccount(t)
	start := time.Now()
	res := acc.CampaignCheckin(0, true)
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("zero gap still waited %s", elapsed)
	}
	if res.Earned != 20 {
		t.Errorf("Earned = %d, want 20", res.Earned)
	}
}

// TestNextCheckinWindowIsUTC8 pins the boundary rule: at/after 10:00 the next
// window is tomorrow, and the note is always rendered in UTC+8 so a container
// on UTC does not show a time eight hours off.
func TestNextCheckinWindowIsUTC8(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)

	before := time.Date(2026, 11, 17, 9, 59, 0, 0, zone)
	w := accounts.NextCheckinWindow(before)
	if got := time.Unix(w.Epoch, 0).In(zone); got.Hour() != 10 || got.Day() != 17 {
		t.Errorf("09:59 should resolve to the same day 10:00, got %s", got)
	}

	at := time.Date(2026, 11, 17, 10, 0, 0, 0, zone)
	w2 := accounts.NextCheckinWindow(at)
	if got := time.Unix(w2.Epoch, 0).In(zone); got.Day() != 18 {
		t.Errorf("10:00 exactly should roll to tomorrow, got %s", got)
	}

	// A UTC host must still see the UTC+8 rule.
	utcNoon := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC) // 10:00 UTC+8
	w3 := accounts.NextCheckinWindow(utcNoon)
	if got := time.Unix(w3.Epoch, 0).In(zone); got.Day() != 18 {
		t.Errorf("UTC host: got %s, want the next UTC+8 day", got)
	}
	if !strings.Contains(w3.Note, "UTC+8") {
		t.Errorf("note %q must state the zone", w3.Note)
	}
}
