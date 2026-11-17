package qoder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LoginTTLSeconds bounds how long a device-authorization flow stays pending.
const LoginTTLSeconds = 600

// DefaultUserType is the assumption used before /userinfo answers.
const DefaultUserType = "personal_professional_trial"

// DeviceLogin is a device-authorization flow in progress. The caller owns
// persistence of the PKCE verifier if it wants to resume across restarts; the
// built-in SessionStore does that in memory only.
type DeviceLogin struct {
	State       string `json:"state"`
	AuthURL     string `json:"authUrl"`
	Realm       string `json:"realm"`
	Platform    string `json:"platform"`
	VPCEndpoint string `json:"vpcEndpoint"`

	verifier string
	nonce    string
	created  time.Time
	cfg      RealmConfig
	vpc      *string
}

// LoginPoll is one poll result.
type LoginPoll struct {
	// Status is ok | pending | expired | unknown | cancelled | error.
	Status  string `json:"status"`
	Message string `json:"message"`
	// Credential is set when Status is ok.
	Credential *Credential `json:"credential,omitempty"`
	// Profile carries nickname/user type/org from /userinfo.
	Profile *UserProfile `json:"profile,omitempty"`
}

// UserProfile is the account metadata /userinfo reports.
type UserProfile struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	UserType   string `json:"userType,omitempty"`
	OrgID      string `json:"organizationId,omitempty"`
	OrgName    string `json:"organizationName,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Realm      string `json:"realm,omitempty"`
	Plan       string `json:"plan,omitempty"`
	Platform   string `json:"platform,omitempty"`
	Source     string `json:"source,omitempty"`
	VPCEndpoit string `json:"vpcEndpoint,omitempty"`
}

// SessionStore keeps in-flight device logins. The default is in-memory; a host
// that restarts often can supply its own.
type SessionStore interface {
	Put(state string, login *DeviceLogin)
	Get(state string) (*DeviceLogin, bool)
	Delete(state string) bool
	// Prune drops expired entries and returns how many were removed.
	Prune(ttl time.Duration) int
}

// MemorySessions is the default in-memory SessionStore.
type MemorySessions struct {
	mu      sync.Mutex
	entries map[string]*DeviceLogin
}

// NewMemorySessions builds an empty store.
func NewMemorySessions() *MemorySessions {
	return &MemorySessions{entries: map[string]*DeviceLogin{}}
}

// Put stores a login.
func (s *MemorySessions) Put(state string, login *DeviceLogin) {
	s.mu.Lock()
	s.entries[state] = login
	s.mu.Unlock()
}

// Get returns a login.
func (s *MemorySessions) Get(state string) (*DeviceLogin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.entries[state]
	return l, ok
}

// Delete removes a login, reporting whether it existed.
func (s *MemorySessions) Delete(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[state]
	delete(s.entries, state)
	return ok
}

// Prune drops entries older than ttl.
func (s *MemorySessions) Prune(ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.entries {
		if time.Since(v.created) > ttl {
			delete(s.entries, k)
			n++
		}
	}
	return n
}

// MakePKCE returns (verifier, challenge) for S256.
func MakePKCE() (string, string, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// StartDeviceLogin builds the PKCE device-authorization URL and remembers the
// verifier in the session store.
//
// The two realms differ in URL parameters (from the official client):
//
//	CN   : challenge, challenge_method, nonce (dashed uuid), redirect_uri,
//	       client_id, machine_id
//	Intl : challenge, challenge_method, nonce (32-hex), client_id, machine_id
func (c *Client) StartDeviceLogin(ctx context.Context, sessions SessionStore,
	realm, platform string, vpc *string) (*DeviceLogin, error) {
	if sessions == nil {
		return nil, fmt.Errorf("start device login: nil session store")
	}
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
		nonce = RandomUUID()
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
		q = append(q, "machine_id="+urlQueryEscape(c.machineID(realm, vpc)))
	}
	authURL := cfg.Website + "/device/selectAccounts?" + strings.Join(q, "&")
	state := fmt.Sprintf("qd-%d", time.Now().UnixNano())

	sessions.Prune(LoginTTLSeconds * time.Second)
	if platform == "" {
		platform = "CLI"
	}
	login := &DeviceLogin{
		State: state, AuthURL: authURL, Realm: realm, Platform: platform,
		VPCEndpoint: cfg.VPCInstance,
		verifier:    verifier, nonce: nonce, created: time.Now(), cfg: cfg, vpc: vpc,
	}
	sessions.Put(state, login)
	return login, nil
}

// PollDeviceLogin advances a device-authorization flow. It performs no
// persistence: the caller decides whether to store the returned credential.
func (c *Client) PollDeviceLogin(ctx context.Context, sessions SessionStore,
	state string) *LoginPoll {
	state = strings.TrimSpace(state)
	login, ok := sessions.Get(state)
	if !ok {
		return &LoginPoll{Status: "unknown", Message: "state not recognised - start the login again"}
	}
	if time.Since(login.created) > LoginTTLSeconds*time.Second {
		sessions.Delete(state)
		return &LoginPoll{Status: "expired", Message: "login window expired - start again"}
	}

	q := "nonce=" + urlQueryEscape(login.nonce) +
		"&verifier=" + urlQueryEscape(login.verifier) +
		"&challenge_method=S256"
	data, err := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(login.cfg) + PathDevicePoll + "?" + q,
		Headers: map[string]string{
			"Accept": "application/json", "User-Agent": "QoderWork",
		},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		if he, isHTTP := err.(*HTTPStatusError); isHTTP {
			// 404/202 means "user has not authorized yet" — keep polling.
			if he.Code == 404 || he.Code == 202 {
				return &LoginPoll{Status: "pending", Message: "等待浏览器完成 Qoder 设备授权"}
			}
			return &LoginPoll{Status: "error",
				Message: fmt.Sprintf("poll http %d %s", he.Code, truncateText(he.Body, 160))}
		}
		return &LoginPoll{Status: "pending", Message: "poll error: " + err.Error()}
	}

	token := firstNonEmpty(asString(data["token"]), asString(data["device_token"]))
	if token == "" {
		return &LoginPoll{Status: "pending", Message: "waiting for token"}
	}

	cred := Credential{
		UID:          asString(data["user_id"]),
		Realm:        login.Realm,
		AccessToken:  token,
		RefreshToken: asString(data["refresh_token"]),
		ExpiresAt:    DeviceExpiry(data),
		VPCOverride:  login.vpc,
	}
	profile := &UserProfile{
		UID: cred.UID, UserType: DefaultUserType, Domain: login.cfg.Domain,
		Realm: login.Realm, Platform: login.Platform, Source: "oauth",
		VPCEndpoit: login.cfg.VPCInstance,
	}
	// /userinfo is best-effort: a token that polls but cannot read the profile
	// is still usable, so a failure here must not fail the login.
	if ui, uiErr := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(login.cfg) + PathUserinfo,
		Headers: map[string]string{
			"Accept": "application/json", "User-Agent": c.uaOf(login.cfg),
			"Authorization": "Bearer " + token,
		},
		Timeout: 15 * time.Second,
	}); uiErr == nil {
		cred.UID = firstNonEmpty(asString(ui["id"]), cred.UID)
		profile.UID = firstNonEmpty(asString(ui["id"]), profile.UID)
		profile.Nickname = asString(ui["name"])
		profile.UserType = firstNonEmpty(asString(ui["user_type"]), DefaultUserType)
		profile.OrgID = firstNonEmpty(asString(ui["organization_id"]), asString(ui["organizationId"]))
		profile.OrgName = firstNonEmpty(asString(ui["organization_name"]), asString(ui["organizationName"]))
	}
	if cred.UID == "" {
		cred.UID = "q-" + randomHex(24)
		profile.UID = cred.UID
	}
	if profile.Nickname == "" {
		profile.Nickname = "u" + tail(cred.UID, 8)
	}

	if current, ok := sessions.Get(state); !ok || current != login {
		return &LoginPoll{Status: "cancelled", Message: "login was cancelled"}
	}
	sessions.Delete(state)
	return &LoginPoll{Status: "ok", Credential: &cred, Profile: profile}
}

// CancelDeviceLogin aborts an in-flight device authorization.
func CancelDeviceLogin(sessions SessionStore, state string) bool {
	if sessions == nil {
		return false
	}
	return sessions.Delete(strings.TrimSpace(state))
}

// ExchangePAT turns a pt- personal token into a jobToken credential.
//
// A PAT survives jrt- expiry because the refresh path re-exchanges it, so a
// host can keep it as the last-resort credential.
func (c *Client) ExchangePAT(ctx context.Context, pat, realm string, vpc *string) (*Credential, *UserProfile, error) {
	pat = strings.TrimSpace(pat)
	if !strings.HasPrefix(pat, "pt-") {
		return nil, nil, fmt.Errorf("PAT must start with pt-")
	}
	cfg, err := RealmConfigFor(realm, vpc)
	if err != nil {
		return nil, nil, err
	}
	data, err := c.http.DoJSON(ctx, Request{
		Method: "POST", URL: primaryOpenAPI(cfg) + PathJobExchange,
		Headers: map[string]string{
			"Content-Type": "application/json", "Accept": "application/json",
			"User-Agent": c.uaOf(cfg),
		},
		Body:    []byte(fmt.Sprintf(`{"personal_token":%q}`, pat)),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	token := asString(data["token"])
	if token == "" {
		return nil, nil, fmt.Errorf("jobToken exchange returned no token")
	}
	cred := &Credential{
		UID: asString(data["user_id"]), Realm: realm, AccessToken: token,
		RefreshToken: asString(data["refresh_token"]), PersonalToken: pat,
		ExpiresAt: DeviceExpiry(data), VPCOverride: vpc,
	}
	profile := &UserProfile{
		UID: cred.UID, UserType: DefaultUserType, Domain: cfg.Domain,
		Realm: realm, Platform: "CLI", Source: "pat", VPCEndpoit: cfg.VPCInstance,
	}
	if ui, uiErr := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(cfg) + PathUserinfo,
		Headers: map[string]string{
			"Accept": "application/json", "User-Agent": c.uaOf(cfg),
			"Authorization": "Bearer " + token,
		},
		Timeout: 15 * time.Second,
	}); uiErr == nil {
		cred.UID = firstNonEmpty(asString(ui["id"]), cred.UID)
		profile.UID = firstNonEmpty(asString(ui["id"]), profile.UID)
		profile.Nickname = asString(ui["name"])
		profile.UserType = firstNonEmpty(asString(ui["user_type"]), DefaultUserType)
		profile.OrgID = firstNonEmpty(asString(ui["organization_id"]), asString(ui["organizationId"]))
		profile.OrgName = firstNonEmpty(asString(ui["organization_name"]), asString(ui["organizationName"]))
	}
	if cred.UID == "" {
		cred.UID = "q-" + randomHex(24)
		profile.UID = cred.UID
	}
	if profile.Nickname == "" {
		profile.Nickname = "u" + tail(cred.UID, 8)
	}
	return cred, profile, nil
}

// Refresh rotates the access token.
//
// drt- goes through deviceToken/refresh; jrt- or a PAT goes through
// jobToken/refresh, falling back to a fresh PAT exchange. A PAT never
// overwrites an active OAuth session — it is the last resort for jrt- expiry.
func (c *Client) Refresh(ctx context.Context, cred Credential) (Tokens, error) {
	cfg, err := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if err != nil {
		return Tokens{}, err
	}
	base := primaryOpenAPI(cfg)

	if strings.HasPrefix(cred.RefreshToken, "drt-") {
		return c.postToken(ctx, cfg, base+PathDeviceRefresh,
			map[string]any{"refresh_token": cred.RefreshToken}, "device", cred)
	}
	if cred.RefreshToken != "" {
		if tok, err := c.postToken(ctx, cfg, base+PathJobRefresh,
			map[string]any{"refresh_token": cred.RefreshToken}, "job", cred); err == nil {
			return tok, nil
		}
	}
	if cred.PersonalToken != "" {
		return c.postToken(ctx, cfg, base+PathJobExchange,
			map[string]any{"personal_token": cred.PersonalToken}, "job", cred)
	}
	if cred.RefreshToken == "" && cred.PersonalToken == "" {
		return Tokens{}, fmt.Errorf("no refresh credential; sign in again")
	}
	return Tokens{}, fmt.Errorf("refresh failed for realm %s", cred.Realm)
}

func (c *Client) postToken(ctx context.Context, cfg RealmConfig, url string,
	payload map[string]any, kind string, cred Credential) (Tokens, error) {
	data, err := c.doJSON(ctx, "POST", url, map[string]string{
		"Content-Type": "application/json", "Accept": "application/json",
		"User-Agent": c.uaOf(cfg),
	}, payload, 30*time.Second)
	if err != nil {
		return Tokens{}, err
	}
	var token, refresh string
	var exp int64
	if kind == "device" {
		token = firstNonEmpty(asString(data["token"]), asString(data["device_token"]))
		refresh = firstNonEmpty(asString(data["refresh_token"]), cred.RefreshToken)
		exp = DeviceExpiry(data)
	} else {
		token = asString(data["token"])
		refresh = firstNonEmpty(asString(data["refresh_token"]), cred.RefreshToken)
		if v, ok := data["expires_in"]; ok && v != nil {
			exp = time.Now().Unix() + asInt(v)/1000
		} else {
			exp = cred.ExpiresAt
		}
	}
	if token == "" {
		return Tokens{}, fmt.Errorf("refresh returned no token")
	}
	return Tokens{AccessToken: token, RefreshToken: refresh, ExpiresAt: exp, Kind: kind}, nil
}

// FetchProfile reads /userinfo for an existing credential.
func (c *Client) FetchProfile(ctx context.Context, cred Credential) (*UserProfile, error) {
	cred, err := requireCredential("fetch profile", cred)
	if err != nil {
		return nil, err
	}
	cfg, err := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if err != nil {
		return nil, err
	}
	ui, err := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(cfg) + PathUserinfo,
		Headers: map[string]string{
			"Accept": "application/json", "User-Agent": c.uaOf(cfg),
			"Authorization": "Bearer " + cred.AccessToken,
		},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &UserProfile{
		UID:      firstNonEmpty(asString(ui["id"]), cred.UID),
		Nickname: asString(ui["name"]),
		UserType: firstNonEmpty(asString(ui["user_type"]), DefaultUserType),
		OrgID:    firstNonEmpty(asString(ui["organization_id"]), asString(ui["organizationId"])),
		OrgName:  firstNonEmpty(asString(ui["organization_name"]), asString(ui["organizationName"])),
		Domain:   cfg.Domain, Realm: cred.Realm, VPCEndpoit: cfg.VPCInstance,
	}, nil
}

// DeviceExpiry reads a deviceToken response's lifetime.
//
// expires_in is milliseconds; expires_at is RFC3339; the default is 30 days.
func DeviceExpiry(data map[string]any) int64 {
	if v, ok := data["expires_in"]; ok && v != nil {
		return time.Now().Unix() + asInt(v)/1000
	}
	for _, key := range []string{"expires_at", "expiresAt"} {
		if s, ok := data[key].(string); ok && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t.Unix()
			}
		}
		if v, ok := data[key]; ok && v != nil {
			if n := normalizeEpoch(v); n > 0 {
				return n
			}
		}
	}
	return time.Now().Unix() + 30*86400
}

// machineID reuses the official client's machine id when present so the login
// does not look like a brand-new device.
func (c *Client) machineID(realm string, vpc *string) string {
	if c.nativeMachine != nil {
		if id := strings.TrimSpace(c.nativeMachine(realm, vpc)); id != "" {
			return id
		}
	}
	if id := localMachineID(realm, vpc); id != "" {
		return id
	}
	return RandomUUID()
}

// localMachineID reads machine_id from the official CLI's auth directory.
func localMachineID(realm string, vpc *string) string {
	cfg, err := RealmConfigFor(realm, vpc)
	if err != nil {
		return ""
	}
	root, err := os.UserHomeDir()
	if err != nil || root == "" {
		return ""
	}
	dirs := []string{filepath.Join(root, cfg.HomeDir, ".auth")}
	if cfg.VPCInstance != "" {
		dirs = append([]string{
			filepath.Join(root, cfg.HomeDir, ".vpc", cfg.VPCInstance, ".auth"),
		}, dirs...)
	}
	for _, dir := range dirs {
		if raw, readErr := os.ReadFile(filepath.Join(dir, "machine_id")); readErr == nil {
			if id := strings.TrimSpace(string(raw)); id != "" {
				return id
			}
		}
	}
	return ""
}

// RandomUUID returns a v4-shaped UUID.
func RandomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x-0000-4000-a000-%012x", time.Now().UnixNano(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(buf)[:n]
}

// urlQueryEscape escapes a query value the way Python's urlencode does.
func urlQueryEscape(s string) string {
	return strings.ReplaceAll(urlQueryEscapeStd(s), "+", "%20")
}

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
