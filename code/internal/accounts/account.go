package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Failure-governance thresholds (task-74). Restarting the process resets them.
const (
	// BreakerThreshold is how many consecutive hard failures trip the breaker.
	BreakerThreshold = 3
	// DegradeThreshold is how many consecutive unknown failures start the
	// downgrade window (non-blocking; only affects pick order).
	DegradeThreshold = 5
	// SessionDeadMarkers mark an upstream-revoked offline session.
	SessionDeadTOKENEXPIRE = "TOKEN_EXPIRE"
	SessionDeadCode        = "12153"
)

// CheckinWindowHourUTC8 is the fixed reset hour for daily credits.
const CheckinWindowHourUTC8 = 10

// LegacyExpiryNever is CN's "never expires" placeholder epoch.
const LegacyExpiryNever = 253402214400

var utc8 = time.FixedZone("UTC+8", 8*3600)

// CheckinWindow is the next daily reset window.
type CheckinWindow struct {
	Epoch int64  `json:"epoch"`
	Note  string `json:"note"`
}

// NextCheckinWindow returns the next "10:00 UTC+8" boundary and a human note.
//
// The window is always rendered in UTC+8, never the host's local zone: in a
// container the host is usually UTC, and rendering locally would show a "next"
// time eight hours off from the official rule.
func NextCheckinWindow(now time.Time) CheckinWindow {
	local := now.In(utc8)
	today := time.Date(local.Year(), local.Month(), local.Day(),
		CheckinWindowHourUTC8, 0, 0, 0, utc8)
	target := today
	if !local.Before(today) {
		target = today.AddDate(0, 0, 1)
	}
	return CheckinWindow{Epoch: target.Unix(), Note: target.Format("01-02 15:04") + "（UTC+8）"}
}

// Credits is the normalised quota snapshot.
type Credits struct {
	Remain          int              `json:"remain"`
	RemainEffective int              `json:"remain_effective"`
	Used            int              `json:"used"`
	Size            int              `json:"size"`
	Exceeded        bool             `json:"exceeded"`
	UsagePct        *float64         `json:"usage_pct,omitempty"`
	ExpiresAt       int64            `json:"expires_at,omitempty"`
	Shared          *SharedQuota     `json:"shared,omitempty"`
	Packages        []map[string]any `json:"packages,omitempty"`
	Composition     []Composition    `json:"composition,omitempty"`
	Source          string           `json:"source,omitempty"`
	UpdatedAt       int64            `json:"updated_at"`
	UpdatedISO      string           `json:"updated_iso"`
}

// SharedQuota is the organisation-level pool.
type SharedQuota struct {
	Remain    int    `json:"remain"`
	Used      int    `json:"used"`
	Size      int    `json:"size"`
	Available bool   `json:"available"`
	Unlimited bool   `json:"unlimited,omitempty"`
	Source    string `json:"source"`
}

