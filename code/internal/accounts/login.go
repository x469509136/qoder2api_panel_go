package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LoginTTLSeconds bounds how long a device-authorization flow stays pending.
const LoginTTLSeconds = 600

// LoginState is one in-flight device authorization.
type LoginState struct {
	Created     float64
	Verifier    string
	Nonce       string
	Realm       string
	Platform    string
	Config      RealmConfig
	VPCOverride *string
}

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

var loginMu sync.Mutex
var logins = map[string]*LoginState{}

// MakePKCE returns (verifier, challenge) for S256.
func MakePKCE() (string, string, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// StartLogin builds the PKCE device-authorization URL.
//
// The two realms differ in URL parameters (from the official client):
//
//	CN   : challenge, challenge_method, nonce (dashed uuid), redirect_uri,
//	       client_id, machine_id
//	Intl : challenge, challenge_method, nonce (32-hex), client_id, machine_id
func (p *Pool) StartLogin(realm, platform string, vpc *string) (*LoginStart, error) {
	if realm != RealmCN && realm != RealmINTL {
		realm = RealmCN
	}
	cfg, err := RealmConfigFor(realm, vpc)
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := MakePKCE()
	if err != nil {
		return nil, err
	}
	nonce := randomHex(32)
	if cfg.NonceDashed {
		nonce = randomUUID()
	}
	q := []string{
		"challenge=" + urlQueryEscape(challenge),
		"challenge_method=S256",
		"nonce=" + urlQueryEscape(nonce),
	}
	if cfg.SendRedirectURI {
		q = append(q, "redirect_uri="+urlQueryEscape(cfg.RedirectURI))
	}
	if cfg.SendClientID {
		q = append(q, "client_id="+urlQueryEscape(cfg.ClientID))
		q = append(q, "machine_id="+urlQueryEscape(localMachineID(realm, vpc)))
	}
	authURL := cfg.Website + "/device/selectAccounts?" + strings.Join(q, "&")
	state := fmt.Sprintf("qd-%d", time.Now().UnixNano())

	loginMu.Lock()
	for k, v := range logins {
		if time.Since(time.Unix(int64(v.Created), 0)) > LoginTTLSeconds*time.Second {
			delete(logins, k)
		}
	}
	logins[state] = &LoginState{
		Created:     float64(time.Now().Unix()),
		Verifier:    verifier,
		Nonce:       nonce,
		Realm:       realm,
		Platform:    platform,
		Config:      cfg,
		VPCOverride: vpc,
	}
	loginMu.Unlock()

	if platform == "" {
		platform = "CLI"
	}
	return &LoginStart{
		State:       state,
		AuthURL:     authURL,
		Realm:       realm,
		Platform:    platform,
		VPCEndpoint: cfg.VPCInstance,
	}, nil
}

// localMachineID reuses the official client's machine id when present so the
// login does not look like a brand-new device.
func localMachineID(realm string, vpc *string) string {
	for _, dir := range CLIAuthDirs(realm, vpc) {
		if raw, err := os.ReadFile(filepath.Join(dir, "machine_id")); err == nil {
			if id := strings.TrimSpace(string(raw)); id != "" {
				return id
			}
		}
	}
	return randomUUID()
}

// PollLogin advances a device-authorization flow.
func (p *Pool) PollLogin(state string) *LoginResult {
	state = strings.TrimSpace(state)
	loginMu.Lock()
	info := logins[state]
	loginMu.Unlock()
	if info == nil {
		return &LoginResult{Status: "unknown", Message: "state not recognised - start the login again"}
	}
	if time.Since(time.Unix(int64(info.Created), 0)) > LoginTTLSeconds*time.Second {
		loginMu.Lock()
		delete(logins, state)
		loginMu.Unlock()
		return &LoginResult{Status: "expired", Message: "login window expired - start again"}
	}
	q := "nonce=" + urlQueryEscape(info.Nonce) +
		"&verifier=" + urlQueryEscape(info.Verifier) +
		"&challenge_method=S256"
	url := info.Config.OpenAPI + PathDevicePoll + "?" + q
	data, err := DefaultClient.DoJSON(Request{
		Method: "GET",
		URL:    url,
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": "QoderWork",
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		if he, ok := err.(*HTTPStatusError); ok {
			// 404/202 means "user has not authorized yet" — keep polling.
			if he.Code == 404 || he.Code == 202 {
				return &LoginResult{Status: "pending", Message: "等待浏览器完成 Qoder 设备授权"}
			}
			return &LoginResult{Status: "error",
				Message: fmt.Sprintf("poll http %d %s", he.Code, truncate(he.Body, 160))}
		}
		return &LoginResult{Status: "pending", Message: "poll error: " + err.Error()}
	}
	token := firstNonEmpty(asString(data["token"]), asString(data["device_token"]))
	if token == "" {
		return &LoginResult{Status: "pending", Message: "waiting for token"}
	}
	uid := asString(data["user_id"])
	nickname := ""
	userType := DefaultUserType
	orgID, orgName := "", ""
	if ui, err := DefaultClient.DoJSON(Request{
		Method: "GET",
		URL:    info.Config.OpenAPI + PathUserinfo,
		Headers: map[string]string{
			"Accept":        "application/json",
			"User-Agent":    info.Config.UA,
			"Authorization": "Bearer " + token,
		},
		Timeout: 15 * time.Second,
	}); err == nil {
		uid = firstNonEmpty(asString(ui["id"]), uid)
		nickname = asString(ui["name"])
		userType = firstNonEmpty(asString(ui["user_type"]), DefaultUserType)
		orgID = firstNonEmpty(asString(ui["organization_id"]), asString(ui["organizationId"]))
		orgName = firstNonEmpty(asString(ui["organization_name"]), asString(ui["organizationName"]))
	}
	if uid == "" {
		uid = "q-" + randomHex(24)
	}
	if nickname == "" {
		nickname = "u" + uid[max(0, len(uid)-8):]
	}
	accData := map[string]any{
		"uid":              uid,
		"nickname":         nickname,
		"domain":           info.Config.Domain,
		"realm":            info.Realm,
		"vpcEndpoint":      info.VPCOverride,
		"platform":         info.Platform,
		"accessToken":      token,
		"refreshToken":     asString(data["refresh_token"]),
		"expiresAt":        deviceExpiry(data),
		"source":           "oauth",
		"enabled":          true,
		"userType":         userType,
		"organizationId":   orgID,
		"organizationName": orgName,
	}
	if info.Platform == "" {
		accData["platform"] = "CLI"
	}
	acc, err := NewAccount(accData, "")
	if err != nil {
		return &LoginResult{Status: "error", Message: err.Error()}
	}

	loginMu.Lock()
	if logins[state] != info {
		loginMu.Unlock()
		return &LoginResult{Status: "cancelled", Message: "login was cancelled"}
	}
	delete(logins, state)
	loginMu.Unlock()

	if err := p.Add(acc); err != nil {
		return &LoginResult{Status: "error", Message: err.Error()}
	}
	return &LoginResult{Status: "ok", Account: acc.Public()}
}

// CancelLogin aborts an in-flight device authorization.
func CancelLogin(state string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	_, ok := logins[strings.TrimSpace(state)]
	delete(logins, strings.TrimSpace(state))
	return ok
}

// ImportPAT exchanges a pt- personal token for a jobToken and stores the
// account. A PAT survives jrt- expiry because the refresh path re-exchanges it.
func (p *Pool) ImportPAT(pat, realm string, vpc *string) (*Account, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return nil, fmt.Errorf("PAT must start with pt-")
	}
	cfg, err := RealmConfigFor(realm, vpc)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"personal_token": pat})
	data, err := DefaultClient.DoJSON(Request{
		Method: "POST",
		URL:    cfg.OpenAPI + PathJobExchange,
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"User-Agent":   cfg.UA,
		},
		Body:    body,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	token := asString(data["token"])
	if token == "" {
		return nil, fmt.Errorf("jobToken exchange returned no token")
	}
	uid, nickname := "", ""
	userType := DefaultUserType
	orgID, orgName := "", ""
	if ui, err := DefaultClient.DoJSON(Request{
		Method: "GET",
		URL:    cfg.OpenAPI + PathUserinfo,
		Headers: map[string]string{
			"Accept":        "application/json",
			"User-Agent":    cfg.UA,
			"Authorization": "Bearer " + token,
		},
		Timeout: 15 * time.Second,
	}); err == nil {
		uid = asString(ui["id"])
		nickname = asString(ui["name"])
		userType = firstNonEmpty(asString(ui["user_type"]), DefaultUserType)
		orgID = firstNonEmpty(asString(ui["organization_id"]), asString(ui["organizationId"]))
		orgName = firstNonEmpty(asString(ui["organization_name"]), asString(ui["organizationName"]))
	}
	if uid == "" {
		uid = "q-" + randomHex(24)
	}
	if nickname == "" {
		nickname = "u" + uid[max(0, len(uid)-8):]
	}
	acc, err := NewAccount(map[string]any{
		"uid":              uid,
		"nickname":         nickname,
		"domain":           cfg.Domain,
		"realm":            realm,
		"vpcEndpoint":      vpc,
		"platform":         "CLI",
		"accessToken":      token,
		"refreshToken":     asString(data["refresh_token"]),
		"personalToken":    pat,
		"expiresAt":        deviceExpiry(data),
		"source":           "pat",
		"enabled":          true,
		"userType":         userType,
		"organizationId":   orgID,
		"organizationName": orgName,
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

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x-0000-4000-a000-%012x", time.Now().UnixNano(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// urlQueryEscape escapes a query value the way Python's urlencode does.
func urlQueryEscape(s string) string {
	return strings.ReplaceAll(urlQueryEscapeStd(s), "+", "%20")
}
