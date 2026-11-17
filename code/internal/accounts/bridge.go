package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xzs/bridge_qoder/internal/qoder"
)

// This file is the ONLY bridge between the account state machine and the
// protocol layer in internal/qoder.

// ---------------------------------------------------------------------------
// Realm / endpoint / value-type aliases
// ---------------------------------------------------------------------------
//
// account.go and pool.go predate the split and keep their historical names.
// Every alias maps to the qoder implementation — there is exactly one of each.

const (
	RealmCN   = qoder.RealmCN
	RealmINTL = qoder.RealmINTL

	PathDevicePoll     = qoder.PathDevicePoll
	PathDeviceRefresh  = qoder.PathDeviceRefresh
	PathJobExchange    = qoder.PathJobExchange
	PathJobRefresh     = qoder.PathJobRefresh
	PathUserinfo       = qoder.PathUserinfo
	PathQuota          = qoder.PathQuota
	PathPlan           = qoder.PathPlan
	PathCheckinStatus  = qoder.PathCheckinStatus
	PathCheckinClaim   = qoder.PathCheckinClaim
	PathProEligibility = qoder.PathProEligibility
	PathProClaim       = qoder.PathProClaim
	PathCampaigns      = qoder.PathCampaigns
	PathCampaignReward = qoder.PathCampaignReward
	PathCampaignClaim  = qoder.PathCampaignClaim
	PathCreditSummary  = qoder.PathCreditSummary
	PathCreditHeatmap  = qoder.PathCreditHeatmap
	PathAchievements   = qoder.PathAchievements

	VPCDomainSuffix = qoder.VPCDomainSuffix

	// CheckinWindowHourUTC8 is the fixed reset hour for daily credits.
	CheckinWindowHourUTC8 = qoder.CheckinWindowHourUTC8
	// LegacyExpiryNever is CN's "never expires" placeholder epoch.
	LegacyExpiryNever = qoder.LegacyExpiryNever
	// LoginTTLSeconds bounds how long a device-authorization flow stays pending.
	LoginTTLSeconds = qoder.LoginTTLSeconds
	// DefaultUserType is the assumption used before /userinfo answers.
	DefaultUserType = qoder.DefaultUserType
	// DesktopClientType is what the desktop client reports.
	DesktopClientType = qoder.DesktopClientType
	// DefaultDesktopVersion is the desktop client version sent with campaigns.
	DefaultDesktopVersion = qoder.DefaultDesktopVersion
)

// CreditSourceFile records which (host, path) pair worked per realm.
const CreditSourceFile = "credits-source.json"

type RealmConfig = qoder.RealmConfig

type (
	Credits             = qoder.Credits
	Composition         = qoder.Composition
	SharedQuota         = qoder.SharedQuota
	CheckinWindow       = qoder.CheckinWindow
	CheckinStatus       = qoder.CheckinStatus
	CheckinStatusResult = qoder.CheckinStatusResult
	Campaign            = qoder.Campaign
	CampaignBenefit     = qoder.CampaignBenefit
	CampaignList        = qoder.CampaignList
	// CampaignView is the historical name of CampaignList in this package;
	// tasks.ViewResult still uses it in its JSON shape.
	CampaignView          = qoder.CampaignList
	CampaignCode          = qoder.CampaignCode
	CampaignClaimResult   = qoder.CampaignClaimResult
	CampaignCheckinResult = qoder.CampaignCheckinResult
	CheckinResult         = qoder.CheckinResult
	CheckinReport         = qoder.CheckinReport
	CreditSource          = qoder.CreditSource
	CreditResult          = qoder.CreditResult
	CreditExpiry          = qoder.CreditExpiry
	ScannedCredential     = qoder.ScannedCredential
	UserProfile           = qoder.UserProfile
)

var (
	// CreditPaths is the candidate matrix used to discover the quota endpoint.
	CreditPaths = qoder.CreditPaths
	// QuotaContainers lists the recognised container labels per category.
	QuotaContainers = qoder.QuotaContainers
	// RealmConfigs is the compiled-in region table. It is exposed because the
	// e2e tests override the gateway host with a fake upstream.
	RealmConfigs = qoder.RealmConfigs
)

// ---------------------------------------------------------------------------
// Package-level functions
// ---------------------------------------------------------------------------

// NormalizeQuotaPayload splits an upstream quota body into the four categories.
func NormalizeQuotaPayload(payload map[string]any) ([]Composition, *SharedQuota) {
	return qoder.NormalizeQuotaPayload(payload)
}

// LedgerIsUnknown distinguishes "unknown balance" from "zero balance".
func LedgerIsUnknown(c *Credits) bool { return qoder.LedgerIsUnknown(c) }

