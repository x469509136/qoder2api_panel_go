package server

import (
	"net/http"
	"strconv"

	"github.com/xzs/bridge_qoder/internal/activity"
	"github.com/xzs/bridge_qoder/internal/pricing"
	"github.com/xzs/bridge_qoder/internal/usage"
)

// handleUsage dispatches the usage endpoints.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	window := usageWindow(r)
	switch path {
	case "/usage", "/v1/usage":
		s.writeJSON(w, http.StatusOK, s.usage.SummarySnapshot())
	case "/usage/recent":
		limit := queryInt(r, "limit", 100)
		page := queryInt(r, "page", 1)
		rows := s.usage.RecentRows(limit, page, normalizeRealm(r.URL.Query().Get("realm")))
		s.writeJSON(w, http.StatusOK, map[string]any{
			"rows": rows, "page": page, "limit": limit,
		})
	case "/usage/stats":
		s.writeJSON(w, http.StatusOK, s.usage.StatsReport(window, 365, 5))
	case "/usage/analytics":
		s.writeJSON(w, http.StatusOK, s.usage.StatsReport(window, 365, 10))
	case "/usage/perf":
		out := s.usage.PerfStats(window, 5000)
		out["inner_errors"] = upstreamInnerErrors()
		out["tool_guards"] = guardsSnapshot()
		out["image_preflight"] = protocolImageStatus()
		s.writeJSON(w, http.StatusOK, out)
	case "/usage/by-account":
		stats := s.usage.StatsReport(window, 1, 50)
		s.writeJSON(w, http.StatusOK, map[string]any{
			"rows": stats["top_accounts"], "window": stats["window"],
		})
	case "/usage/by-key":
		s.writeJSON(w, http.StatusOK, map[string]any{
			"rows": s.usageByKey(window), "window": windowView(window),
		})
	case "/usage/timeseries":
		s.writeJSON(w, http.StatusOK, s.usage.DailyReport(queryInt(r, "days", 7),
			firstNonEmptyStr(r.URL.Query().Get("group"), "model"),
			normalizeRealm(r.URL.Query().Get("realm"))))
	case "/usage/daily":
		s.writeJSON(w, http.StatusOK, s.usage.DailyReport(queryInt(r, "days", 7),
			firstNonEmptyStr(r.URL.Query().Get("group"), "model"),
			normalizeRealm(r.URL.Query().Get("realm"))))
	case "/usage/remaining":
		s.writeJSON(w, http.StatusOK, s.usageRemaining())
	default:
		s.writeError(w, http.StatusNotFound, "unknown usage path: "+path, "invalid_request_error")
	}
}

// usageWindow resolves the panel's range query.
func usageWindow(r *http.Request) usage.Window {
	return usage.RangeWindow(r.URL.Query().Get("range"),
		r.URL.Query().Get("since"), r.URL.Query().Get("until"))
}

func windowView(w usage.Window) map[string]any {
	return map[string]any{"from": w.Since, "to": w.Until}
}

// usageByKey aggregates tokens per API key.
func (s *Server) usageByKey(window usage.Window) []map[string]any {
	type row struct {
		tokens   int
		requests int
	}
	agg := map[string]*row{}
	s.usage.ScanRows(func(r usage.Row) bool {
		if r.ErrType != "" || !window.Contains(r.At) {
			return true
		}
		key := firstNonEmptyStr(r.KeyID, "(none)")
		cell, ok := agg[key]
		if !ok {
			cell = &row{}
			agg[key] = cell
		}
		cell.tokens += r.TotalTokens
		cell.requests++
		return true
	})
	out := make([]map[string]any, 0, len(agg))
	for k, v := range agg {
		out = append(out, map[string]any{
			"key_id": k, "tokens": v.tokens, "requests": v.requests,
		})
	}
	return out
}

