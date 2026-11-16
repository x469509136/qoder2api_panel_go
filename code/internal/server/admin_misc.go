package server

import (
	"net/http"
	"os"
	"runtime"

	"github.com/xzs/bridge_qoder/internal/agents"
	"github.com/xzs/bridge_qoder/internal/logx"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

// innerErrorSnapshot forwards the upstream inner-error counters.
func innerErrorSnapshot() map[string]any { return upstream.InnerErrorSnapshot() }

// handleTasks dispatches /tasks and /scheduler.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet {
		switch path {
		case "/tasks":
			s.writeJSON(w, http.StatusOK, s.tasksView(r))
			return
		case "/scheduler":
			s.writeJSON(w, http.StatusOK, s.scheduler.Status())
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown tasks path: "+path, "invalid_request_error")
		return
	}
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	switch path {
	case "/tasks/run":
		targets := s.resolveAccountTargets(payload)
		result := s.tasks.RunCheckin(targets)
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "accounts": result.AccountsCount,
			"credit_added": result.CreditAdded, "failed": result.Failed,
			"logs": result.Logs,
		})
	case "/tasks/travel":
		targets := s.resolveAccountTargets(payload)
		result := s.tasks.RunCampaigns(targets)
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "accounts": result.AccountsCount,
			"credit_added": result.CreditAdded, "failed": result.Failed,
			"logs": result.Logs,
		})
	case "/scheduler/trigger":
		s.writeJSON(w, http.StatusOK, s.scheduler.TriggerNow())
	case "/scheduler/toggle":
		on, okToggle := payload["enabled"].(bool)
		if !okToggle {
			on = !s.scheduler.Enabled()
		}
		s.scheduler.SetEnabled(on)
		s.writeJSON(w, http.StatusOK, s.scheduler.Status())
	case "/scheduler/config":
		status := s.scheduler.SetSchedule(asStringAny(payload["checkin_times"]),
			asStringAny(payload["keepalive_times"]), asStringAny(payload["cron"]),
			intAny(payload["jitter_secs"]))
		s.writeJSON(w, http.StatusOK, status)
	default:
		s.writeError(w, http.StatusNotFound, "unknown tasks path: "+path, "invalid_request_error")
	}
}

// tasksView aggregates the per-account task state.
func (s *Server) tasksView(r *http.Request) map[string]any {
	realm := normalizeRealm(r.URL.Query().Get("realm"))
	rows := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		if realm != "" && acc.Realm != realm {
			continue
		}
		rows = append(rows, s.tasksViewForAccount(acc))
	}
	return map[string]any{"accounts": rows, "realm": firstNonEmptyStr(realm, "all")}
}

// handleLogs serves the in-memory log ring.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/logs" && r.Method == http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]any{
			"entries": s.log.Query(logxQuery(r)),
			"tags":    s.log.Tags(),
		})
	case r.URL.Path == "/logs/export":
		path := s.log.FilePath()
		if path == "" {
			s.writeError(w, http.StatusNotFound, "file logging is disabled", "invalid_request_error")
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		// A log written by an older build (before logx.Redact existed) may still
		// contain tokens. Scrub on the way out so downloading the file cannot
		// hand an operator's credential to whoever is looking at the panel.
		raw = []byte(logx.Redact(string(raw)))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=gateway.log")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	case r.URL.Path == "/logs/clear":
		s.log.Clear()
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		s.writeError(w, http.StatusNotFound, "unknown logs path: "+r.URL.Path, "invalid_request_error")
	}
}

// handleDiag serves the diagnostic probes.
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/diag/vm":
		s.writeJSON(w, http.StatusOK, vmStatusReport())
	case "/diag/queue":
		payload, ok := s.payloadObject(w, r)
		if !ok {
			return
		}
		s.writeJSON(w, http.StatusOK, s.diagQueue(payload))
	case "/diag/checkin":
		payload, ok := s.payloadObject(w, r)
		if !ok {
			return
		}
		s.writeJSON(w, http.StatusOK, s.diagCheckin(payload))
	default:
		s.writeError(w, http.StatusNotFound, "unknown diag path: "+r.URL.Path, "invalid_request_error")
	}
}

// handleUpdate reports update availability.
//
// Self-update is deliberately out of scope for the Go build: the deployment
// pipeline replaces the binary, and a process that rewrites its own executable
// is a liability. The endpoint reports state instead of pretending otherwise.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"current": s.cfg.Version, "latest": s.cfg.Version,
		"has_update": false, "can_apply": false,
		"apply_blocked_reasons": []string{
			"Go 版本不支持进程内自更新，请用部署流水线替换二进制",
		},
		"runtime": runtime.Version(), "os": runtime.GOOS,
	})
}