// Composition is one quota container (plan / add-on / SOTA / shared).
type Composition struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Remain    int    `json:"remain"`
	Used      int    `json:"used"`
	Size      int    `json:"size"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// Account is one Qoder account and its runtime state.
//
// Persisted fields mirror the Python credential file exactly so an existing
// accounts/ directory keeps working after the port.
type Account struct {
	mu sync.RWMutex

	// Path is the credential file ("" for a stub built in memory).
	Path string `json:"-"`

	UID         string  `json:"uid"`
	Nickname    string  `json:"nickname"`
	Note        string  `json:"note,omitempty"`
	Group       string  `json:"group,omitempty"`
	Domain      string  `json:"domain"`
	Realm       string  `json:"realm"`
	VPCOverride *string `json:"vpcEndpoint"`
	Platform    string  `json:"platform"`

	AccessToken   string  `json:"accessToken"`
	RefreshToken  string  `json:"refreshToken"`
	PersonalToken string  `json:"personalToken,omitempty"`
	ExpiresAt     int64   `json:"expiresAt"`
	AddedAt       int64   `json:"addedAt"`
	Source        string  `json:"source"`
	Enabled       bool    `json:"enabled"`
	LastError     string  `json:"lastError"`
	CooldownUntil float64 `json:"cooldownUntil"`

	Credits              *Credits           `json:"credits,omitempty"`
	LastQuotaAnomaly     map[string]any     `json:"lastQuotaAnomaly,omitempty"`
	CreditsDegraded      any                `json:"creditsDegraded,omitempty"`
	Plan                 string             `json:"plan,omitempty"`
	LastCheckin          string             `json:"lastCheckin,omitempty"`
	LastCreditsDelta     *int               `json:"lastCreditsDelta,omitempty"`
	LastCreditsDeltaAt   float64            `json:"lastCreditsDeltaAt,omitempty"`
	CheckinWhenDisabled  bool               `json:"checkinWhenDisabled,omitempty"`
	CampaignCodes        map[string]string  `json:"campaignCodes,omitempty"`
	CampaignBlockedUntil map[string]float64 `json:"campaignBlockedUntil,omitempty"`
	UserType             string             `json:"userType,omitempty"`
	OrganizationID       string             `json:"organizationId,omitempty"`
	OrganizationName     string             `json:"organizationName,omitempty"`

	// --- runtime-only state (never persisted) ---

	// Model cooldowns: upstream rate limits target a single model, so they must
	// not take the whole account out of rotation.
	modelCooldowns map[string]float64
	// Credential-refresh bookkeeping for the panel.
	creditsPending         bool
	creditsRefreshState    string
	creditsRefreshError    string
	creditsRefreshFailures int
	creditsRefreshNext     float64

	// Budget guardrails (task-61). None = unknown -> fail open.
	ReserveCredits       int
	DailyTokenLimit      int
	DailyTokensToday     *int
	DailyCreditLimit     int
	DailyCreditsToday    *float64
	ModelDailyTokenLimit int
	ModelDailyTokens     map[string]int
	FreeModels           map[string]bool
	ExpiringWindowDays   int
	BalanceUntil         float64
	BalanceReason        string

	// Three-family failure governance (task-74).
	softStreak   int
	fails        int
	degradeCount int
	breakerUntil float64
	degradeUntil float64

	// Team/every-account traffic guard (in-memory).
	teamLeases    int
	teamLastStart float64
	teamClientIPs map[string]float64

	// Check-in capability probing.
	checkinCap       *bool
	checkinCapReason string
	checkinCapAt     float64

	MachineIdentitySource string
	MachineHeadersState   string
}

// MACHINE_HEADERS_OMITTED is the conservative default before a desktop request
// has actually been built.
const MACHINE_HEADERS_OMITTED = "omitted"

// NewAccount builds an Account from a decoded credential document.
func NewAccount(data map[string]any, path string) (*Account, error) {
	a := &Account{
		Path:                  path,
		modelCooldowns:        map[string]float64{},
		ModelDailyTokens:      map[string]int{},
		FreeModels:            map[string]bool{},
		teamClientIPs:         map[string]float64{},
		CreditsDegraded:       data["creditsDegraded"],
		MachineIdentitySource: "derived",
		MachineHeadersState:   MACHINE_HEADERS_OMITTED,
		creditsRefreshState:   "idle",
	}
	a.UID = asString(data["uid"])
	a.Nickname = asString(data["nickname"])
	a.Note = asString(data["note"])
	a.Group = strings.TrimSpace(asString(data["group"]))
	a.Domain = asString(data["domain"])
	a.Realm = asString(data["realm"])
	if a.Realm == "" {
		a.Realm = DetectRealmFromDomain(a.Domain)
	}
	if a.Realm != RealmCN && a.Realm != RealmINTL {
		a.Realm = RealmCN
	}

	a.ApplyVPCOverride(data)
	a.Platform = asString(data["platform"])
	if a.Platform == "" {
		a.Platform = "CLI"
	}
	a.AccessToken = asString(data["accessToken"])
	a.RefreshToken = asString(data["refreshToken"])
	a.PersonalToken = asString(data["personalToken"])
	a.ExpiresAt = normalizeEpoch(data["expiresAt"])
	if added, ok := data["addedAt"]; ok && added != nil {
		a.AddedAt = normalizeEpoch(added)
	} else {
		a.AddedAt = time.Now().Unix()
	}
	a.Source = asString(data["source"])
	if a.Source == "" {
		a.Source = "oauth"
	}
	a.Enabled = true
	if v, ok := data["enabled"].(bool); ok {
		a.Enabled = v
	}
	a.LastError = asString(data["lastError"])
	a.CooldownUntil = asFloat(data["cooldownUntil"])

	if cred, ok := data["credits"].(map[string]any); ok {
		a.Credits = decodeCredits(cred)
	}
	if anomaly, ok := data["lastQuotaAnomaly"].(map[string]any); ok {
		a.LastQuotaAnomaly = anomaly
	}
	a.Plan = asString(data["plan"])
	a.LastCheckin = asString(data["lastCheckin"])
	if v, ok := data["lastCreditsDelta"]; ok && v != nil {
		n := int(asInt(v))
		a.LastCreditsDelta = &n
	}
	a.LastCreditsDeltaAt = asFloat(data["lastCreditsDeltaAt"])
	a.CheckinWhenDisabled = asBool(data["checkinWhenDisabled"])
	if codes, ok := data["campaignCodes"].(map[string]any); ok {
		a.CampaignCodes = map[string]string{}
		for k, v := range codes {
			a.CampaignCodes[k] = asString(v)
		}
	}
	if blocked, ok := data["campaignBlockedUntil"].(map[string]any); ok {
		a.CampaignBlockedUntil = map[string]float64{}
		for k, v := range blocked {
			a.CampaignBlockedUntil[k] = asFloat(v)
		}
	}
	a.UserType = asString(data["userType"])
	if a.UserType == "" {
		a.UserType = DefaultUserType
	}
	a.OrganizationID = asString(data["organizationId"])
	a.OrganizationName = asString(data["organizationName"])
	return a, nil
}

// ApplyVPCOverride resolves the account's VPC selection from explicit override,
// credential domain, or deployment default.
func (a *Account) ApplyVPCOverride(data map[string]any) {
	var override *string
	if raw, present := data["vpcEndpoint"]; present {
		if v, err := NormalizeVPCOverride(raw); err == nil {
			override = v
		}
	} else if a.Realm == RealmCN && strings.HasSuffix(strings.ToLower(a.Domain), "."+VPCDomainSuffix) {
		if v, err := NormalizeVPCOverride(a.Domain); err == nil {
			override = v
		}
	}
	a.VPCOverride = override
	cfg, err := RealmConfigFor(a.Realm, override)
	if err == nil {
		a.Domain = cfg.Domain
	} else {
		a.VPCOverride = nil
		cfg, _ = RealmConfigFor(a.Realm, nil)
		a.Domain = cfg.Domain
	}
}

// RealmConfig returns the account's effective endpoint config.
func (a *Account) RealmConfig() RealmConfig {
	cfg, err := RealmConfigFor(a.Realm, a.VPCOverride)
	if err != nil {
		cfg, _ = RealmConfigFor(a.Realm, nil)
	}
	return cfg
}

// EndpointScope is the key used in credits-source.json.
func (a *Account) EndpointScope() string {
	cfg := a.RealmConfig()
	if cfg.VPCInstance != "" {
		return a.Realm + ":vpc:" + cfg.VPCInstance
	}
	return a.Realm
}

// ToMap renders the persisted document (matching the Python file layout).
func (a *Account) ToMap() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[string]any{
		"uid":                 a.UID,
		"nickname":            a.Nickname,
		"group":               a.Group,
		"domain":              a.Domain,
		"realm":               a.Realm,
		"vpcEndpoint":         a.VPCOverride,
		"platform":            a.Platform,
		"accessToken":         a.AccessToken,
		"refreshToken":        a.RefreshToken,
		"personalToken":       a.PersonalToken,
		"expiresAt":           a.ExpiresAt,
		"addedAt":             a.AddedAt,
		"source":              a.Source,
		"enabled":             a.Enabled,
		"lastError":           a.LastError,
		"cooldownUntil":       a.CooldownUntil,
		"plan":                a.Plan,
		"lastCheckin":         a.LastCheckin,
		"checkinWhenDisabled": a.CheckinWhenDisabled,
		"userType":            a.UserType,
		"organizationId":      a.OrganizationID,
		"organizationName":    a.OrganizationName,
		"note":                a.Note,
	}
	if a.Credits != nil {
		out["credits"] = a.Credits
	} else {
		out["credits"] = nil
	}
	if a.LastQuotaAnomaly != nil {
		out["lastQuotaAnomaly"] = a.LastQuotaAnomaly
	}
	out["creditsDegraded"] = a.CreditsDegraded
	if a.LastCreditsDelta != nil {
		out["lastCreditsDelta"] = *a.LastCreditsDelta
	}
	out["lastCreditsDeltaAt"] = a.LastCreditsDeltaAt
	if a.CampaignCodes != nil {
		out["campaignCodes"] = a.CampaignCodes
	}
	if a.CampaignBlockedUntil != nil {
		out["campaignBlockedUntil"] = a.CampaignBlockedUntil
	}
	return out
}

// Save writes the credential file atomically.
func (a *Account) Save(dir string) error {
	a.mu.RLock()
	path := a.Path
	if path == "" && dir != "" && a.UID != "" {
		path = filepath.Join(dir, a.UID+".json")
	}
	a.mu.RUnlock()
	if path == "" {
		return fmt.Errorf("account %s has no path", a.UID)
	}
	if err := writeJSONAtomic(path, a.ToMap()); err != nil {
		return err
	}
	a.mu.Lock()
	a.Path = path
	a.mu.Unlock()
	return nil
}

// Delete removes the credential file.
func (a *Account) Delete() error {
	if a.Path == "" {
		return nil
	}
	return os.Remove(a.Path)
}

// Public is the panel-facing view (credentials are scrubbed).
func (a *Account) Public() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := float64(time.Now().Unix())
	out := map[string]any{
		"uid":                     a.UID,
		"nickname":                a.Nickname,
		"note":                    a.Note,
		"group":                   a.Group,
		"domain":                  a.Domain,
		"realm":                   a.Realm,
		"vpcEndpoint":             a.VPCOverride,
		"platform":                a.Platform,
		"expiresAt":               a.ExpiresAt,
		"addedAt":                 a.AddedAt,
		"source":                  a.Source,
		"enabled":                 a.Enabled,
		"lastError":               a.LastError,
		"cooldownUntil":           a.CooldownUntil,
		"cooldown_left_s":         maxF(0, a.CooldownUntil-now),
		"plan":                    a.Plan,
		"lastCheckin":             a.LastCheckin,
		"checkinWhenDisabled":     a.CheckinWhenDisabled,
		"userType":                a.UserType,
		"organizationId":          a.OrganizationID,
		"organizationName":        a.OrganizationName,
		"credits":                 a.Credits,
		"lastQuotaAnomaly":        a.LastQuotaAnomaly,
		"creditsDegraded":         a.CreditsDegraded,
		"credits_refresh_state":   a.creditsRefreshState,
		"credits_refresh_error":   a.creditsRefreshError,
		"free_models":             mapKeys(a.FreeModels),
		"machine_identity_source": a.MachineIdentitySource,
		"machine_headers_state":   a.MachineHeadersState,
		"guardrails":              a.guardrailsView(),
		"three_families": map[string]any{
			"soft_streak":   a.softStreak,
			"fails":         a.fails,
			"degrade_count": a.degradeCount,
			"breaker_until": a.breakerUntil,
			"degrade_until": a.degradeUntil,
		},
		"balance_until":      a.BalanceUntil,
		"balance_reason":     a.BalanceReason,
		"inference_eligible": a.inferenceEligibleLocked(),
		"checkin_eligible":   a.checkinEligibleLocked(),
		"is_team":            a.isTeamLocked(),
	}
	if a.LastCreditsDelta != nil {
		out["lastCreditsDelta"] = *a.LastCreditsDelta
	}
	out["lastCreditsDeltaAt"] = a.LastCreditsDeltaAt
	if a.Credits != nil {
		out["credits_remain"] = a.Credits.Remain
		out["credits_used"] = a.Credits.Used
		out["credits_size"] = a.Credits.Size
	}
	if a.OrganizationID != "" || a.OrganizationName != "" {
		out["org_label"] = orgLabel(a.OrganizationID, a.OrganizationName)
	}
	if cw := NextCheckinWindow(time.Now()); a.BalanceUntil > 0 {
		out["balance_reset_at"] = cw.Epoch
		out["balance_reset_note"] = cw.Note
	}
	return out
}

func (a *Account) guardrailsView() map[string]any {
	return map[string]any{
		"reserve_credits":         a.ReserveCredits,
		"daily_token_limit":       a.DailyTokenLimit,
		"daily_tokens_today":      a.DailyTokensToday,
		"daily_credit_limit":      a.DailyCreditLimit,
		"daily_credits_today":     a.DailyCreditsToday,
		"model_daily_token_limit": a.ModelDailyTokenLimit,
		"model_daily_tokens":      a.ModelDailyTokens,
		"expiring_window_days":    a.ExpiringWindowDays,
		"blocked":                 a.QuotaBlocked(""),
	}
}

func orgLabel(id, name string) string {
	switch {
	case id != "" && name != "":
		return name + " (" + id + ")"
	case name != "":
		return name
	default:
		return id
	}
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

func (a *Account) inferenceEligibleLocked() bool {
	return a.Enabled && a.AccessToken != ""
}

func (a *Account) checkinEligibleLocked() bool {
	if a.AccessToken == "" {
		return false
	}
	return a.Enabled || a.CheckinWhenDisabled
}

func (a *Account) isTeamLocked() bool {
	if a.OrganizationID != "" || a.OrganizationName != "" {
		return true
	}
	ut := strings.ToLower(a.UserType)
	return strings.Contains(ut, "team") || strings.Contains(ut, "enterprise")
}

// InferenceEligible reports whether the account may serve inference traffic.
func (a *Account) InferenceEligible() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.inferenceEligibleLocked()
}

// CheckinEligible reports whether the account may participate in check-in.
// Disabled accounts can still claim (a free daily reward), if explicitly asked.
func (a *Account) CheckinEligible() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.checkinEligibleLocked()
}

// IsTeam reports whether this is a team/enterprise account.
func (a *Account) IsTeam() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isTeamLocked()
}

// Ready reports whether the account can serve model right now.
//
// The token freshness check comes last on purpose: it may perform a refresh.
func (a *Account) Ready(model string) bool {
	a.mu.RLock()
	ready := a.readyLocked(model, time.Now())
	a.mu.RUnlock()
	if !ready {
		return false
	}
	if a.tokenFresh() {
		return true
	}
	// Token expired or about to: refreshing is the last resort.
	return a.Refresh() == nil
}

func (a *Account) readyLocked(model string, now time.Time) bool {
	if !a.Enabled || a.AccessToken == "" {
		return false
	}
	ts := float64(now.Unix())
	if a.CooldownUntil > ts {
		return false
	}
	if model != "" && a.modelCooldowns[model] > ts {
		return false
	}
	if a.BalanceUntil > ts {
		return false
	}
	if a.breakerUntil > ts {
		return false
	}
	if a.reserveBlocked() || a.dailyLimitBlocked() ||
		a.creditLimitBlocked(model) || a.modelTokenLimitBlocked(model) {
		return false
	}
	return true
}

func (a *Account) tokenFresh() bool {
	a.mu.RLock()
	exp := a.ExpiresAt
	a.mu.RUnlock()
	if exp == 0 {
		return true
	}
	remaining := exp - time.Now().Unix()
	return remaining > 240
}

// ---------------------------------------------------------------------------
// Budget guardrails (task-61)
// ---------------------------------------------------------------------------

func (a *Account) reserveBlocked() bool {
	reserve := a.ReserveCredits
	if reserve <= 0 || a.Credits == nil {
		return false
	}
	if a.sharedUnlimitedLocked() {
		// An explicit "no cap" shared pool must not be blocked by the floor;
		// relying on "the number is huge" would be luck, not semantics.
		return false
	}
	remain := a.effectiveRemainLocked()
	return remain <= reserve
}

func (a *Account) dailyLimitBlocked() bool {
	if a.DailyTokenLimit <= 0 || a.DailyTokensToday == nil {
		return false
	}
	return *a.DailyTokensToday >= a.DailyTokenLimit
}

func (a *Account) creditLimitReached() bool {
	if a.DailyCreditLimit <= 0 || a.DailyCreditsToday == nil {
		return false
	}
	return *a.DailyCreditsToday >= float64(a.DailyCreditLimit)
}

func (a *Account) creditLimitBlocked(model string) bool {
	if model == "" || a.modelIsFree(model) {
		return false
	}
	return a.creditLimitReached()
}

func (a *Account) modelTokenLimitBlocked(model string) bool {
	if a.ModelDailyTokenLimit <= 0 || model == "" {
		return false
	}
	used, ok := a.ModelDailyTokens[model]
	if !ok {
		return false
	}
	return used >= a.ModelDailyTokenLimit
}

func (a *Account) modelIsFree(model string) bool {
	return model != "" && a.FreeModels[model]
}

func (a *Account) sharedUnlimitedLocked() bool {
	return a.Credits != nil && a.Credits.Shared != nil && a.Credits.Shared.Unlimited
}

func (a *Account) effectiveRemainLocked() int {
	if a.Credits == nil {
		return 0
	}
	return a.Credits.RemainEffective
}

// QuotaBlocked returns the first guardrail that is blocking ("" = none).
func (a *Account) QuotaBlocked(model string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.reserveBlocked():
		return "reserve"
	case a.dailyLimitBlocked():
		return "daily_tokens"
	case a.creditLimitBlocked(model):
		return "daily_credits"
	case a.modelTokenLimitBlocked(model):
		return "model_tokens"
	}
	return ""
}

// ApplyLimits refreshes the guardrail thresholds for the account's realm.
func (a *Account) ApplyLimits(reserve, dailyTokens, dailyCredits, modelDaily int, expiringDays int, freeModels map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ReserveCredits = reserve
	a.DailyTokenLimit = dailyTokens
	a.DailyCreditLimit = dailyCredits
	a.ModelDailyTokenLimit = modelDaily
	a.ExpiringWindowDays = expiringDays
	if freeModels != nil {
		a.FreeModels = freeModels
	}
}

// SetDailyUsage feeds the daily counters folded from usage.jsonl.
// nil means "not folded yet" and must fail open.
func (a *Account) SetDailyUsage(tokens *int, credits *float64, perModel map[string]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DailyTokensToday = tokens
	a.DailyCreditsToday = credits
	if perModel != nil {
		a.ModelDailyTokens = perModel
	}
}

// SoonestExpiringDays returns the fewest days-left among usable credit packs,
// or nil when no pack can be judged.
func (a *Account) SoonestExpiringDays() *float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Credits == nil || len(a.Credits.Packages) == 0 {
		return nil
	}
	now := float64(time.Now().Unix())
	var best *float64
	for _, pkg := range a.Credits.Packages {
		if asBool(pkg["no_expiry"]) {
			continue
		}
		if strings.EqualFold(asString(pkg["package_code"]), "enterprise") {
			continue
		}
		if asBool(pkg["is_expired"]) {
			continue
		}
		remain := asFloat(pkg["remain"])
		if remain <= 0 {
			continue
		}
		var days float64
		hasDays := false
		if v, ok := pkg["days_left"]; ok && v != nil {
			days = asFloat(v)
			hasDays = true
		} else {
			var exp float64
			hasExp := false
			for _, k := range []string{"expires_at", "expireAt"} {
				if v, ok := pkg[k]; ok && v != nil {
					exp = asFloat(v)
					hasExp = true
					break
				}
			}
			if !hasExp {
				continue
			}
			if exp >= LegacyExpiryNever {
				continue
			}
			days = (exp - now) / 86400
			hasDays = true
		}
		if !hasDays || days < 0 {
			continue
		}
		if best == nil || days < *best {
			d := days
			best = &d
		}
	}
	return best
}

// InExpiringWindow reports whether the account should be preferred because its
// balance is about to expire.
func (a *Account) InExpiringWindow() bool {
	a.mu.RLock()
	window := a.ExpiringWindowDays
	a.mu.RUnlock()
	if window <= 0 {
		return false
	}
	soonest := a.SoonestExpiringDays()
	return soonest != nil && *soonest <= float64(window)
}

// ---------------------------------------------------------------------------
// Cooldowns and failure governance
// ---------------------------------------------------------------------------

// NoteError records one error and sets a cooldown, returning the effective
// number of seconds (a single-account pool clamps to 3 s).
func (a *Account) NoteError(message string, cooldown int, singleAccount bool, model string, until *float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastError = truncate(message, 200)
	if model != "" {
		var wait float64
		if until != nil {
			wait = maxF(1, *until-float64(time.Now().Unix()))
		} else if singleAccount {
			wait = 3
		} else {
			wait = float64(cooldown)
		}
		a.modelCooldowns[model] = float64(time.Now().Unix()) + wait
		return wait
	}
	actual := float64(cooldown)
	if singleAccount {
		actual = 3
	}
	a.CooldownUntil = float64(time.Now().Unix()) + actual
	return actual
}

// ThrottleWait returns seconds until the account can serve model again.
func (a *Account) ThrottleWait(model string) float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.Enabled || a.AccessToken == "" {
		return 0
	}
	now := float64(time.Now().Unix())
	wait := maxF(0, a.CooldownUntil-now, a.breakerUntil-now)
	if model != "" {
		wait = maxF(wait, a.modelCooldowns[model]-now)
	}
	return wait
}

// ClearError clears a model cooldown, or every cooldown when model is "".
func (a *Account) ClearError(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model != "" {
		delete(a.modelCooldowns, model)
	} else {
		a.modelCooldowns = map[string]float64{}
	}
	if a.LastError != "" || a.CooldownUntil != 0 {
		a.LastError = ""
		a.CooldownUntil = 0
	}
}

// NoteSoftRate records a soft rate limit (429 family) and backs off
// exponentially: 600 s doubling, capped at 2 hours.
func (a *Account) NoteSoftRate(message string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastError = truncate(message, 200)
	a.softStreak++
	wait := softBackoff(a.softStreak)
	deadline := float64(time.Now().Unix()) + wait
	if deadline > a.CooldownUntil {
		a.CooldownUntil = deadline
	}
	return wait
}

// NoteFailure records a hard failure; the breaker trips from BreakerThreshold
// consecutive failures and the window grows with the streak (cap 6 hours).
func (a *Account) NoteFailure(message string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastError = truncate(message, 200)
	a.fails++
	if a.fails >= BreakerThreshold {
		wait := breakerBackoff(a.fails)
		deadline := float64(time.Now().Unix()) + wait
		if deadline > a.breakerUntil {
			a.breakerUntil = deadline
		}
	}
	return a.breakerUntil
}

// NoteUnknownFailure records an unknown failure: the account stays usable but
// is deprioritised, and the breaker counter is fed too (an unknown failure may
// still be a hard one).
func (a *Account) NoteUnknownFailure(message string) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastError = truncate(message, 200)
	a.degradeCount++
	a.fails++
	now := float64(time.Now().Unix())
	if a.degradeCount >= DegradeThreshold {
		deadline := now + degradeBackoff(a.degradeCount)
		if deadline > a.degradeUntil {
			a.degradeUntil = deadline
		}
	}
	if a.fails >= BreakerThreshold {
		deadline := now + breakerBackoff(a.fails)
		if deadline > a.breakerUntil {
			a.breakerUntil = deadline
		}
	}
	return a.degradeUntil
}

// Degraded reports whether the account is inside the downgrade window.
func (a *Account) Degraded() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.degradeUntil > float64(time.Now().Unix())
}

// NoteSuccess clears every account-level penalty.
//
// balance_until is deliberately untouched: the 402 parking spot has exactly one
// release path (ReviveBalanceCooldown), triggered by a balance refresh.
func (a *Account) NoteSuccess(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model != "" {
		delete(a.modelCooldowns, model)
	}
	a.softStreak = 0
	a.fails = 0
	a.degradeCount = 0
	a.breakerUntil = 0
	a.degradeUntil = 0
	a.LastError = ""
	a.CooldownUntil = 0
}

// NoteBalanceCooled parks the account until the next credit reset (402).
// A short cooldown is pointless: quota resets daily, so releasing it in a few
// minutes just hits another 402.
func (a *Account) NoteBalanceCooled(detail string, until *float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := float64(NextCheckinWindow(time.Now()).Epoch)
	if until != nil {
		deadline = *until
	}
	if deadline > a.BalanceUntil {
		a.BalanceUntil = deadline
	}
	a.BalanceReason = truncate(detail, 200)
	return a.BalanceUntil
}

// ReviveBalanceCooldown lifts the 402 park, but only when the balance refresh
// actually saw remain > 0.
func (a *Account) ReviveBalanceCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.BalanceUntil == 0 {
		return false
	}
	if a.sharedUnlimitedLocked() {
		a.BalanceUntil = 0
		a.BalanceReason = ""
		return true
	}
	if a.effectiveRemainLocked() <= 0 {
		return false
	}
	a.BalanceUntil = 0
	a.BalanceReason = ""
	return true
}

func softBackoff(streak int) float64 {
	return exponentialBackoff(streak, 600, 7200, 1)
}

func breakerBackoff(fails int) float64 {
	return exponentialBackoff(fails, 1800, 21600, BreakerThreshold)
}

func degradeBackoff(count int) float64 {
	return exponentialBackoff(count, 600, 7200, DegradeThreshold)
}

// exponentialBackoff is base * 2^(count-offset), capped, with at most 20
// doublings so a long streak cannot overflow.
func exponentialBackoff(count, base, cap, offset int) float64 {
	step := count - offset
	if step < 0 {
		step = 0
	}
	if step > 20 {
		step = 20
	}
	v := float64(base) * float64(uint64(1)<<uint(step))
	capF := float64(cap)
	if v > capF {
		v = capF
	}
	return v
}

// ---------------------------------------------------------------------------
// Check-in capability
// ---------------------------------------------------------------------------

// CheckinCapability returns the runtime-probed check-in availability.
// nil means "not probed yet — try once".
func (a *Account) CheckinCapability() (probed *bool, reason string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.checkinCap == nil {
		return nil, ""
	}
	if time.Since(time.Unix(int64(a.checkinCapAt), 0)) > 6*time.Hour {
		return nil, a.checkinCapReason
	}
	v := *a.checkinCap
	return &v, a.checkinCapReason
}

// MarkCheckinCapability records the probe result.
func (a *Account) MarkCheckinCapability(available bool, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checkinCap = &available
	a.checkinCapReason = reason
	a.checkinCapAt = float64(time.Now().Unix())
}

// CanCheckin reports whether the account still needs today's check-in.
func (a *Account) CanCheckin() bool {
	if capable, _ := a.CheckinCapability(); capable != nil && !*capable {
		return false
	}
	a.mu.RLock()
	last := a.LastCheckin
	a.mu.RUnlock()
	if last == "" {
		return true
	}
	return !strings.HasPrefix(last, time.Now().In(utc8).Format("2006-01-02"))
}

// StampCheckin records today's date in UTC+8.
func (a *Account) StampCheckin() {
	a.mu.Lock()
	a.LastCheckin = time.Now().In(utc8).Format("2006-01-02")
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Token management
// ---------------------------------------------------------------------------

// SetTokens replaces the credential pair (used by refresh and import paths).
func (a *Account) SetTokens(access, refresh string, expiresAt int64) {
	a.mu.Lock()
	a.AccessToken = access
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if expiresAt != 0 {
		a.ExpiresAt = expiresAt
	}
	a.LastError = ""
	a.CooldownUntil = 0
	a.mu.Unlock()
}

// TokenFamily classifies the refresh strategy by token prefix.
func (a *Account) TokenFamily() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case strings.HasPrefix(a.RefreshToken, "drt-"):
		return "device"
	case a.RefreshToken != "":
		return "job"
	case a.PersonalToken != "":
		return "pat"
	}
	return ""
}

// SessionDead reports whether an upstream message means the session is revoked
// (re-login required, refreshing is pointless).
func SessionDead(msg string) bool {
	if msg == "" {
		return false
	}
	return strings.Contains(msg, SessionDeadTOKENEXPIRE) ||
		strings.Contains(msg, SessionDeadCode) ||
		strings.Contains(msg, "Offline user session not found")
}

// LedgerIsUnknown distinguishes "unknown balance" from "zero balance".
//
// A revoked credential gets a degenerate quota body: all zeros (or missing
// keys) plus isQuotaExceeded=true and expiresAt=CN's never-expires placeholder.
// Treating that as zero would trip the guardrails and take the account out.
func LedgerIsUnknown(c *Credits) bool {
	if c == nil {
		return false
	}
	if c.RemainEffective != 0 || c.Size != 0 {
		return false
	}
	if !c.Exceeded {
		return false
	}
	return c.ExpiresAt >= LegacyExpiryNever
}

// EffectiveRemain returns the balance used by guardrails and the summary.
func (c *Credits) EffectiveRemain() int {
	if c == nil {
		return 0
	}
	return c.RemainEffective
}

func decodeCredits(raw map[string]any) *Credits {
	c := &Credits{
		Remain:          int(asInt(raw["remain"])),
		RemainEffective: int(asInt(raw["remain_effective"])),
		Used:            int(asInt(raw["used"])),
		Size:            int(asInt(raw["size"])),
		Exceeded:        asBool(raw["exceeded"]),
		ExpiresAt:       normalizeEpoch(raw["expires_at"]),
		Source:          asString(raw["source"]),
		UpdatedAt:       int64(asInt(raw["updated_at"])),
		UpdatedISO:      asString(raw["updated_iso"]),
	}
	if c.RemainEffective == 0 && raw["remain_effective"] == nil {
		c.RemainEffective = c.Remain
	}
	if v, ok := raw["usage_pct"]; ok && v != nil {
		f := asFloat(v)
		c.UsagePct = &f
	}
	if shared, ok := raw["shared"].(map[string]any); ok {
		c.Shared = &SharedQuota{
			Remain:    int(asInt(shared["remain"])),
			Used:      int(asInt(shared["used"])),
			Size:      int(asInt(shared["size"])),
			Available: truthyDefault(shared["available"], true),
			Unlimited: asBool(shared["unlimited"]),
			Source:    asString(shared["source"]),
		}
	}
	if pkgs, ok := raw["packages"].([]any); ok {
		for _, p := range pkgs {
			if pm, ok := p.(map[string]any); ok {
				c.Packages = append(c.Packages, pm)
			}
		}
	}
	if comp, ok := raw["composition"].([]any); ok {
		for _, b := range comp {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			c.Composition = append(c.Composition, Composition{
				Name:      asString(bm["name"]),
				Kind:      asString(bm["kind"]),
				Remain:    int(asInt(bm["remain"])),
				Used:      int(asInt(bm["used"])),
				Size:      int(asInt(bm["size"])),
				ExpiresAt: normalizeEpoch(bm["expires_at"]),
			})
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// AccountLike (COSY session interface)
// ---------------------------------------------------------------------------

// UIDValue etc. satisfy sign.AccountLike without importing the sign package.
func (a *Account) UIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UID
}

func (a *Account) NicknameValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Nickname
}

func (a *Account) AccessTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

func (a *Account) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

func (a *Account) UserTypeValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.UserType == "" {
		return DefaultUserType
	}
	return a.UserType
}

func (a *Account) OrgIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.OrganizationID
}

func (a *Account) OrgNameValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.OrganizationName
}

// DefaultUserType is the fallback plan label.
const DefaultUserType = "personal_professional_trial"

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func maxF(values ...float64) float64 {
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// MarshalJSON keeps the persisted shape stable for panel exports.
//
// It emits the PUBLIC view, not the persisted document. ToMap() carries the
// access / refresh / personal tokens, and a type that marshals to plaintext
// credentials is a leak waiting for one careless `json.Marshal(acc)` — a log
// field, an error wall, a debug handler, a new panel route that forgot the
// scrub. Anyone who genuinely needs the persisted document (Save, the
// secrets=1 export) calls ToMap() explicitly, which is a visible choice.
//
// Verified: /accounts and every other panel route already used Public(); this
// only closes the implicit path.
func (a *Account) MarshalJSON() ([]byte, error) { return json.Marshal(a.Public()) }

// ExpiryUnix returns the access token's expiry (0 = unknown).
func (a *Account) ExpiryUnix() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// IsCampaignBlocked reports whether a campaign is in its "already claimed by
// this person" cooldown.
func (a *Account) IsCampaignBlocked(campaignID string, now float64) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.CampaignBlockedUntil == nil {
		return false
	}
	until, ok := a.CampaignBlockedUntil[campaignID]
	return ok && until > now
}

// CampaignCode returns the stored redemption code for a campaign.
func (a *Account) CampaignCode(campaignID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.CampaignCodes == nil {
		return ""
	}
	return a.CampaignCodes[campaignID]
}

// SetNote updates the display-only note.
func (a *Account) SetNote(note string) {
	a.mu.Lock()
	if len(note) > 120 {
		note = note[:120]
	}
	a.Note = note
	a.mu.Unlock()
}

// SetGroup updates the account group.
func (a *Account) SetGroup(group string) {
	a.mu.Lock()
	a.Group = group
	a.mu.Unlock()
}

// SetCheckinWhenDisabled toggles "keep checking in while paused".
func (a *Account) SetCheckinWhenDisabled(on bool) {
	a.mu.Lock()
	a.CheckinWhenDisabled = on
	a.mu.Unlock()
}

// EnabledValue reports the enabled flag.
func (a *Account) EnabledValue() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Enabled
}

// Disable marks the account as needing a fresh login (session dead).
func (a *Account) Disable(reason string) {
	a.mu.Lock()
	a.Enabled = false
	a.LastError = truncate(reason, 200)
	a.mu.Unlock()
}

// UIDValueOr returns the UID or a fallback.
func (a *Account) UIDValueOr(fallback string) string {
	if a.UID != "" {
		return a.UID
	}
	return fallback
}

// PoolCandidate adapts an Account to the route package's Candidate interface.
type PoolCandidate struct{ Account *Account }

// UID implements route.AccountLike.
func (c PoolCandidate) UID() string { return c.Account.UID }

// Group implements route.AccountLike.
func (c PoolCandidate) Group() string {
	c.Account.mu.RLock()
	defer c.Account.mu.RUnlock()
	return c.Account.Group
}

// RealmOf implements route.AccountLike.
func (c PoolCandidate) RealmOf() string { return c.Account.Realm }

// IsReady implements route.Candidate.
func (c PoolCandidate) IsReady(model string) bool { return c.Account.Ready(model) }

// IsDegraded implements route.Candidate.
func (c PoolCandidate) IsDegraded() bool { return c.Account.Degraded() }

// UTC8 is the fixed timezone the official daily reset uses.
func UTC8() *time.Location { return utc8 }
