package accounts

import (
	"context"
	"fmt"

	"github.com/xzs/bridge_qoder/internal/qoder"
)

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Login flows: the protocol runs in internal/qoder (DeviceLogin/Poll/PAT);
// this file keeps the historical Pool-facing API and persists what it returns.

// LoginStart is the response of start_login.
type LoginStart struct {
	State       string `json:"state"`
	AuthURL     string `json:"authUrl"`
	Realm       string `json:"realm"`
	Platform    string `json:"platform"`
	VPCEndpoint string `json:"vpcEndpoint"`
}

// LoginResult is one poll result.
type LoginResult struct {
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Account map[string]any `json:"account,omitempty"`
}

// loginSessions holds in-flight device authorizations.
var loginSessions = qoder.NewMemorySessions()

// StartLogin builds the PKCE device-authorization URL.
func (p *Pool) StartLogin(realm, platform string, vpc *string) (*LoginStart, error) {
	login, err := protocolClient.StartDeviceLogin(context.Background(), loginSessions, realm, platform, vpc)
	if err != nil {
		return nil, err
	}
	return &LoginStart{
		State: login.State, AuthURL: login.AuthURL, Realm: login.Realm,
		Platform: login.Platform, VPCEndpoint: login.VPCEndpoint,
	}, nil
}

// PollLogin advances a device-authorization flow and adds the account when the
// user finished authorising in the browser.
func (p *Pool) PollLogin(state string) *LoginResult {
	poll := protocolClient.PollDeviceLogin(context.Background(), loginSessions, state)
	if poll.Status != "ok" {
		return &LoginResult{Status: poll.Status, Message: poll.Message}
	}
	cred, profile := poll.Credential, poll.Profile

	accData := map[string]any{
		"uid":              cred.UID,
		"nickname":         profile.Nickname,
		"domain":           profile.Domain,
		"realm":            cred.Realm,
		"vpcEndpoint":      cred.VPCOverride,
		"platform":         firstNonEmpty(profile.Platform, "CLI"),
		"accessToken":      cred.AccessToken,
		"refreshToken":     cred.RefreshToken,
		"expiresAt":        cred.ExpiresAt,
		"source":           "oauth",
		"enabled":          true,
		"userType":         profile.UserType,
		"organizationId":   profile.OrgID,
		"organizationName": profile.OrgName,
	}
	acc, err := NewAccount(accData, "")
	if err != nil {
		return &LoginResult{Status: "error", Message: err.Error()}
	}
	if err := p.Add(acc); err != nil {
		return &LoginResult{Status: "error", Message: err.Error()}
	}
	return &LoginResult{Status: "ok", Account: acc.Public()}
}

// CancelLogin aborts an in-flight device authorization.
func CancelLogin(state string) bool { return qoder.CancelDeviceLogin(loginSessions, state) }

// ImportPAT exchanges a pt- personal token for a jobToken and adds the account.
// A PAT survives jrt- expiry because the refresh path re-exchanges it.
func (p *Pool) ImportPAT(pat, realm string, vpc *string) (*Account, error) {
	cred, profile, err := protocolClient.ExchangePAT(context.Background(), pat, realm, vpc)
	if err != nil {
		return nil, err
	}
	acc, err := NewAccount(map[string]any{
		"uid":              cred.UID,
		"nickname":         profile.Nickname,
		"domain":           profile.Domain,
		"realm":            realm,
		"vpcEndpoint":      vpc,
		"platform":         firstNonEmpty(profile.Platform, "CLI"),
		"accessToken":      cred.AccessToken,
		"refreshToken":     cred.RefreshToken,
		"personalToken":    cred.PersonalToken,
		"expiresAt":        cred.ExpiresAt,
		"source":           "pat",
		"enabled":          true,
		"userType":         profile.UserType,
		"organizationId":   profile.OrgID,
		"organizationName": profile.OrgName,
	}, "")
	if err != nil {
		return nil, err
	}
	if err := p.Add(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

// ImportRows upserts credential documents (panel JSON import).
//
// overwrite=false keeps existing uids untouched; the return value says how many
// were added, updated and skipped.
func (p *Pool) ImportRows(rows []map[string]any, realm, group string, overwrite bool) (added, updated, skipped int, errs []string) {
	for _, row := range rows {
		uid := asString(row["uid"])
		if uid == "" || asString(row["accessToken"]) == "" {
			skipped++
			continue
		}
		if tid, ok := row["expiresAt"]; ok {
			row["expiresAt"] = normalizeEpoch(tid)
		}
		if realm != "" {
			row["realm"] = realm
		}
		if group != "" {
			row["group"] = group
		}
		if row["realm"] == nil {
			row["realm"] = DetectRealmFromDomain(asString(row["domain"]))
		}
		existing := p.Get(uid)
		if existing != nil {
			if !overwrite {
				skipped++
				continue
			}
			existing.mu.Lock()
			existing.AccessToken = asString(row["accessToken"])
			if rt := asString(row["refreshToken"]); rt != "" {
				existing.RefreshToken = rt
			}
			if pt := asString(row["personalToken"]); pt != "" {
				existing.PersonalToken = pt
			}
			if e := normalizeEpoch(row["expiresAt"]); e != 0 {
				existing.ExpiresAt = e
			}
			if n := asString(row["nickname"]); n != "" {
				existing.Nickname = n
			}
			if g := asString(row["group"]); g != "" {
				existing.Group = g
			}
			existing.Enabled = true
			existing.LastError = ""
			existing.mu.Unlock()
			if err := existing.Save(p.dir); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", uid, err))
			}
			updated++
			continue
		}
		acc, err := NewAccount(row, "")
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", uid, err))
			skipped++
			continue
		}
		if err := p.Add(acc); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", uid, err))
			skipped++
			continue
		}
		added++
	}
	return
}

// Export returns the credential documents (with tokens) for backup.
func (p *Pool) Export() []map[string]any {
	out := []map[string]any{}
	for _, a := range p.Accounts() {
		out = append(out, a.ToMap())
	}
	return out
}
