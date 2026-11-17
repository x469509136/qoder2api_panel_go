package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Options configures a Client.
type Options struct {
	// HTTP is the transport. Nil falls back to DefaultHTTPClient.
	HTTP HTTP
	// UserAgentDefault is used when a realm config has no UA. Optional.
	UserAgentDefault string
	// DesktopVersion overrides the version sent on campaign/check-in calls.
	// Empty reads QD_DESKTOP_VERSION, then DefaultDesktopVersion.
	DesktopVersion string
	// MachineHeaders supplies the native Cosy-Machine* header set. It is only
	// consulted when Credential.MachineIdentitySource == "native"; returning nil
	// keeps the derived-identity behaviour (which upstream rejects for claims).
	MachineHeaders func() map[string]string
	// NativeMachineID supplies the machine id used during device login. Nil
	// generates one.
	NativeMachineID func(realm string, vpc *string) string
}

// Client talks to Qoder's business APIs. It holds no per-account state: every
// call takes a Credential and returns values, so a host keeps ownership of
// persistence, locking, cooldown and rotation.
type Client struct {
	http           HTTP
	uaDefault      string
	desktopVersion string
	machineHeaders func() map[string]string
	nativeMachine  func(realm string, vpc *string) string
}

// New builds a Client.
func New(opts Options) *Client {
	c := &Client{
		http:           opts.HTTP,
		uaDefault:      opts.UserAgentDefault,
		desktopVersion: strings.TrimSpace(opts.DesktopVersion),
		machineHeaders: opts.MachineHeaders,
		nativeMachine:  opts.NativeMachineID,
	}
	if c.http == nil {
		c.http = DefaultHTTPClient
	}
	if c.uaDefault == "" {
		c.uaDefault = "QoderWork/1.1.75"
	}
	return c
}

// DesktopClientType is what the desktop client reports (CLI=5, QoderWork=6).
const DesktopClientType = "10"

// DefaultDesktopVersion is the desktop client version sent with campaign
// requests; QD_DESKTOP_VERSION overrides it.
const DefaultDesktopVersion = "0.4.8"

// machineIdentityOmitted is the conservative default before a desktop request
// has actually been built.
const machineIdentityOmitted = "omitted"

// desktopVersionOf resolves the effective desktop version.
func (c *Client) desktopVersionOf() string {
	if c != nil && c.desktopVersion != "" {
		return c.desktopVersion
	}
	if v := strings.TrimSpace(os.Getenv("QD_DESKTOP_VERSION")); v != "" {
		return v
	}
	return DefaultDesktopVersion
}

// uaOf returns the realm's UA, or the client default.
func (c *Client) uaOf(cfg RealmConfig) string {
	if strings.TrimSpace(cfg.UA) != "" {
		return cfg.UA
	}
	return c.uaDefault
}

// AccountHeaders builds the plain Bearer header set for business endpoints.
//
// The User-Agent must match the official client of the account's own realm:
// Go's default "Go-http-client/2.0" would be an obvious gateway tell.
func (c *Client) AccountHeaders(cfg RealmConfig, cred Credential) map[string]string {
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json, text/plain, */*",
		"User-Agent":    c.uaOf(cfg),
		"Authorization": "Bearer " + cred.AccessToken,
	}
}

// DesktopHeaders builds the header set the official desktop uses for campaign
// and check-in endpoints.
//
// Native machine headers are attached only when the credential actually has a
// native identity: sending a derived one makes the daily claim campaign
// disappear, which is worse than omitting it.
func (c *Client) DesktopHeaders(cfg RealmConfig, cred Credential) map[string]string {
	h := map[string]string{
		"Accept":          "application/json",
		"User-Agent":      "Qoder",
		"Authorization":   "Bearer " + cred.AccessToken,
		"Cosy-ClientType": DesktopClientType,
		"Cosy-Version":    c.desktopVersionOf(),
	}
	if cred.MachineIdentitySource == "native" && c.machineHeaders != nil {
		for k, v := range c.machineHeaders() {
			if v != "" {
				h[k] = v
			}
		}
	}
	return h
}

// doJSON performs one JSON call against base+path.
func (c *Client) doJSON(ctx context.Context, method, url string,
	headers map[string]string, body any, timeout time.Duration) (map[string]any, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	return c.http.DoJSON(ctx, Request{
		Method: method, URL: url, Headers: headers,
		Body: raw, Timeout: timeout,
	})
}

// primaryOpenAPI is the host used for a single-shot call (login, refresh).
// Candidates are reserved for the discovery paths where falling back is safe.
func primaryOpenAPI(cfg RealmConfig) string {
	return strings.TrimRight(cfg.OpenAPI, "/")
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// endpointScope mirrors the key a host uses to remember discovered paths.
func endpointScope(realm, vpcInstance string) string {
	return EndpointScope(realm, vpcInstance)
}

// requireCredential rejects calls that cannot carry authentication.
func requireCredential(op string, cred Credential) (Credential, error) {
	if strings.TrimSpace(cred.AccessToken) == "" {
		return cred, fmt.Errorf("%s: access token required", op)
	}
	if cred.Realm == "" {
		cred.Realm = RealmCN
	}
	return cred, nil
}
