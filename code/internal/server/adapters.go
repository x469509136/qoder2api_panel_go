package server

import (
	"net/http"
	"runtime"
	"strings"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/agents"
	"github.com/xzs/bridge_qoder/internal/logx"
	"github.com/xzs/bridge_qoder/internal/route"
	"github.com/xzs/bridge_qoder/internal/settings"
	"github.com/xzs/bridge_qoder/internal/tasks"
	"github.com/xzs/bridge_qoder/internal/upstream"
)

// settingsKeyEntry is the panel-facing key row.
type settingsKeyEntry = settings.KeyEntry

// routeAccountLike is the routing package's account interface.
type routeAccountLike = route.AccountLike

// settingsCleanKey normalises one key entry.
func settingsCleanKey(m map[string]any) *settings.KeyEntry { return settings.CleanKeyEntry(m) }

// routeNormalizeRules validates a rule list.
func routeNormalizeRules(items []any) ([]route.Rule, error) { return route.NormalizeRules(items) }

// logxQuery translates a request into a log query.
func logxQuery(r *http.Request) logx.Query {
	return logx.Query{
		Limit:   queryInt(r, "limit", 200),
		Level:   r.URL.Query().Get("level"),
		Tag:     r.URL.Query().Get("tag"),
		Search:  r.URL.Query().Get("search"),
		SinceID: int64(queryInt(r, "since_id", 0)),
	}
}

// tasksViewForAccount builds one account's task rows.
func (s *Server) tasksViewForAccount(acc *accounts.Account) map[string]any {
	view := tasks.FetchTaskView(acc, s.cfg.AccountsDir)
	row := map[string]any{
		"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm,
		"tasks": view.Tasks, "summary": view.Summary,
	}
	if view.Campaign != nil {
		row["campaign"] = map[string]any{
			"ok": view.Campaign.OK, "show": view.Campaign.Show,
			"claimable": view.Campaign.Claimable, "url": view.Campaign.URL,
			"count": len(view.Campaign.Campaigns),
		}
	}
	return row
}

// vmStatusReport reports local virtualisation state.
//
// Upstream risk control looks at whether the machine is a VM, so the panel shows
// the same evidence rather than a guess.
func vmStatusReport() map[string]any {
	evidence := []string{}
	isVM := false
	brand := ""
	if runtime.GOOS == "linux" {
		if raw, err := readFile("/proc/cpuinfo"); err == nil {
			text := string(raw)
			lower := strings.ToLower(text)
			if strings.Contains(lower, "hypervisor") {
				evidence = append(evidence, "CPU 标志含 hypervisor（运行在虚拟机监控程序之上）")
				isVM = true
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(strings.ToLower(line), "model name") {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						cpu := strings.TrimSpace(parts[1])
						evidence = append(evidence, "处理器型号："+cpu)
						for _, marker := range []string{"virtual", "vmware", "qemu", "kvm"} {
							if strings.Contains(strings.ToLower(cpu), marker) {
								brand = marker
								isVM = true
							}
						}
					}
					break
				}
			}
		}
	}
	summary := "未检测到虚拟化运行环境（物理机）"
	if isVM {
		summary = "本机疑似运行在虚拟机中"
		if brand != "" {
			summary += "（" + brand + "）"
		}
	}
	return map[string]any{
		"is_vm": isVM, "level": map[bool]string{true: "中", false: "无"}[isVM],
		"brand": brand, "source": "local", "evidence": evidence,
		"summary": summary,
		"note":    "Go 版本只做本机交叉校验；官方风控桥（runtime-info）需另行提供",
	}
}