// handleAgents serves the client-configuration surface.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/agents":
		s.writeJSON(w, http.StatusOK, agents.Overview(s.cfg.AccountsDir))
	case "/agents/models":
		realm := firstNonEmptyStr(normalizeRealm(r.URL.Query().Get("realm")), s.currentRealm())
		s.writeJSON(w, http.StatusOK, s.agentModels(realm))
	case "/agents/apply", "/agents/restore":
		// These write files on the gateway host, so only a loopback caller is
		// allowed; anything else is a remote privilege escalation.
		if !clientIsLoopback(r) {
			s.writeError(w, http.StatusForbidden,
				"agent configuration is only available from this machine", "invalid_request_error")
			return
		}
		payload, ok := s.payloadObject(w, r)
		if !ok {
			return
		}
		if r.URL.Path == "/agents/apply" {
			s.agentApply(w, payload)
			return
		}
		s.agentRestore(w, payload)
	default:
		s.writeError(w, http.StatusNotFound, "unknown agents path: "+r.URL.Path, "invalid_request_error")
	}
}

// handleAgentsCLI serves the configurator entry point for remote machines.
func (s *Server) handleAgentsCLI(w http.ResponseWriter, r *http.Request) {
	body := agents.CLISource()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// handleAgentsTemplate renders a client configuration template.
func (s *Server) handleAgentsTemplate(w http.ResponseWriter, r *http.Request) {
	client := r.URL.Query().Get("client")
	baseURL := firstNonEmptyStr(r.URL.Query().Get("base_url"), "http://127.0.0.1:18090")
	model := firstNonEmptyStr(r.URL.Query().Get("model"), "auto")
	tmpl, err := agents.Template(client, baseURL, model)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, tmpl)
}

// handleAdmin serves the routing workbench.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet {
		switch path {
		case "/admin/overview":
			s.writeJSON(w, http.StatusOK, s.adminOverview())
			return
		case "/admin/groups":
			s.writeJSON(w, http.StatusOK, s.adminGroups())
			return
		case "/admin/routing":
			cfg, err := s.route.Load()
			if err != nil {
				s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
				return
			}
			s.writeJSON(w, http.StatusOK, cfg)
			return
		case "/admin/jobs":
			s.writeJSON(w, http.StatusOK, map[string]any{
				"scheduler": s.scheduler.Status(),
				"keepalive": s.keepalive.Status(),
			})
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown admin path: "+path, "invalid_request_error")
		return
	}
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	switch path {
	case "/admin/routing/save":
		s.adminRoutingSave(w, payload)
	case "/admin/routing/preview":
		s.adminRoutingPreview(w, payload)
	case "/admin/accounts/group":
		uid := asStringAny(payload["uid"])
		acc := s.pool.Get(uid)
		if acc == nil {
			s.writeError(w, http.StatusNotFound, "account not found", "invalid_request_error")
			return
		}
		acc.SetGroup(asStringAny(payload["group"]))
		if err := acc.Save(s.cfg.AccountsDir); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
	case "/admin/groups/action":
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		s.writeError(w, http.StatusNotFound, "unknown admin path: "+path, "invalid_request_error")
	}
}

// handleSettings serves the settings surface.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet {
		switch {
		case path == "/settings":
			s.writeJSON(w, http.StatusOK, s.exportSettingsView(false))
			return
		case path == "/settings/reveal":
			// A panel still on the default password must not be able to print a
			// plaintext key.
			if s.settings.PanelPasswordIsDefault() {
				s.writeError(w, http.StatusForbidden,
					"change the default panel password before revealing keys",
					"invalid_request_error")
				return
			}
			id := r.URL.Query().Get("id")
			for _, k := range s.settings.APIKeys() {
				if k.ID == id {
					s.writeJSON(w, http.StatusOK, map[string]any{"id": k.ID, "key": k.Key})
					return
				}
			}
			s.writeError(w, http.StatusNotFound, "key not found", "invalid_request_error")
			return
		}
		s.writeError(w, http.StatusNotFound, "unknown settings path: "+path, "invalid_request_error")
		return
	}
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	switch path {
	case "/settings/save":
		s.saveSettings(w, payload)
	case "/settings/tool-echo-mode":
		mode, err := s.settings.SetToolEchoMode(asStringAny(payload["mode"]))
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode})
	case "/settings/anthropic-thinking":
		mode, err := s.settings.SetAnthropicThinking(asStringAny(payload["mode"]))
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode})
	case "/settings/model-context":
		ctxMap, err := s.settings.SetModelContext(asStringAny(payload["model"]),
			asStringAny(payload["context"]))
		if err != nil {
			s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model_context": ctxMap})
	case "/settings/web-tools":
		on, _ := payload["enabled"].(bool)
		if err := s.settings.SetWebToolsEnabled(on); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": on})
	default:
		s.writeError(w, http.StatusNotFound, "unknown settings path: "+path, "invalid_request_error")
	}
}

