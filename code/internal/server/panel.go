package server

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// serveDashboard returns the single-file web panel.
func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	path := assetPath("web", "dashboard.html")
	if path == "" {
		writeText(w, http.StatusNotFound, "dashboard.html not found next to the executable\n")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "dashboard read failed\n")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// serveI18n returns a language pack.
func (s *Server) serveI18n(w http.ResponseWriter, r *http.Request, name string) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if name == "manifest" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"default": "en",
			"packs": []map[string]any{
				{"code": "en", "label": "English"},
			},
		})
		return
	}
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		s.writeError(w, http.StatusBadRequest, "invalid pack name", "invalid_request_error")
		return
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	path := assetPath("i18n", name)
	if path == "" {
		s.writeError(w, http.StatusNotFound, "pack not found", "invalid_request_error")
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "pack read failed", "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// handlePanelStatus answers whether the panel needs a login.
//
// It answers without a token on purpose: the browser must know whether to show
// the login page at all.
func (s *Server) handlePanelStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"panel_password_required":   true,
		"panel_password_is_default": s.settings.PanelPasswordIsDefault(),
		"authenticated":             s.panelAuthorized(r),
		"api_key_set":               len(s.settings.APIKeys()) > 0 || s.apiKeyString() != "",
		"version":                   s.cfg.Version,
	})
}

// handlePanelLogin exchanges the panel password for a token.
func (s *Server) handlePanelLogin(w http.ResponseWriter, r *http.Request) {
	// Refuse before reading the body: a locked-out client must not get a free
	// password comparison (and must not be able to burn CPU on huge bodies).
	key := loginThrottleKey(r)
	if ok, wait := s.loginThrottle.allowed(key); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		s.writeError(w, http.StatusTooManyRequests,
			"too many wrong panel passwords; retry in "+itoa(int(wait.Seconds())+1)+"s",
			"invalid_request_error")
		return
	}
	body, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	password := asStringAny(body["password"])
	if !s.settings.VerifyPanelPassword(password) {
		if wait := s.loginThrottle.fail(key); wait > 0 {
			s.log.Warn("panel", "login locked out for %s after repeated failures (%s)",
				key, wait)
		}
		s.writeError(w, http.StatusUnauthorized, "wrong panel password", "invalid_request_error")
		return
	}
	s.loginThrottle.succeed(key)
	token := s.panel.Create()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "token": token, "expires_in": s.panel.TTL(),
	})
}

// handlePanelLogout invalidates every panel session.
//
// A per-token revoke would only live in this process, so a restart would revive
// it; rotating the signing secret is what actually logs every browser out.
func (s *Server) handlePanelLogout(w http.ResponseWriter, r *http.Request) {
	s.panel.RevokeAll()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePanelPassword changes the panel password.
func (s *Server) handlePanelPassword(w http.ResponseWriter, r *http.Request) {
	body, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	current := asStringAny(body["current"])
	next := asStringAny(body["password"])
	if !s.settings.VerifyPanelPassword(current) {
		s.writeError(w, http.StatusUnauthorized, "wrong panel password", "invalid_request_error")
		return
	}
	if strings.TrimSpace(next) == "" {
		s.writeError(w, http.StatusBadRequest, "password must not be empty", "invalid_request_error")
		return
	}
	if err := s.settings.SetPanelPassword(next); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	s.panel.RevokeAll()
	token := s.panel.Create()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// handleHealth reports version, realm, accounts and the protocol snapshots.
//
// The payload is split in two on purpose. Anonymous callers get operational
// counters only, because /health is the one management-adjacent route that is
// also reachable cross-origin (browsers poll it) and needs no credential:
//
//	scheduler.logs  every recent task line — account nicknames, upstream error
//	                bodies ("token is not active"), and state_file's absolute path
//	keepalive       session IDs, which are the client's conversation keys
//	risk.last       the raw upstream error detail
//	env             runs a temp-file probe and an outbound HEAD per call
//
// None of that is credential material, but together it names accounts, leaks
// the deployment's filesystem layout, and lets any web page make this process
// touch its own data directory. Identified callers (panel session, valid API
// key, loopback) still get everything, so the panel and the launcher are
// unaffected.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"ok":               true,
		"service":          "qoder-proxy",
		"version":          s.cfg.Version,
		"pid":              os.Getpid(),
		"started_at":       s.startedAt.Unix(),
		"started_iso":      s.startedAt.Format("2006-01-02 15:04:05"),
		"realm":            s.currentRealm(),
		"accounts":         len(s.pool.Accounts()),
		"accounts_ready":   s.pool.CountReady("", "", nil),
		"api_key_required": s.authRequired(),
		"image_preflight":  protocolImageStatus(),
		"tool_guards":      guardsSnapshot(),
	}
	// A keyed request additionally learns which account it is using.
	//
	// !s.authRequired() is deliberately NOT enough: switched-off auth means
	// "not checked", not "authenticated". A --host 0.0.0.0 instance with no
	// key used to hand the account uid to any LAN client.
	if s.healthIdentityVisible(r) {
		info["risk"] = s.riskSnapshot()
		info["env"] = s.envReport()
		info["keepalive"] = s.keepalive.Status()
		info["scheduler"] = s.scheduler.Status()
		rep := s.pool.RepresentativeMatching(s.currentRealm())
		if rep != nil {
			info["uid"] = rep.UID
			info["domain"] = rep.Domain
			info["issuer"] = "qoder"
			info["expires_at"] = rep.ExpiryUnix()
		} else {
			info["uid"] = nil
		}
	}
	s.writeJSON(w, http.StatusOK, info)
}