// diagQueue probes whether a model is queueing right now.
func (s *Server) diagQueue(payload map[string]any) map[string]any {
	model := firstNonEmptyStr(asStringAny(payload["model"]), "auto")
	realm := firstNonEmptyStr(normalizeRealm(asStringAny(payload["realm"])), s.currentRealm())
	modelKey := s.resolveModelKey(model, realm)
	acc := s.pool.RepresentativeMatching(realm)
	if acc == nil {
		return map[string]any{"kind": "error", "advice": "区域 " + realm + " 没有可用账号"}
	}
	body, err := s.buildBody(map[string]any{
		"model": model, "messages": []any{map[string]any{"role": "user", "content": "ping"}},
		"stream": false, "max_tokens": 1,
	}, acc, modelKey, realm, "")
	if err != nil {
		return map[string]any{"kind": "error", "advice": err.Error()}
	}
	cfg := acc.RealmConfig()
	resp, postErr := s.upstream.Post(ctxBackground(), upstreamRequest(body, acc,
		s.sessions, cfg.Gateway+upstreamChatPath))
	if postErr == nil {
		resp.Close()
		return map[string]any{"kind": "ok", "advice": "该模型此刻未排队", "model": model,
			"realm": realm, "identity_source": acc.MachineIdentitySource}
	}
	code := upstreamStatus(postErr)
	detail := errText(postErr)
	out := map[string]any{
		"kind": "error", "code": code, "model": model, "realm": realm,
		"identity_source": acc.MachineIdentitySource,
	}
	if wait := upstreamQueueWaitSeconds(detail); wait > 0 {
		out["kind"] = "queued"
		out["retry_after"] = wait
		out["advice"] = "该模型正在排队：换模型/换区域，或设 QD_QUEUE_WAIT_MODE=fast 立即返回 429"
	} else if code == 403 {
		out["kind"] = "rejected"
		out["advice"] = "上游拒绝（403）：可能是账号无该模型权限或额度不足，不是排队"
	} else {
		msg, _ := friendlyUpstreamError(code, detail)
		out["advice"] = msg
	}
	return out
}

// diagCheckin reports the check-in capability of each account.
func (s *Server) diagCheckin(payload map[string]any) map[string]any {
	rows := []map[string]any{}
	for _, acc := range s.resolveAccountTargets(payload) {
		res := acc.CheckinStatusFor()
		row := map[string]any{"uid": acc.UID, "realm": acc.Realm, "ok": res.OK}
		if res.Unavailable {
			row["unavailable"] = true
			row["reason"] = res.Reason
		}
		if res.Status != nil {
			row["status"] = res.Status.Status
			row["today_checked_in"] = res.Status.TodayCheckedIn
			row["streak_days"] = res.Status.StreakDays
		}
		if res.Error != "" {
			row["error"] = res.Error
		}
		rows = append(rows, row)
	}
	return map[string]any{"accounts": rows}
}

// agentModels renders the model table the agent-configuration page prefills.
func (s *Server) agentModels(realm string) map[string]any {
	out := []map[string]any{}
	for _, raw := range s.fetchModels(realm) {
		info := catalogInfo(raw)
		row := map[string]any{
			"id":             firstNonEmptyStr(info.DisplayName, info.Key),
			"upstream_key":   info.Key,
			"is_free":        info.IsFree,
			"context_window": info.MaxInputTokens,
		}
		if def := catalogDefaultEffort(info); def != "" {
			row["default_effort"] = def
		}
		out = append(out, row)
	}
	return map[string]any{"models": out, "realm": realm}
}

// agentApply writes client configuration.
func (s *Server) agentApply(w http.ResponseWriter, payload map[string]any) {
	clientID := asStringAny(payload["client"])
	baseURL := firstNonEmptyStr(asStringAny(payload["base_url"]),
		"http://127.0.0.1:"+itoa(s.cfg.Port))
	result, err := agents.Apply(s.cfg.AccountsDir, clientID, baseURL,
		asStringAny(payload["api_key"]), asStringAny(payload["model"]), "")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// agentRestore reverts a client configuration.
func (s *Server) agentRestore(w http.ResponseWriter, payload map[string]any) {
	result, err := agents.Restore(s.cfg.AccountsDir, asStringAny(payload["client"]), "")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

// upstreamQueueWaitSeconds exposes the queue parser to the diag handler.
func upstreamQueueWaitSeconds(detail string) int { return upstream.QueueWaitSeconds(detail) }