// usageRemaining reports per-account remaining credits and a days-left estimate.
func (s *Server) usageRemaining() map[string]any {
	type spend struct {
		credits float64
		tokens  int
	}
	dayStart := float64(usage.LocalMidnightUnix(1))
	spend24 := map[string]*spend{}
	s.usage.ScanRows(func(r usage.Row) bool {
		if r.At < dayStart || r.Account == "" || r.ErrType != "" {
			return true
		}
		cell, ok := spend24[r.Account]
		if !ok {
			cell = &spend{}
			spend24[r.Account] = cell
		}
		cell.credits += r.Credit
		cell.tokens += r.TotalTokens
		return true
	})
	rows := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		row := map[string]any{
			"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm,
		}
		remain := any(nil)
		if acc.Credits != nil {
			remain = acc.Credits.RemainEffective
		}
		row["credits_remain"] = remain
		used24 := 0.0
		if cell, ok := spend24[acc.UID]; ok {
			used24 = cell.credits
		}
		row["credits_24h"] = roundTo(used24, 2)
		if remain != nil && used24 > 0 {
			if n, ok := remain.(int); ok {
				row["days_left"] = roundTo(float64(n)/used24, 1)
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"accounts": rows}
}

// ---------------------------------------------------------------------------
// Credits insight
// ---------------------------------------------------------------------------

// handleCredits dispatches /credits/*.
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/credits/summary":
		s.writeJSON(w, http.StatusOK, s.creditsSummary())
	case "/credits/probe":
		s.creditsProbe(w, r)
	default:
		s.writeError(w, http.StatusNotFound, "unknown credits path: "+r.URL.Path, "invalid_request_error")
	}
}

// creditsSummary aggregates locally recorded balances (no upstream call).
func (s *Server) creditsSummary() map[string]any {
	byRealm := map[string]int{}
	totalRemain := 0
	accounts := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		row := map[string]any{
			"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm,
			"registered_at":  acc.AddedAt,
			"credits_remain": nil, "credits_used": nil, "credits_size": nil,
		}
		if acc.Credits != nil {
			row["credits_remain"] = acc.Credits.RemainEffective
			row["credits_used"] = acc.Credits.Used
			row["credits_size"] = acc.Credits.Size
			byRealm[acc.Realm] += acc.Credits.RemainEffective
			totalRemain += acc.Credits.RemainEffective
		}
		accounts = append(accounts, row)
	}
	return map[string]any{
		"by_realm": byRealm,
		"totals":   map[string]any{"remain": totalRemain, "accounts": len(accounts)},
		"accounts": accounts,
		"note":     "本地记录的余额快照，不是实时值",
	}
}

// creditsProbe walks the endpoint candidate matrix for one account.
func (s *Server) creditsProbe(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	limit := intAny(payload["limit"])
	if limit <= 0 {
		limit = 6
	}
	rows := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		res := acc.ProbeCreditEndpoints(s.cfg.AccountsDir, limit)
		row := map[string]any{"uid": acc.UID, "realm": acc.Realm}
		if res != nil && res.OK {
			row["hit"] = true
			if res.Credits != nil {
				row["remain"] = res.Credits.Remain
				row["size"] = res.Credits.Size
			}
		} else {
			row["hit"] = false
		}
		rows = append(rows, row)
		break // one probe per request: the endpoint is shared per realm
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": rows})
}

// handleCreditsUsage reports official credit consumption (cached upstream data).
func (s *Server) handleCreditsUsage(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		if uid := r.URL.Query().Get("uid"); uid != "" && acc.UID != uid {
			continue
		}
		row := map[string]any{"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm}
		if acc.Credits != nil {
			row["remain"] = acc.Credits.RemainEffective
			row["used"] = acc.Credits.Used
			row["size"] = acc.Credits.Size
			row["updated_at"] = acc.Credits.UpdatedAt
		}
		rows = append(rows, row)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows, "note": "来自最近一次余额快照",
	})
}

// handleCreditsGrants lists the credit packs of the latest snapshot.
func (s *Server) handleCreditsGrants(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	for _, acc := range s.pool.Accounts() {
		if acc.Credits == nil {
			continue
		}
		for _, pkg := range acc.Credits.Packages {
			row := map[string]any{"uid": acc.UID, "realm": acc.Realm}
			for k, v := range pkg {
				row[k] = v
			}
			rows = append(rows, row)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"grants": rows})
}

// handleActivity returns the check-in/claim event log.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"events": activity.Recent(s.cfg.UsageDir, queryInt(r, "limit", 200), 30),
	})
}

// handlePricing returns the reference price table.
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(pricing.DefaultPrices))
	realm := firstNonEmptyStr(normalizeRealm(r.URL.Query().Get("realm")), s.currentRealm())
	for _, raw := range s.fetchModels(realm) {
		info := catalogInfo(raw)
		if info.Key != "" {
			names = append(names, info.Key)
		}
	}
	s.writeJSON(w, http.StatusOK, pricing.Table(s.cfg.UsageDir, names))
}

func queryInt(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func upstreamInnerErrors() map[string]any {
	// The inner-error counters live in the upstream package; this indirection
	// keeps the panel payload shape stable.
	return innerErrorSnapshot()
}
