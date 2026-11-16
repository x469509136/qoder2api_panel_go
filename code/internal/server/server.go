package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/catalog"
	"github.com/xzs/bridge_qoder/internal/guards"
	"github.com/xzs/bridge_qoder/internal/keepalive"
	"github.com/xzs/bridge_qoder/internal/logx"
	"github.com/xzs/bridge_qoder/internal/protocol"
	"github.com/xzs/bridge_qoder/internal/route"
	"github.com/xzs/bridge_qoder/internal/scheduler"
	"github.com/xzs/bridge_qoder/internal/session"
	"github.com/xzs/bridge_qoder/internal/settings"
	"github.com/xzs/bridge_qoder/internal/sign"
	"github.com/xzs/bridge_qoder/internal/tasks"
	"github.com/xzs/bridge_qoder/internal/upstream"
	"github.com/xzs/bridge_qoder/internal/usage"
)

// Server is the gateway runtime.
type Server struct {
	cfg *Config
	log *logx.Logger

	settings     *settings.Store
	panel        *settings.PanelSessions
	pool         *accounts.Pool
	usage        *usage.Store
	catalog      *catalog.Registry
	route        *route.Manager
	sessions     *sign.SessionCache
	upstream     *upstream.Client
	tasks        *tasks.Runner
	scheduler    *scheduler.Scheduler
	keepalive    *keepalive.Manager
	sessionCache session.Cache

	// apiKey is the startup-level key (CLI or auto-generated LAN key). Once the
	// panel stores its own keys this becomes a fallback.
	apiKey          atomic.Value // string
	apiKeyFromPanel bool
	systemPrompt    string

	// realm is the global exit switch.
	realmMu sync.RWMutex
	realm   string

	startedAt time.Time
	pid       int

	httpServer *http.Server
	ready      chan struct{}

	// sentEffort records the reasoning effort actually written to the upstream
	// body, so the usage row reflects reality rather than a re-derivation.
	sentEffort string

	// convMu guards the per-conversation upstream identity map.
	convMu  sync.Mutex
	convMap map[string]*protocol.ConversationIdentity

	// currentKeyEntry is the key entry for the request in flight.
	currentKeyEntry *settings.KeyEntry

	// skewMu guards the cached clock-skew measurement.
	skewMu   sync.Mutex
	skewAt   int64
	skewSecs int64
	skewSet  bool

	// risk telemetry
	riskMu    sync.Mutex
	riskStats map[string]any

	// loginThrottle limits password guessing on /panel/login.
	loginThrottle *loginThrottle
}

// New builds a server from configuration.
func New(cfg *Config, logger *logx.Logger) (*Server, error) {
	if logger == nil {
		logger = logx.Discard()
	}
	s := &Server{
		cfg:           cfg,
		log:           logger,
		startedAt:     time.Now(),
		pid:           os.Getpid(),
		ready:         make(chan struct{}),
		loginThrottle: newLoginThrottle(),
		riskStats: map[string]any{
			"queue": 0, "rate": 0, "rejected": 0, "version_gate": 0,
			"timeout": 0, "last": nil,
		},
	}
	s.settings = settings.NewStore(cfg.AccountsDir)
	s.panel = settings.NewPanelSessions(s.settings)
	s.catalog = catalog.Default()
	s.route = route.NewManager(cfg.AccountsDir)
	s.sessions = sign.NewSessionCache()
	s.upstream = upstream.NewClient()
	s.keepalive = keepalive.NewManager()
	s.systemPrompt = cfg.SystemPrompt
	s.realm = cfg.DefaultRealm
	if s.realm != accounts.RealmCN && s.realm != accounts.RealmINTL {
		s.realm = accounts.RealmCN
	}
	s.catalog.SetWarningSink(func(msg string) { s.log.Warn("catalog", "%s", msg) })

	store, err := usage.NewStore(cfg.UsageDir)
	if err != nil {
		return nil, fmt.Errorf("usage store: %w", err)
	}
	s.usage = store

	pool, err := accounts.NewPool(cfg.AccountsDir, func(level, msg string) {
		s.log.Log(level, "account", msg)
	})
	if err != nil {
		return nil, fmt.Errorf("account pool: %w", err)
	}
	s.pool = pool
	accounts.SessionInvalidator = func(uid string) { s.sessions.Invalidate(uid) }

	if err := s.resolveStartupKeys(); err != nil {
		return nil, err
	}
	s.loadPersistedRealm()
	s.applyPoolSettings()
	s.sessionCache = s.buildSessionCache()

	s.tasks = tasks.NewRunner(s.pool, cfg.AccountsDir, cfg.UsageDir, func(level, msg string) {
		if level == "" {
			level = logx.LevelInfo
		}
		s.log.Log(level, "tasks", msg)
	})
	s.scheduler = scheduler.New(s.pool, cfg.AccountsDir, s.tasks)
	s.keepalive.Configure(s.keepaliveSend)
	return s, nil
}

