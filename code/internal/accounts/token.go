package accounts

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AccountHeaders returns the plain Bearer headers for business endpoints.
//
// The User-Agent must match the official client of the account's own realm:
// using Go's default "Go-http-client/2.0" would be an obvious gateway tell.
func (a *Account) AccountHeaders() map[string]string {
	cfg := a.RealmConfig()
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    cfg.UA,
		"Authorization": "Bearer " + a.AccessToken,
	}
}

// DesktopClientType is what the desktop client reports (CLI=5, QoderWork=6).
const DesktopClientType = "10"

// DefaultDesktopVersion is the desktop client version sent with campaign
// requests; QD_DESKTOP_VERSION overrides it.
const DefaultDesktopVersion = "0.4.8"

// DesktopVersion returns the effective desktop client version.
func DesktopVersion() string {
	if v := strings.TrimSpace(getenv("QD_DESKTOP_VERSION")); v != "" {
		return v
	}
	return DefaultDesktopVersion
}

// DesktopHeaders builds the header set the official desktop uses for campaign
// and check-in endpoints. Machine identity headers are only sent when a real
// machine identity is available: sending a derived one makes the "daily claim"
// campaign evaporate.
func (a *Account) DesktopHeaders() map[string]string {
	h := map[string]string{
		"Accept":          "application/json",
		"User-Agent":      "Qoder",
		"Authorization":   "Bearer " + a.AccessToken,
		"Cosy-ClientType": DesktopClientType,
		"Cosy-Version":    DesktopVersion(),
	}
	if a.MachineIdentitySource == "native" {
		// Filled in by the machine-identity provider once it has sampled a real
		// identity; the gateway never invents Cosy-Machine* values here.
		for k, v := range nativeMachineHeaders() {
			if v != "" {
				h[k] = v
			}
		}
	}
	return h
}

// Refresh rotates the access token.
//
// drt- goes through deviceToken/refresh; jrt- or a PAT goes through
// jobToken/refresh, falling back to a fresh PAT exchange. A PAT never
// overwrites an active OAuth session — it is the last resort for jrt- expiry.
func (a *Account) Refresh() error {
	cfg := a.RealmConfig()
	base := cfg.OpenAPI

	a.mu.RLock()
	refreshToken := a.RefreshToken
	personalToken := a.PersonalToken
	a.mu.RUnlock()

	if strings.HasPrefix(refreshToken, "drt-") {
		return a.postToken(base+PathDeviceRefresh,
			map[string]any{"refresh_token": refreshToken}, "device")
	}
	if refreshToken != "" {
		if err := a.postToken(base+PathJobRefresh,
			map[string]any{"refresh_token": refreshToken}, "job"); err == nil {
			return nil
		}
	}
	if personalToken != "" {
		return a.postToken(base+PathJobExchange,
			map[string]any{"personal_token": personalToken}, "job")
	}
	if refreshToken == "" && personalToken == "" {
		a.setLastError("no refresh token; sign in again")
	}
	return fmt.Errorf("no refresh credential for %s", a.UID)
}

func (a *Account) setLastError(msg string) {
	a.mu.Lock()
	a.LastError = truncate(msg, 200)
	a.mu.Unlock()
}

func (a *Account) postToken(url string, payload map[string]any, kind string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cfg := a.RealmConfig()
	data, err := DefaultClient.DoJSON(Request{
		Method: "POST",
		URL:    url,
		Headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"User-Agent":   cfg.UA,
		},
		Body:    body,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		if he, ok := err.(*HTTPStatusError); ok {
			a.setLastError(fmt.Sprintf("refresh failed: HTTP %d %s", he.Code, truncate(he.Body, 160)))
			if (he.Code == 401 || he.Code == 403) && SessionDead(he.Body) {
				a.mu.Lock()
				a.Enabled = false
				a.LastError = "session dead (TOKEN_EXPIRE): re-login required"
				a.mu.Unlock()
				if a.Path != "" {
					_ = a.Save(dirOf(a.Path))
				}
			}
			return err
		}
		a.setLastError("refresh failed: " + err.Error())
		return err
	}

	a.mu.RLock()
	oldRefresh := a.RefreshToken
	oldExpiry := a.ExpiresAt
	a.mu.RUnlock()

	var token string
	var refresh string
	var exp int64
	if kind == "device" {
		token = firstNonEmpty(asString(data["token"]), asString(data["device_token"]))
		refresh = firstNonEmpty(asString(data["refresh_token"]), oldRefresh)
		exp = deviceExpiry(data)
	} else {
		token = asString(data["token"])
		refresh = firstNonEmpty(asString(data["refresh_token"]), oldRefresh)
		if v, ok := data["expires_in"]; ok && v != nil {
			exp = time.Now().Unix() + asInt(v)/1000
		} else {
			exp = oldExpiry
		}
	}
	if token == "" {
		a.setLastError("refresh returned no token")
		return fmt.Errorf("refresh returned no token")
	}
	a.SetTokens(token, refresh, exp)
	if a.Path != "" {
		_ = a.Save(dirOf(a.Path))
	}
	// The cached COSY session carries the old token; it must be rebuilt.
	if SessionInvalidator != nil {
		SessionInvalidator(a.UID)
	}
	return nil
}

// SessionInvalidator is wired by the server to drop the COSY session cache
// entry after a token rotation (avoids importing sign from this package).
var SessionInvalidator func(uid string)

// deviceExpiry reads the deviceToken response's lifetime.
// expires_in is milliseconds; expires_at is RFC3339; the default is 30 days.
func deviceExpiry(data map[string]any) int64 {
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}

// nativeMachineHeaders is replaced by the machine-identity provider at startup.
var nativeMachineHeaders = func() map[string]string { return nil }
