package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xzs/bridge_qoder/internal/qoder"
)

func newAccount(t *testing.T, extra map[string]any) *Account {
	t.Helper()
	data := map[string]any{
		"uid": "uid-a", "nickname": "nick", "domain": "qoder.com.cn",
		"realm": "cn", "accessToken": "dt-x", "enabled": true,
	}
	for k, v := range extra {
		data[k] = v
	}
	acc, err := NewAccount(data, "")
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestAccountReadyBasics(t *testing.T) {
	acc := newAccount(t, nil)
	if !acc.Ready("") {
		t.Fatal("a fresh enabled account must be ready")
	}
	acc.Disable("dead")
	if acc.Ready("") {
		t.Fatal("a disabled account must not be ready")
	}
}

func TestCooldownSingleAccountClamp(t *testing.T) {
	acc := newAccount(t, nil)
	wait := acc.NoteError("boom", 60, true, "", nil)
	if wait != 3 {
		t.Fatalf("single-account clamp = %v, want 3", wait)
	}
	if acc.Ready("") {
		t.Fatal("a cooling-down account must not be ready")
	}
	wait = acc.NoteError("boom", 60, false, "", nil)
	if wait != 60 {
		t.Fatalf("multi-account cooldown = %v, want 60", wait)
	}
}

func TestModelScopedCooldown(t *testing.T) {
	acc := newAccount(t, nil)
	acc.NoteError("throttled", 60, false, "Qwen3.8-Max", nil)
	if acc.Ready("Qwen3.8-Max") {
		t.Fatal("the cooled model must not be ready")
	}
	if !acc.Ready("GLM-5.3") {
		t.Fatal("a model-level cooldown must not block other models")
	}
	acc.NoteSuccess("Qwen3.8-Max")
	if !acc.Ready("Qwen3.8-Max") {
		t.Fatal("note_success must clear the model cooldown")
	}
}

func TestBackoffShapes(t *testing.T) {
	// 10 minutes doubling, capped at two hours.
	if got := softBackoff(1); got != 600 {
		t.Fatalf("softBackoff(1) = %v", got)
	}
	if got := softBackoff(2); got != 1200 {
		t.Fatalf("softBackoff(2) = %v", got)
	}
	if got := softBackoff(100); got != 7200 {
		t.Fatalf("softBackoff(100) = %v, want the 2h cap", got)
	}
	// The breaker only trips at the threshold, starting at 30 minutes.
	if got := breakerBackoff(3); got != 1800 {
		t.Fatalf("breakerBackoff(3) = %v", got)
	}
	if got := breakerBackoff(200); got != 21600 {
		t.Fatalf("breakerBackoff(200) = %v, want the 6h cap", got)
	}
	// Downgrade starts at 10 minutes.
	if got := degradeBackoff(5); got != 600 {
		t.Fatalf("degradeBackoff(5) = %v", got)
	}
}

func TestBreakerTripsAfterThreshold(t *testing.T) {
	acc := newAccount(t, nil)
	acc.NoteFailure("a")
	acc.NoteFailure("b")
	if !acc.Ready("") {
		t.Fatal("the breaker must not trip before the threshold")
	}
	acc.NoteFailure("c")
	if acc.Ready("") {
		t.Fatal("the breaker must trip at the threshold")
	}
	acc.NoteSuccess("")
	if !acc.Ready("") {
		t.Fatal("a success must reset the breaker")
	}
}

func TestBalanceCooldownOnlyReleasedByRefresh(t *testing.T) {
	acc := newAccount(t, nil)
	acc.NoteBalanceCooled("402", nil)
	if acc.Ready("") {
		t.Fatal("a 402 park must block the account")
	}
	// A plain success must NOT lift the park.
	acc.NoteSuccess("")
	if acc.Ready("") {
		t.Fatal("note_success must not lift the 402 park")
	}
	// Only a refresh that sees a positive balance does.
	acc.Credits = &Credits{Remain: 10, RemainEffective: 10}
	if !acc.ReviveBalanceCooldown() {
		t.Fatal("a positive balance must lift the park")
	}
	if !acc.Ready("") {
		t.Fatal("the account must be ready after revival")
	}
}

func TestGuardrailsFailOpenWhenUnknown(t *testing.T) {
	acc := newAccount(t, nil)
	acc.ApplyLimits(0, 1000, 0, 0, 0, nil)
	// Daily usage not folded yet: must not block.
	if acc.QuotaBlocked("") != "" {
		t.Fatal("an unknown daily counter must fail open")
	}
	tokens := 1000
	acc.SetDailyUsage(&tokens, nil, nil)
	if acc.QuotaBlocked("") != "daily_tokens" {
		t.Fatalf("quota block = %q", acc.QuotaBlocked(""))
	}
}

func TestReserveFloorAndFreeModels(t *testing.T) {
	acc := newAccount(t, nil)
	acc.Credits = &Credits{Remain: 5, RemainEffective: 5}
	acc.ApplyLimits(10, 0, 0, 0, 0, nil)
	if acc.QuotaBlocked("") != "reserve" {
		t.Fatal("the reserve floor should block")
	}
	// A shared pool with no cap must bypass the floor explicitly.
	acc.Credits.Shared = &SharedQuota{Unlimited: true}
	if acc.QuotaBlocked("") != "" {
		t.Fatal("an unlimited shared pool must bypass the reserve floor")
	}
	// The daily credit cap exempts free models.
	acc2 := newAccount(t, nil)
	used := 100.0
	acc2.SetDailyUsage(nil, &used, nil)
	acc2.ApplyLimits(0, 0, 50, 0, 0, map[string]bool{"qfmodel": true})
	if acc2.QuotaBlocked("qfmodel") != "" {
		t.Fatal("a free model must be exempt from the daily credit cap")
	}
	if acc2.QuotaBlocked("dmodel") != "daily_credits" {
		t.Fatal("a paid model must hit the daily credit cap")
	}
}

func TestLedgerIsUnknown(t *testing.T) {
	// A degenerate body: all zeros, exceeded, never-expires placeholder.
	c := &Credits{Exceeded: true, ExpiresAt: LegacyExpiryNever}
	if !LedgerIsUnknown(c) {
		t.Fatal("the degenerate body must be classified as unknown")
	}
	// A genuinely exhausted account has size > 0.
	c2 := &Credits{Exceeded: true, Size: 1000, ExpiresAt: LegacyExpiryNever}
	if LedgerIsUnknown(c2) {
		t.Fatal("a real exhaustion must not be classified as unknown")
	}
}

func TestNextCheckinWindowUTC8(t *testing.T) {
	// 09:00 UTC+8 -> today at 10:00.
	before := time.Date(2026, 11, 15, 9, 0, 0, 0, qoder.UTC8)
	w := NextCheckinWindow(before)
	want := time.Date(2026, 11, 15, 10, 0, 0, 0, qoder.UTC8)
	if w.Epoch != want.Unix() {
		t.Fatalf("window = %s, want %s", time.Unix(w.Epoch, 0).In(qoder.UTC8), want)
	}
	// 10:00 UTC+8 exactly -> tomorrow (never return a time already reached).
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, qoder.UTC8)
	w2 := NextCheckinWindow(at)
	if w2.Epoch != want.AddDate(0, 0, 1).Unix() {
		t.Fatalf("at the boundary the window must roll to tomorrow")
	}
	// The note is rendered in UTC+8 regardless of the host zone.
	if !strings.Contains(w.Note, "UTC+8") {
		t.Fatalf("note = %q", w.Note)
	}
}