// resolveStartupKeys applies the key precedence:
// CLI --api-key > panel-managed key > auto-generated LAN key.
func (s *Server) resolveStartupKeys() error {
	if s.cfg.PanelPassword != "" {
		if err := s.settings.SetPanelPassword(s.cfg.PanelPassword); err != nil {
			return err
		}
		s.log.Info("panel", "password set from --panel-password")
	}
	if stored, ok := s.settings.APIKeyOverride(); ok && s.cfg.APIKey == "" {
		s.apiKey.Store(stored)
		s.apiKeyFromPanel = true
		return nil
	}
	if s.cfg.APIKey != "" {
		s.apiKey.Store(s.cfg.APIKey)
		return nil
	}
	if s.cfg.LAN {
		key, created, err := s.settings.EnsureLauncherKey()
		if err != nil {
			return err
		}
		s.apiKey.Store(key)
		if created {
			s.log.Info("apikey", "generated a LAN API key and saved it to settings.json")
		}
		return nil
	}
	s.apiKey.Store("")
	return nil
}

// loadPersistedRealm restores the exit the panel last selected.
func (s *Server) loadPersistedRealm() {
	data := s.settings.Load()
	if v, ok := data["realm"].(string); ok {
		if v == accounts.RealmCN || v == accounts.RealmINTL {
			s.realm = v
		}
	}
}

// savePersistedRealm records the current exit.
func (s *Server) savePersistedRealm(realm string) {
	_ = s.settings.Update(func(data map[string]any) { data["realm"] = realm })
}

// applyPoolSettings pushes guardrails and the free-model list into every account.
func (s *Server) applyPoolSettings() {
	free := s.freeModelNames(s.currentRealm())
	s.pool.ApplySettings(s.settings, s.currentRealm(), free)
}

func (s *Server) buildSessionCache() session.Cache {
	cfg := session.FromEnv()
	if cfg.Mode == "off" {
		return nil
	}
	secret, err := s.settings.PanelSessionSecret()
	if err != nil || secret == "" {
		return nil
	}
	cache := session.Build(cfg, session.HashSecret([]byte(secret)))
	if cache == nil {
		s.log.Warn("session", "session cache disabled: configuration could not be built")
	}
	return cache
}

// currentRealm returns the global exit.
func (s *Server) currentRealm() string {
	s.realmMu.RLock()
	defer s.realmMu.RUnlock()
	return s.realm
}

// setRealm switches the global exit.
func (s *Server) setRealm(realm string) {
	if realm != accounts.RealmCN && realm != accounts.RealmINTL {
		return
	}
	s.realmMu.Lock()
	s.realm = realm
	s.realmMu.Unlock()
	s.savePersistedRealm(realm)
	s.applyPoolSettings()
}

// freeModelNames lists the free models of a realm.
func (s *Server) freeModelNames(realm string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range s.catalog.ModelsForRealm(realm) {
		if info := catalog.ModelInfo(raw); info.IsFree && info.Key != "" {
			out[info.Key] = true
			out[info.DisplayName] = true
		}
	}
	return out
}

// StartBackground launches the schedulers and warm-up.
func (s *Server) StartBackground() {
	go s.warmModels()
	if keepalive.Enabled() {
		s.keepalive.Start()
	}
	s.scheduler.Start()
	go s.refreshDailyUsageLoop()
}

func (s *Server) warmModels() {
	for _, realm := range []string{accounts.RealmCN, accounts.RealmINTL} {
		s.fetchModels(realm)
	}
}

// refreshDailyUsageLoop folds the usage log into per-account daily counters so the
// guardrails have numbers to work with.
func (s *Server) refreshDailyUsageLoop() {
	s.refreshDailyUsage()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.refreshDailyUsage()
	}
}