// NextCheckinWindow returns the next "10:00 UTC+8" boundary.
func NextCheckinWindow(now time.Time) CheckinWindow { return qoder.NextCheckinWindow(now) }

// ExpiryOf reports when a snapshot's credits expire.
func ExpiryOf(c *Credits, now time.Time) CreditExpiry { return qoder.ExpiryOf(c, now) }

// NormalizeVPCOverride maps a stored value onto an instance name.
func NormalizeVPCOverride(value any) (*string, error) { return qoder.NormalizeVPCOverride(value) }

// RealmConfigFor returns the effective config for (realm, vpc override).
func RealmConfigFor(realm string, vpc *string) (RealmConfig, error) {
	return qoder.RealmConfigFor(realm, vpc)
}

// GetRealmConfig returns the compiled-in config for a realm.
func GetRealmConfig(realm string) RealmConfig { return qoder.GetRealmConfig(realm) }

// GatewayCandidates returns the inference host list for a realm.
func GatewayCandidates(realm string, cfg RealmConfig) []string {
	return qoder.GatewayCandidates(realm, cfg)
}

// OpenAPICandidates returns the business-endpoint base candidates.
func OpenAPICandidates(realm string, cfg RealmConfig) []string {
	return qoder.OpenAPICandidates(realm, cfg)
}

// DetectRealmFromDomain infers the realm from a credential domain.
func DetectRealmFromDomain(domain string) string { return qoder.DetectRealmFromDomain(domain) }

// CLIAuthDirs returns the official CLI credential directories to scan.
func CLIAuthDirs(realm string, vpc *string) []string { return qoder.CLIAuthDirs(realm, vpc) }

// ScanDesktopCredentials looks for credentials the official client stored.
func ScanDesktopCredentials() []ScannedCredential { return qoder.ScanDesktopCredentials() }

// AccountFromScan builds an Account from a desktop-scan hit. The scan only
// reports findings; nothing is imported without an explicit choice.
func AccountFromScan(sc *ScannedCredential) (*Account, error) {
	if sc == nil || !sc.Valid || sc.Credential == nil {
		return nil, fmt.Errorf("scan hit is not usable")
	}
	cfg := GetRealmConfig(sc.Realm)
	return NewAccount(map[string]any{
		"uid":          sc.Credential.UID,
		"nickname":     sc.Nickname,
		"domain":       cfg.Domain,
		"realm":        sc.Realm,
		"accessToken":  sc.Credential.AccessToken,
		"refreshToken": sc.Credential.RefreshToken,
		"expiresAt":    sc.Credential.ExpiresAt,
		"source":       "desktop-scan",
		"enabled":      true,
	}, "")
}

// VPCConfigured reports whether a deployment-level VPC default is active.
func VPCConfigured() bool { return qoder.VPCConfigured() }

// VPCInstanceName returns the deployment-level VPC instance ("" when none).
func VPCInstanceName() string { return qoder.VPCInstanceName() }

// IsConfiguredVCPHost reports whether host belongs to the configured VPC.
func IsConfiguredVCPHost(host string) bool { return qoder.IsConfiguredVCPHost(host) }

// MakePKCE returns (verifier, challenge) for S256.
func MakePKCE() (string, string, error) { return qoder.MakePKCE() }

// RandomUUID returns a v4-shaped UUID.
func RandomUUID() string { return qoder.RandomUUID() }

// DeviceExpiry reads a deviceToken response's lifetime.
func DeviceExpiry(data map[string]any) int64 { return qoder.DeviceExpiry(data) }

// DesktopVersion returns the effective desktop client version.
func DesktopVersion() string {
	if v := strings.TrimSpace(getenv("QD_DESKTOP_VERSION")); v != "" {
		return v
	}
	return qoder.DefaultDesktopVersion
}

// VPCConfigError holds a startup-time QD_CN_VPC_ENDPOINT parse failure.
func VPCConfigError() string { return qoder.VPCConfigError }

// ---------------------------------------------------------------------------
// Account → qoder delegation
// ---------------------------------------------------------------------------
//
// The Account keeps persistence, cooldowns and guardrails; the protocol calls
// below convert its state into a qoder.Credential, run the call, and write
// the results back.

// protocolClient is the shared business-API client. It is stateless, so one
// instance serves every account.
var protocolClient = qoder.New(qoder.Options{
	HTTP:            nil, // nil = qoder.DefaultHTTPClient
	MachineHeaders:  nativeMachineHeaders,
	NativeMachineID: nativeMachineID,
})

// credential snapshots the account state the protocol layer needs. Values are
// copied under the read lock: the qoder package never touches the Account.
func (a *Account) credential() qoder.Credential {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return qoder.Credential{
		UID:                   a.UID,
		Realm:                 a.Realm,
		AccessToken:           a.AccessToken,
		RefreshToken:          a.RefreshToken,
		PersonalToken:         a.PersonalToken,
		VPCOverride:           a.VPCOverride,
		MachineIdentitySource: a.MachineIdentitySource,
		ExpiresAt:             a.ExpiresAt,
	}
}