// saveSettings applies a settings document from the panel.
func (s *Server) saveSettings(w http.ResponseWriter, payload map[string]any) {
	if keys, ok := payload["api_keys"].([]any); ok {
		entries := make([]*settingsKeyEntry, 0, len(keys))
		for _, raw := range keys {
			if m, ok := raw.(map[string]any); ok {
				if entry := settingsCleanKey(m); entry != nil {
					entries = append(entries, entry)
				}
			}
		}
		if _, err := s.settings.SetAPIKeys(entries); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	if v, ok := payload["banned_models"]; ok {
		if _, err := s.settings.SetBannedModels(v); err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	if limits, okLimits := payload["limits"].(map[string]any); okLimits {
		for key, raw := range limits {
			entry, okEntry := raw.(map[string]any)
			if !okEntry {
				continue
			}
			for _, scope := range []string{"global", "intl", "cn"} {
				if v, exists := entry[scope]; exists {
					if _, err := s.settings.SetLimit(key, scope, v); err != nil {
						s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
						return
					}
				}
			}
		}
	}
	if v, ok := payload["auth_disabled"].(bool); ok {
		_ = s.settings.SetAuthDisabled(v)
	}
	if v, ok := payload["realm"].(string); ok && normalizeRealm(v) != "" {
		s.setRealm(normalizeRealm(v))
	}
	s.applyPoolSettings()
	s.writeJSON(w, http.StatusOK, s.exportSettingsView(false))
}

// adminOverview summarises the fleet.
func (s *Server) adminOverview() map[string]any {
	byRealm := map[string]int{}
	enabled := 0
	for _, acc := range s.pool.Accounts() {
		byRealm[acc.Realm]++
		if acc.EnabledValue() {
			enabled++
		}
	}
	return map[string]any{
		"accounts": len(s.pool.Accounts()), "enabled": enabled,
		"ready": s.pool.CountReady("", "", nil), "by_realm": byRealm,
		"realm": s.currentRealm(), "version": s.cfg.Version,
	}
}

// adminGroups lists the account groups in use.
func (s *Server) adminGroups() map[string]any {
	groups := map[string][]string{}
	for _, acc := range s.pool.Accounts() {
		g := acc.Group
		if g == "" {
			g = "(ungrouped)"
		}
		groups[g] = append(groups[g], acc.UID)
	}
	return map[string]any{"groups": groups}
}

// adminRoutingSave persists a routing document.
func (s *Server) adminRoutingSave(w http.ResponseWriter, payload map[string]any) {
	rules, ok := payload["rules"].([]any)
	if !ok {
		s.writeError(w, http.StatusBadRequest, "rules is required", "invalid_request_error")
		return
	}
	parsed, err := routeNormalizeRules(rules)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	cfg, err := s.route.Save(parsed, nil, nil, asStringAny(payload["expected_revision"]))
	if err != nil {
		s.writeError(w, http.StatusConflict, err.Error(), "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, cfg)
}

// adminRoutingPreview renders a side-effect-free routing preview.
func (s *Server) adminRoutingPreview(w http.ResponseWriter, payload map[string]any) {
	model := firstNonEmptyStr(asStringAny(payload["model"]), "auto")
	likes := s.pool.AccountLikes()
	routeLikes := make([]routeAccountLike, 0, len(likes))
	for _, item := range likes {
		routeLikes = append(routeLikes, item)
	}
	preview, err := s.route.Preview(routeLikes, model, s.keyEntryFor(payload),
		normalizeRealm(asStringAny(payload["realm"])), s.currentRealm())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, preview)
}

// keyEntryFor returns the key entry named in a payload (id lookup).
func (s *Server) keyEntryFor(payload map[string]any) *settingsKeyEntry {
	id := asStringAny(payload["key_id"])
	if id == "" {
		return nil
	}
	for _, k := range s.settings.APIKeys() {
		if k.ID == id {
			return k
		}
	}
	return nil
}