func (s *Server) refreshDailyUsage() {
	start := float64(usage.LocalMidnightUnix(0))
	type agg struct {
		tokens   int
		credits  float64
		perModel map[string]int
	}
	byAccount := map[string]*agg{}
	s.usage.ScanRows(func(row usage.Row) bool {
		if row.At < start || row.Account == "" || row.ErrType != "" {
			return true
		}
		a, ok := byAccount[row.Account]
		if !ok {
			a = &agg{perModel: map[string]int{}}
			byAccount[row.Account] = a
		}
		a.tokens += row.TotalTokens
		a.credits += row.Credit
		a.perModel[row.Model] += row.TotalTokens
		return true
	})
	for _, acc := range s.pool.Accounts() {
		a, ok := byAccount[acc.UID]
		if !ok {
			tokens := 0
			credits := 0.0
			acc.SetDailyUsage(&tokens, &credits, map[string]int{})
			continue
		}
		tokens := a.tokens
		credits := a.credits
		acc.SetDailyUsage(&tokens, &credits, a.perModel)
	}
}

// keepaliveSend re-sends a minimal request to renew the upstream cache.
func (s *Server) keepaliveSend(entry *keepalive.Entry) error {
	if entry == nil || entry.Account == nil {
		return fmt.Errorf("keepalive: no account")
	}
	payload := cloneMap(entry.Payload)
	payload["stream"] = false
	payload["max_tokens"] = 1
	payload["max_completion_tokens"] = 1
	acc := entry.Account
	modelKey := entry.ModelKey
	body, err := protocol.BuildQoderBody(payload, protocol.BuildContext{
		UID: acc.UID, UserType: acc.UserTypeValue(), ModelKey: modelKey,
		DisplayName:    s.displayNameFor(modelKey, entry.Realm),
		Realm:          entry.Realm,
		SessionID:      entry.SessionID,
		ToolResultKeep: 2000, ToolResultScope: "tail", ToolResultTail: 1,
		CosyVersion: sign.CosyVersion(),
	})
	if err != nil {
		return err
	}
	cfg := acc.RealmConfig()
	rawURL := cfg.Gateway + upstream.ChatPath
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := s.upstream.Post(ctx, upstream.Request{
		Body: body, Account: acc, ModelKey: modelKey, RawURL: rawURL,
		Sessions: s.sessions, Timeout: 120 * time.Second,
	})
	if err != nil {
		return err
	}
	defer resp.Close()
	holder := &upstream.Holder{}
	return upstream.Stream(ctx, resp.Body, holder, func(map[string]any) error { return nil })
}

// ---------------------------------------------------------------------------
// HTTP wiring
// ---------------------------------------------------------------------------

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.routeRequest)
	return mux
}

// ListenAndServe binds and serves.
func (s *Server) ListenAndServe() error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: SSE streams stay open for minutes.
		IdleTimeout: 120 * time.Second,
	}
	s.httpServer = srv
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	close(s.ready)
	return srv.Serve(ln)
}