// TestMarshalJSONNeverEmitsCredentials pins that the implicit encoding path is
// the public view.
//
// ToMap() carries access/refresh/personal tokens because it is the on-disk
// document. A type whose json.Marshal output contains plaintext credentials is
// one careless call away from a leak — a log line, an error payload, a new panel
// route that forgot to scrub. Persistence and the secrets=1 export call ToMap()
// explicitly, so making MarshalJSON safe costs nothing and closes the implicit
// path.
func TestMarshalJSONNeverEmitsCredentials(t *testing.T) {
	acc, err := NewAccount(map[string]any{
		"uid": "m-1", "nickname": "n", "realm": "cn",
		"accessToken": "dt-secret", "refreshToken": "drt-secret",
		"personalToken": "pt-secret", "enabled": true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(acc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	for _, secret := range []string{"dt-secret", "drt-secret", "pt-secret"} {
		if strings.Contains(text, secret) {
			t.Errorf("json.Marshal(acc) leaked %q: %s", secret, text)
		}
	}
	// The identity fields must survive, or a caller cannot tell accounts apart.
	if !strings.Contains(text, "m-1") {
		t.Errorf("json.Marshal(acc) lost the uid: %s", text)
	}
	// The persistence document must still carry them.
	if acc.ToMap()["accessToken"] != "dt-secret" {
		t.Error("ToMap() must keep the tokens: it is the on-disk document")
	}
}

func TestAccountPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	data := map[string]any{
		"uid": "round-trip", "nickname": "n", "realm": "intl",
		"domain": "qoder.com", "accessToken": "dt-1", "refreshToken": "drt-1",
		"personalToken": "pt-1", "expiresAt": 1999999999,
		"enabled": true, "userType": "team", "organizationId": "org-1",
		"credits":       map[string]any{"remain": 42, "size": 100, "used": 58},
		"campaignCodes": map[string]any{"c1": "CODE1"},
	}
	acc, err := NewAccount(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "round-trip.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reloaded map[string]any
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	acc2, err := NewAccount(reloaded, "")
	if err != nil {
		t.Fatal(err)
	}
	if acc2.Realm != "intl" || acc2.AccessToken != "dt-1" {
		t.Fatalf("round trip lost fields: %+v", acc2.ToMap())
	}
	if acc2.Credits == nil || acc2.Credits.Remain != 42 {
		t.Fatalf("credits lost: %+v", acc2.Credits)
	}
	if acc2.CampaignCodes["c1"] != "CODE1" {
		t.Fatal("redemption codes must persist (the campaign only grants once)")
	}
}

func TestPoolLoadSkipsNonAccountFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, doc map[string]any) {
		blob, _ := json.Marshal(doc)
		if err := os.WriteFile(filepath.Join(dir, name), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("uid-1.json", map[string]any{"uid": "uid-1", "accessToken": "dt-a", "realm": "cn"})
	write("settings.json", map[string]any{"panel_password_hash": "x"})
	write("models-cache-cn.json", map[string]any{"rows": []any{}})
	write("credits-source.json", map[string]any{"cn": map[string]any{}})
	if err := os.MkdirAll(filepath.Join(dir, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("scheduler", "state.json"), map[string]any{"enabled": true})

	pool, err := NewPool(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	accts := pool.Accounts()
	if len(accts) != 1 || accts[0].UID != "uid-1" {
		t.Fatalf("loaded %d accounts, want only uid-1", len(accts))
	}
}

func TestPoolPickAndAffinity(t *testing.T) {
	dir := t.TempDir()
	for i, uid := range []string{"a", "b", "c"} {
		doc, _ := json.Marshal(map[string]any{
			"uid": uid, "accessToken": "dt-" + uid, "realm": "cn",
			"enabled": true, "addedAt": 1000 + i,
		})
		if err := os.WriteFile(filepath.Join(dir, uid+".json"), doc, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := NewPool(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Session affinity binds the same conversation to one account.
	first := pool.PickForSession("cn", "", "conv-1", nil, nil)
	if first == nil {
		t.Fatal("no account picked")
	}
	for i := 0; i < 5; i++ {
		again := pool.PickForSession("cn", "", "conv-1", nil, nil)
		if again.UID != first.UID {
			t.Fatalf("affinity broke: %s != %s", again.UID, first.UID)
		}
	}
	// Round-robin spreads different conversations.
	seen := map[string]bool{first.UID: true}
	for i := 0; i < 3; i++ {
		acc := pool.Pick("cn", "", nil, nil)
		seen[acc.UID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("round-robin did not spread across accounts: %v", seen)
	}
}

func TestVPCParsing(t *testing.T) {
	cases := []struct {
		in       string
		instance string
		openapi  string
	}{
		{"acme", "acme", "https://acme-openapi.vpc.qoder.com.cn"},
		{"acme.vpc.qoder.com.cn", "acme", "https://acme-openapi.vpc.qoder.com.cn"},
		{"acme-openapi.vpc.qoder.com.cn", "acme", "https://acme-openapi.vpc.qoder.com.cn"},
		{"acme-gateway.vpc.qoder.com.cn", "acme", "https://acme-openapi.vpc.qoder.com.cn"},
	}
	for _, c := range cases {
		vpc := c.in
		cfg, err := RealmConfigFor(RealmCN, &vpc)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if cfg.VPCInstance != c.instance {
			t.Fatalf("%q -> instance %q", c.in, cfg.VPCInstance)
		}
		if cfg.OpenAPI != c.openapi {
			t.Fatalf("%q -> openapi %q", c.in, cfg.OpenAPI)
		}
	}
	if _, err := qoder.ParseVPCEndpoint("https://acme.vpc.qoder.com.cn"); err == nil {
		t.Fatal("a URL with a scheme must be rejected")
	}
	if _, err := qoder.ParseVPCEndpoint("acme/evil"); err == nil {
		t.Fatal("a path must be rejected")
	}
}

func TestGatewayCandidatesVPC(t *testing.T) {
	cfg := GetRealmConfig(RealmINTL)
	hosts := GatewayCandidates(RealmINTL, cfg)
	if len(hosts) < 2 {
		t.Fatalf("intl should have fallback hosts: %v", hosts)
	}
	// A VPC instance has exactly one host: the public cluster must never carry
	// enterprise credentials.
	vpcCfg := GetRealmConfig(RealmCN)
	vpcCfg.VPCInstance = "acme"
	vpcCfg.GatewayFallbacks = []string{"https://gateway.qoder.com.cn"}
	if got := GatewayCandidates(RealmCN, vpcCfg); len(got) != 1 {
		t.Fatalf("VPC candidates = %v, want exactly one", got)
	}
}

func TestDetectRealmFromDomain(t *testing.T) {
	cases := map[string]string{
		"qoder.com.cn":          "cn",
		"qoder.com":             "intl",
		"api3.qoder.sh":         "intl",
		"acme.vpc.qoder.com.cn": "cn",
	}
	for domain, want := range cases {
		if got := DetectRealmFromDomain(domain); got != want {
			t.Fatalf("%q -> %q, want %q", domain, got, want)
		}
	}
}

func TestQuotaNormalization(t *testing.T) {
	payload := map[string]any{
		"userQuota":   map[string]any{"remaining": 100, "used": 20, "total": 120},
		"addOnQuota":  map[string]any{"remaining": 50, "used": 10, "total": 60},
		"sotaCredits": map[string]any{"remaining": 30, "used": 0, "total": 30},
		"orgResourcePackage": map[string]any{
			"remaining": 952, "used": 1548, "cap": 2500, "available": true,
		},
	}
	composition, shared := NormalizeQuotaPayload(payload)
	if len(composition) != 3 {
		t.Fatalf("composition = %+v", composition)
	}
	if shared == nil || shared.Remain != 952 || shared.Size != 2500 {
		t.Fatalf("shared = %+v", shared)
	}
	if !shared.Available {
		t.Fatal("available must be read as true")
	}
}

func TestSharedUnlimitedSentinel(t *testing.T) {
	payload := map[string]any{
		"orgResourcePackage": map[string]any{
			"remaining": 12345, "used": 0, "cap": -1, "available": true,
		},
	}
	_, shared := NormalizeQuotaPayload(payload)
	if shared == nil || !shared.Unlimited {
		t.Fatalf("cap=-1 must be treated as unlimited: %+v", shared)
	}
}

func TestSharedAvailableStringFalse(t *testing.T) {
	// The upstream once returned the boolean as a string; "false" must read as
	// false, not as a truthy non-empty string.
	payload := map[string]any{
		"sharedQuota": map[string]any{"remaining": 5, "cap": 10, "available": "false"},
	}
	_, shared := NormalizeQuotaPayload(payload)
	if shared == nil || shared.Available {
		t.Fatalf("available \"false\" must read as false: %+v", shared)
	}
}

func TestSessionDeadMarkers(t *testing.T) {
	for _, msg := range []string{
		"TOKEN_EXPIRE", "code 12153", "Offline user session not found",
	} {
		if !SessionDead(msg) {
			t.Fatalf("%q must be recognised as a dead session", msg)
		}
	}
	if SessionDead("rate limited") {
		t.Fatal("a rate limit is not a dead session")
	}
}

func TestTokenFamily(t *testing.T) {
	acc := newAccount(t, map[string]any{"refreshToken": "drt-x"})
	if acc.TokenFamily() != "device" {
		t.Fatalf("family = %q", acc.TokenFamily())
	}
	acc2 := newAccount(t, map[string]any{"refreshToken": "jrt-x"})
	if acc2.TokenFamily() != "job" {
		t.Fatalf("family = %q", acc2.TokenFamily())
	}
	acc3 := newAccount(t, map[string]any{"personalToken": "pt-x"})
	if acc3.TokenFamily() != "pat" {
		t.Fatalf("family = %q", acc3.TokenFamily())
	}
}

func TestExpiringWindow(t *testing.T) {
	soon := time.Now().Add(48 * time.Hour).Unix()
	acc := newAccount(t, nil)
	acc.Credits = &Credits{Packages: []map[string]any{
		{"name": "basic", "remain": 10, "expires_at": soon},
	}}
	acc.ApplyLimits(0, 0, 0, 0, 3, nil)
	if !acc.InExpiringWindow() {
		t.Fatal("a pack expiring in 2 days must be inside a 3-day window")
	}
	acc.ApplyLimits(0, 0, 0, 0, 1, nil)
	if acc.InExpiringWindow() {
		t.Fatal("a 1-day window must exclude a 2-day pack")
	}
	// The never-expires placeholder must be excluded.
	acc2 := newAccount(t, nil)
	acc2.Credits = &Credits{Packages: []map[string]any{
		{"name": "basic", "remain": 10, "expires_at": LegacyExpiryNever},
	}}
	acc2.ApplyLimits(0, 0, 0, 0, 3650, nil)
	if acc2.InExpiringWindow() {
		t.Fatal("the never-expires placeholder must not count as expiring")
	}
}

func TestCheckinEligibilityDecoupled(t *testing.T) {
	acc := newAccount(t, map[string]any{"enabled": false})
	if acc.InferenceEligible() {
		t.Fatal("a disabled account must not serve inference")
	}
	if acc.CheckinEligible() {
		t.Fatal("check-in stays off until explicitly allowed")
	}
	acc.SetCheckinWhenDisabled(true)
	if !acc.CheckinEligible() {
		t.Fatal("explicitly allowing check-in while paused must work")
	}
	if acc.InferenceEligible() {
		t.Fatal("allowing check-in must not re-enable inference")
	}
}