// CheckinStatusFor probes the legacy daily check-in endpoint.
func (a *Account) CheckinStatusFor() *CheckinStatusResult {
	return protocolClient.CheckinStatus(context.Background(), a.credential())
}

// Checkin claims today's daily credits (campaign platform first, legacy
// endpoint as fallback), stamps the local date when the reward is settled and
// refreshes the balance snapshot so the panel shows the new number.
func (a *Account) Checkin(accountsDir string) CheckinResult {
	report := protocolClient.Checkin(context.Background(), a.credential())
	if report.Stamped {
		a.StampCheckin()
	}
	if report.Stamped || report.Earned > 0 {
		a.refreshCreditsQuietly(accountsDir)
	}
	// OK means "this run produced a usable outcome". The campaign leg may have
	// been unreachable (the legacy fallback answered) without the run failing,
	// so Campaign.OK alone is not the verdict — report.Error is.
	res := CheckinResult{
		OK:            report.Error == "",
		Already:       report.Already,
		Msg:           report.Message,
		Error:         report.Error,
		RewardCredits: report.Earned,
		Campaign:      report.Campaign,
	}
	if report.UsesLegacy {
		res.Status = "legacy"
	}
	return res
}

// FetchCampaigns lists the official campaign platform's live campaigns.
func (a *Account) FetchCampaigns() *CampaignList {
	return protocolClient.FetchCampaigns(context.Background(), a.credential())
}

// ClaimCampaign claims one campaign's reward and persists the redemption code
// plus the per-person dedup marker on the account document.
func (a *Account) ClaimCampaign(campaignID string, accountsDir string) CampaignClaimResult {
	claim := protocolClient.ClaimCampaign(context.Background(), a.credential(), campaignID)
	if claim.Code != "" {
		a.mu.Lock()
		if a.CampaignCodes == nil {
			a.CampaignCodes = map[string]string{}
		}
		a.CampaignCodes[campaignID] = claim.Code
		a.mu.Unlock()
		_ = a.Save(dirOf(a.Path))
	}
	if claim.Blocked {
		a.mu.Lock()
		if a.CampaignBlockedUntil == nil {
			a.CampaignBlockedUntil = map[string]float64{}
		}
		a.CampaignBlockedUntil[campaignID] = float64(time.Now().Unix()) + 6*3600
		a.mu.Unlock()
	}
	if claim.OK {
		a.refreshCreditsQuietly(accountsDir)
	}
	return claim
}

// CampaignCheckin claims every claimable credits campaign for one account.
func (a *Account) CampaignCheckin(gap float64, onlyCredits bool) *CampaignCheckinResult {
	res := protocolClient.CampaignCheckin(context.Background(), a.credential(), gap, onlyCredits)
	if res.Stamped {
		a.StampCheckin()
	}
	return res
}

// FetchCredits GETs the quota endpoint and stores the snapshot on the account.
func (a *Account) FetchCredits(accountsDir string) *QuotaFetchResult {
	var hint *CreditSource
	if accountsDir != "" {
		if src, ok := LoadCreditSources(accountsDir)[a.EndpointScope()]; ok {
			hint = &src
		}
	}
	res := protocolClient.FetchCredits(context.Background(), a.credential(), hint)
	if res.Source != nil && accountsDir != "" {
		SaveCreditSource(accountsDir, a.EndpointScope(), res.Source.Host, res.Source.Path, res.Source.Fields)
	}
	if res.OK && res.Credits != nil {
		a.storeCredits(res.Credits)
	} else if res.Error != "" && strings.Contains(res.Error, "degenerate quota body") {
		a.mu.Lock()
		a.LastQuotaAnomaly = map[string]any{
			"reason": strings.TrimPrefix(res.Error, "degenerate quota body: "),
			"at":     float64(time.Now().Unix()),
			"http":   200,
		}
		a.mu.Unlock()
	}
	return &QuotaFetchResult{OK: res.OK, HTTP: res.HTTP, Error: res.Error, Credits: res.Credits}
}

// ProbeCreditEndpoints walks the candidate matrix and remembers the first hit.
func (a *Account) ProbeCreditEndpoints(accountsDir string, limit int) *QuotaFetchResult {
	res := protocolClient.ProbeCreditEndpoints(context.Background(), a.credential(), limit)
	if res == nil {
		return nil
	}
	if res.Source != nil && accountsDir != "" {
		SaveCreditSource(accountsDir, a.EndpointScope(), res.Source.Host, res.Source.Path, res.Source.Fields)
	}
	if res.OK && res.Credits != nil {
		a.storeCredits(res.Credits)
	}
	return &QuotaFetchResult{OK: res.OK, Error: res.Error, Credits: res.Credits}
}