// Shutdown stops the server and background work.
func (s *Server) Shutdown(ctx context.Context) error {
	s.keepalive.Stop()
	s.scheduler.Stop()
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// Ready is closed once the listener is up.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// routeRequest dispatches one request by path.
//
// Management routes are matched before inference so /agents/cli (public) is not
// swallowed by the /agents prefix.
func (s *Server) routeRequest(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	start := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("http", "panic on %s: %v", path, rec)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()

	w.Header().Set("Server", "qd-proxy/"+s.cfg.Version)
	s.applyCORS(w, r)

	switch {
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return

	// --- public probes ---
	case path == "/ping" || path == "/healthz" || path == "/livez" || path == "/readyz":
		writeText(w, http.StatusOK, "pong\n")

	// --- dashboard ---
	case path == "/" || path == "/dashboard" || path == "/ui":
		s.serveDashboard(w, r)
	case strings.HasPrefix(path, "/i18n/"):
		s.serveI18n(w, r, strings.TrimPrefix(path, "/i18n/"))

	// --- panel session ---
	case path == "/panel/status":
		s.handlePanelStatus(w, r)
	case path == "/panel/login":
		s.handlePanelLogin(w, r)
	case path == "/panel/logout":
		s.requirePanel(s.handlePanelLogout)(w, r)
	case path == "/panel/password":
		s.requirePanel(s.handlePanelPassword)(w, r)

	// --- public read-only ---
	case path == "/agents/cli":
		s.handleAgentsCLI(w, r)
	case path == "/agents/template":
		s.handleAgentsTemplate(w, r)

	// --- health / realm ---
	case path == "/health":
		s.handleHealth(w, r)
	case path == "/realm":
		if r.Method == http.MethodPost {
			s.requireKey(s.handleRealmSet)(w, r)
			return
		}
		s.handleRealmGet(w, r)

	// --- inference ---
	case path == "/v1/chat/completions" || path == "/chat/completions" ||
		path == "/v1/completions" || path == "/completions":
		s.requireKey(s.handleChat)(w, r)
	case path == "/v1/responses" || path == "/responses":
		s.requireKey(s.handleResponses)(w, r)
	case path == "/v1/messages":
		s.requireKey(s.handleAnthropic)(w, r)
	case path == "/v1/messages/count_tokens":
		s.requireKey(s.handleAnthropicCountTokens)(w, r)
	case path == "/v1/models" || path == "/models":
		s.requireKey(s.handleModels)(w, r)
	case path == "/models/refresh" || path == "/v1/models/refresh":
		s.requireKey(s.handleModelsRefresh)(w, r)

	default:
		if s.dispatchManagement(w, r) {
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown path: "+path, "invalid_request_error")
	}
	_ = start
}

// dispatchManagement handles the panel's management routes.
func (s *Server) dispatchManagement(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/admin/") {
		s.requirePanel(s.handleAdmin)(w, r)
		return true
	}
	switch {
	case strings.HasPrefix(path, "/accounts"):
		s.requirePanel(s.handleAccounts)(w, r)
		return true
	case strings.HasPrefix(path, "/tasks"), strings.HasPrefix(path, "/scheduler"):
		s.requirePanel(s.handleTasks)(w, r)
		return true
	case strings.HasPrefix(path, "/usage"), strings.HasPrefix(path, "/v1/usage"):
		s.requirePanel(s.handleUsage)(w, r)
		return true
	case strings.HasPrefix(path, "/settings"):
		s.requirePanel(s.handleSettings)(w, r)
		return true
	case strings.HasPrefix(path, "/logs"):
		s.requirePanel(s.handleLogs)(w, r)
		return true
	case strings.HasPrefix(path, "/diag"):
		s.requirePanel(s.handleDiag)(w, r)
		return true
	case strings.HasPrefix(path, "/agents"):
		s.requirePanel(s.handleAgents)(w, r)
		return true
	case strings.HasPrefix(path, "/update"):
		s.requirePanel(s.handleUpdate)(w, r)
		return true
	case strings.HasPrefix(path, "/credits"):
		s.requirePanel(s.handleCredits)(w, r)
		return true
	case path == "/pricing":
		s.requirePanel(s.handlePricing)(w, r)
		return true
	}
	return false
}

// applyCORS adds the permissive headers browser-based inference clients need.
//
// Only the inference surface and the public probes are cross-origin readable.
// The management surface (accounts, settings, logs, agents, admin, usage,
// tasks, diag, credits, panel) is deliberately excluded: those routes are
// authenticated by a header (X-Panel-Token), not a cookie, so a wildcard origin
// lets any page the operator happens to visit read them. With ?secrets=1 that
// includes plaintext upstream credentials. See corsAllowed below.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	if !s.corsAllowed(r.URL.Path) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

// corsAllowed reports whether a path may be read from a foreign origin.
//
// Allowlist, not blocklist: browser clients only need inference plus a couple of
// public probes, so everything else stays same-origin by default. Adding a new
// management route must not silently widen this.
func (s *Server) corsAllowed(path string) bool {
	for _, prefix := range []string{
		"/v1/chat/completions", "/v1/completions", "/v1/responses",
		"/v1/messages", "/v1/models", "/v1/usage",
		"/chat/completions", "/completions", "/responses", "/messages", "/models",
	} {
		if path == prefix {
			return true
		}
	}
	// Public, credential-free probes: safe to expose and useful for a browser
	// health widget.
	switch path {
	case "/ping", "/healthz", "/livez", "/readyz", "/health":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// suppliedKey extracts the presented API key.
func (s *Server) suppliedKey(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if v := strings.TrimSpace(auth); v != "" {
		return v
	}
	// Anthropic clients use x-api-key, not a bearer token. Missing this fallback
	// makes every /v1/messages call 401.
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	// A browser top-level navigation cannot set headers, so ?key= is accepted.
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// authRequired reports whether a key is needed at all.
func (s *Server) authRequired() bool {
	if s.settings.AuthDisabled() {
		return false
	}
	if len(s.settings.APIKeys()) > 0 {
		return true
	}
	return strings.TrimSpace(s.apiKeyString()) != ""
}

func (s *Server) apiKeyString() string {
	v, _ := s.apiKey.Load().(string)
	return v
}

// identifyKey resolves which configured key was presented.
func (s *Server) identifyKey(r *http.Request) *settings.KeyEntry {
	supplied := s.suppliedKey(r)
	if supplied == "" {
		return nil
	}
	extra := []string{}
	if k := strings.TrimSpace(s.apiKeyString()); k != "" {
		extra = append(extra, k)
	}
	return s.settings.MatchAPIKey(supplied, extra)
}

// keyEntry returns the key entry recorded for this request (set by requireKey).
func (s *Server) keyEntry(r *http.Request) *settings.KeyEntry {
	if v := r.Context().Value(ctxKeyEntry); v != nil {
		if entry, ok := v.(*settings.KeyEntry); ok {
			return entry
		}
	}
	return nil
}

type ctxKey int

const ctxKeyEntry ctxKey = iota

// requireKey enforces API-key auth on inference routes.
//
// A valid panel session also passes: the browser should not have to keep the key
// in localStorage.
func (s *Server) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.panelAuthorized(r) {
			next(w, r)
			return
		}
		entry := s.identifyKey(r)
		if entry == nil && s.authRequired() {
			s.writeError(w, http.StatusUnauthorized,
				"invalid api key", "invalid_request_error")
			return
		}
		if entry != nil {
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyEntry, entry))
		}
		next(w, r)
	}
}