// healthIdentityVisible reports whether /health may name the account in use.
//
// A valid panel session or a recognised API key counts. Loopback counts too,
// so the local panel and the launcher keep working without one. Everything
// else gets the anonymous snapshot: no uid, domain or expiry.
func (s *Server) healthIdentityVisible(r *http.Request) bool {
	if s.panelAuthorized(r) {
		return true
	}
	if s.identifyKey(r) != nil {
		return true
	}
	return clientIsLoopback(r)
}

// envReport runs the deployment self-checks.
//
// Clock skew breaks COSY signatures outright (they are time-based), and an
// unwritable accounts directory means the device identity cannot persist, so a
// container rebuild looks like a new device.
func (s *Server) envReport() map[string]any {
	writable := true
	if fi, err := os.Stat(s.cfg.AccountsDir); err != nil || !fi.IsDir() {
		writable = false
	} else if f, err := os.CreateTemp(s.cfg.AccountsDir, ".probe-*"); err != nil {
		writable = false
	} else {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
	}
	identityCached := false
	if _, err := os.Stat(s.cfg.AccountsDir + "/machine_identity.json"); err == nil {
		identityCached = true
	}
	proxy := false
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			proxy = true
			break
		}
	}
	return map[string]any{
		"container":         inContainer(),
		"proxy":             proxy,
		"accounts_writable": writable,
		"identity_cached":   identityCached,
		"clock_skew_s":      s.clockSkew(),
		"traffic_guard":     trafficGuardMode(),
	}
}

func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	return false
}

func trafficGuardMode() string {
	if v := strings.TrimSpace(os.Getenv("QD_TRAFFIC_GUARD")); v != "" {
		return strings.ToLower(v)
	}
	if v := strings.TrimSpace(os.Getenv("QD_TEAM_GUARD")); v != "" {
		return strings.ToLower(v)
	}
	return "auto"
}

// clockSkew reports the measured upstream clock offset, caching it for 10 min.
func (s *Server) clockSkew() any {
	s.skewMu.Lock()
	defer s.skewMu.Unlock()
	if s.skewAt > 0 && time.Since(time.Unix(s.skewAt, 0)) < 10*time.Minute {
		if s.skewSet {
			return s.skewSecs
		}
		return nil
	}
	skew, err := measureClockSkew()
	s.skewAt = time.Now().Unix()
	if err != nil || skew == nil {
		s.skewSet = false
		return nil
	}
	s.skewSecs = *skew
	s.skewSet = true
	return s.skewSecs
}

// handleRealmGet reports the global exit and whether auto-routing is on.
func (s *Server) handleRealmGet(w http.ResponseWriter, r *http.Request) {
	auto := envBool("QD_AUTO_REALM", true)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"current": s.currentRealm(),
		"auto":    auto,
		"mode":    map[bool]string{true: "auto", false: "fixed"}[auto],
	})
}

// handleRealmSet switches the global exit.
func (s *Server) handleRealmSet(w http.ResponseWriter, r *http.Request) {
	body, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	realm := normalizeRealm(asStringAny(body["realm"]))
	if realm == "" {
		realm = normalizeRealm(r.URL.Query().Get("realm"))
	}
	if realm == "" {
		s.writeError(w, http.StatusBadRequest, "realm must be cn or intl", "invalid_request_error")
		return
	}
	s.setRealm(realm)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "current": realm})
}

// ---------------------------------------------------------------------------
// small helpers used by the panel surface
// ---------------------------------------------------------------------------

// clientIsLoopback reports whether the caller is on this machine.
//
// The agent-configuration endpoints write files on the gateway host, so a remote
// caller must never reach them.
func clientIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

func mustJSON(v any) []byte {
	blob, _ := json.Marshal(v)
	return blob
}