// FetchPlan updates the plan name from /api/v2/user/plan.
func (a *Account) FetchPlan() string {
	name := protocolClient.FetchPlan(context.Background(), a.credential())
	if name != "" {
		a.mu.Lock()
		a.Plan = name
		a.mu.Unlock()
		_ = a.Save(dirOf(a.Path))
	}
	return name
}

// Refresh rotates the access token.
//
// The protocol call is qoder.Refresh; this wrapper keeps the account-side
// reactions that used to live in token.go: persist the new pair, drop the
// cached COSY session, and disable the account when the upstream says the
// session is revoked.
func (a *Account) Refresh() error {
	tok, err := protocolClient.Refresh(context.Background(), a.credential())
	if err != nil {
		code := 0
		body := ""
		if he, ok := err.(*qoder.HTTPStatusError); ok {
			code, body = he.Code, he.Body
		}
		if code != 0 {
			a.setLastError(fmt.Sprintf("refresh failed: HTTP %d %s", code, truncate(body, 160)))
		} else {
			a.setLastError("refresh failed: " + err.Error())
		}
		if code == 401 || code == 403 {
			if SessionDead(body) {
				a.mu.Lock()
				a.Enabled = false
				a.LastError = "session dead (TOKEN_EXPIRE): re-login required"
				a.mu.Unlock()
				if a.Path != "" {
					_ = a.Save(dirOf(a.Path))
				}
			}
		}
		return err
	}

	a.SetTokens(tok.AccessToken, tok.RefreshToken, tok.ExpiresAt)
	if a.Path != "" {
		_ = a.Save(dirOf(a.Path))
	}
	// The cached COSY session carries the old token; it must be rebuilt.
	if SessionInvalidator != nil {
		SessionInvalidator(a.UID)
	}
	return nil
}

// setLastError records a bounded diagnostic on the account.
func (a *Account) setLastError(msg string) {
	a.mu.Lock()
	a.LastError = truncate(msg, 200)
	a.mu.Unlock()
}

// refreshCreditsQuietly refreshes the balance snapshot, swallowing failures:
// a failed refresh must not turn a successful check-in into an error.
func (a *Account) refreshCreditsQuietly(accountsDir string) {
	defer func() { _ = recover() }()
	a.FetchCredits(accountsDir)
}

// ---------------------------------------------------------------------------
// credits-source.json persistence (host-side bookkeeping)
// ---------------------------------------------------------------------------
//
// internal/qoder returns the winning (host, path) as CreditResult.Source; the
// gateway persists it so the next refresh skips the probe.

// CreditsSourcePath returns accounts/credits-source.json.
func CreditsSourcePath(accountsDir string) string {
	return filepath.Join(accountsDir, CreditSourceFile)
}

// LoadCreditSources reads the per-scope endpoint discovery record.
func LoadCreditSources(accountsDir string) map[string]CreditSource {
	out := map[string]CreditSource{}
	raw, err := os.ReadFile(CreditsSourcePath(accountsDir))
	if err != nil {
		return out
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	for scope, body := range doc {
		var src CreditSource
		if err := json.Unmarshal(body, &src); err == nil && src.Host != "" {
			out[scope] = src
		}
	}
	return out
}

// SaveCreditSource remembers a working (host, path) pair for one scope.
func SaveCreditSource(accountsDir, scope, host, path string, fields []string) {
	doc := map[string]any{}
	if raw, err := os.ReadFile(CreditsSourcePath(accountsDir)); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	doc[scope] = map[string]any{
		"host": host, "path": path, "fields": fields,
		"at": float64(time.Now().Unix()),
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(accountsDir, 0o755)
	tmp := CreditsSourcePath(accountsDir) + ".tmp"
	if os.WriteFile(tmp, out, 0o600) == nil {
		_ = os.Rename(tmp, CreditsSourcePath(accountsDir))
	}
}

// ---------------------------------------------------------------------------
// Native machine identity seam
// ---------------------------------------------------------------------------

// nativeMachineHeaders is replaced by the machine-identity provider at startup.
var nativeMachineHeaders = func() map[string]string { return nil }

// nativeMachineID reads the official CLI's machine id so a login does not look
// like a brand-new device.
func nativeMachineID(realm string, vpc *string) string {
	for _, dir := range CLIAuthDirs(realm, vpc) {
		if raw, err := os.ReadFile(filepath.Join(dir, "machine_id")); err == nil {
			if id := strings.TrimSpace(string(raw)); id != "" {
				return id
			}
		}
	}
	return ""
}