// requirePanel enforces a panel session on management routes.
func (s *Server) requirePanel(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.panelAuthorized(r) {
			s.writeError(w, http.StatusUnauthorized,
				"panel password required", "invalid_request_error")
			return
		}
		next(w, r)
	}
}

func (s *Server) panelToken(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Panel-Token"))
}

func (s *Server) panelAuthorized(r *http.Request) bool {
	return s.panel.Valid(s.panelToken(r))
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	blob, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(blob)
}

// writeError renders an OpenAI-shaped error.
func (s *Server) writeError(w http.ResponseWriter, code int, message, errType string) {
	if errType == "" {
		errType = "server_error"
	}
	s.writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": message, "type": errType, "code": clientErrorCode(code, errType),
		},
	})
}

// clientErrorCode maps a status onto the code clients branch on.
//
// A non-credential error must not reach the client as 401/403: clients classify
// those as "my key is broken" and stop retrying.
func clientErrorCode(code int, errType string) any {
	authLike := errType == "invalid_request_error" || errType == "server_error"
	if authLike && (code == 401 || code == 403) {
		return 429
	}
	return code
}

func writeText(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// readPayload decodes a JSON object body up to the configured cap.
func (s *Server) readPayload(r *http.Request, allowList bool) (any, error) {
	if r.Body == nil {
		return map[string]any{}, nil
	}
	limited := io.LimitReader(r.Body, s.cfg.MaxPayloadBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(raw)) > s.cfg.MaxPayloadBytes {
		return nil, errBodyTooLarge
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	if allowList && strings.HasPrefix(trimmed, "[") {
		var list []any
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("invalid json: %w", err)
		}
		return list, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	return obj, nil
}

var errBodyTooLarge = fmt.Errorf("request body too large")

// payloadObject reads an object body or writes a 400.
func (s *Server) payloadObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := s.readPayload(r, false)
	if err != nil {
		if err == errBodyTooLarge {
			s.writeError(w, http.StatusRequestEntityTooLarge,
				"request body too large", "invalid_request_error")
			return nil, false
		}
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return nil, false
	}
	obj, ok := body.(map[string]any)
	if !ok {
		s.writeError(w, http.StatusBadRequest,
			"request body must be a JSON object", "invalid_request_error")
		return nil, false
	}
	return obj, true
}

func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// assetPath resolves a bundled asset next to the executable.
func assetPath(parts ...string) string {
	candidates := []string{filepath.Join(append([]string{"."}, parts...)...)}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append([]string{filepath.Join(append([]string{dir}, parts...)...)}, candidates...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// applyGuardLenientOrStrict turns a guard failure into the configured ending.
func (s *Server) toolEchoLenient() bool {
	if mode := s.settings.ToolEchoMode(); mode != "" {
		guards.SetToolEchoMode(mode)
	} else {
		guards.SetToolEchoMode("")
	}
	return guards.ToolEchoMode() == guards.ToolEchoLenient
}

// noteRisk records one risk/queue signal.
func (s *Server) noteRisk(kind, detail string) {
	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	if cur, ok := s.riskStats[kind].(int); ok {
		s.riskStats[kind] = cur + 1
	} else {
		s.riskStats[kind] = 1
	}
	if len(detail) > 120 {
		detail = detail[:120]
	}
	s.riskStats["last"] = map[string]any{
		"kind": kind, "at": time.Now().Format("01-02 15:04:05"), "detail": detail,
	}
}

// riskSnapshot copies the risk counters.
func (s *Server) riskSnapshot() map[string]any {
	s.riskMu.Lock()
	defer s.riskMu.Unlock()
	out := make(map[string]any, len(s.riskStats))
	for k, v := range s.riskStats {
		out[k] = v
	}
	return out
}
