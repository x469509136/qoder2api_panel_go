// Package usage records one row per request and derives every panel metric from
// those rows.
//
// The log is append-only JSONL, so all aggregation is a single forward scan with
// optional time-window filtering.
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row is one usage record.
type Row struct {
	At        float64 `json:"at"`
	ISO       string  `json:"iso"`
	Model     string  `json:"model"`
	Stream    bool    `json:"stream"`
	ElapsedMS *int    `json:"elapsed_ms,omitempty"`
	TTFTMS    *int    `json:"ttft_ms,omitempty"`
	GenMS     *int    `json:"gen_ms,omitempty"`

	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CachedReported   bool    `json:"cached_reported"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`
	OriginalCredits  float64 `json:"original_credits"`
	Billable         bool    `json:"billable"`

	Account string `json:"account,omitempty"`
	Realm   string `json:"realm"`
	KeyID   string `json:"key_id,omitempty"`
	ErrType string `json:"error,omitempty"`

	EffortRequested string `json:"reasoning_effort_requested,omitempty"`
	Effort          string `json:"reasoning_effort,omitempty"`
	RealmReason     string `json:"realm_reason,omitempty"`

	MsgsSHA string `json:"msgs_sha,omitempty"`
	NMsgs   int    `json:"n_msgs,omitempty"`
	SysSrc  string `json:"sys_src,omitempty"`
	NMsgSys int    `json:"n_msg_system,omitempty"`

	SessionAffinity   string `json:"session_affinity,omitempty"`
	UpstreamSessionID string `json:"upstream_session_id,omitempty"`
	RouteRule         string `json:"route_rule,omitempty"`

	TokensPerSec float64 `json:"tokens_per_sec,omitempty"`
	CacheHitPct  float64 `json:"cache_hit_pct,omitempty"`

	OutputContentChars int    `json:"output_content_chars,omitempty"`
	OutputReasonChars  int    `json:"output_reasoning_chars,omitempty"`
	OutputHasToolCall  bool   `json:"output_has_tool_call,omitempty"`
	OutputFinishReason string `json:"output_finish_reason,omitempty"`
	OutputCodeNoTools  bool   `json:"output_code_without_tools,omitempty"`
}

// Summary is the running aggregate persisted next to the log.
type Summary struct {
	Requests         int64                     `json:"requests"`
	Errors           int64                     `json:"errors"`
	PromptTokens     int64                     `json:"prompt_tokens"`
	CompletionTokens int64                     `json:"completion_tokens"`
	ReasoningTokens  int64                     `json:"reasoning_tokens"`
	CachedTokens     int64                     `json:"cached_tokens"`
	TotalTokens      int64                     `json:"total_tokens"`
	Credit           float64                   `json:"credit"`
	OriginalCredits  float64                   `json:"original_credits"`
	CostUSD          float64                   `json:"cost_usd"`
	Unpriced         int64                     `json:"unpriced"`
	Started          int64                     `json:"started"`
	TTFTMsSum        int64                     `json:"ttft_ms_sum"`
	TTFTSamples      int64                     `json:"ttft_samples"`
	GenMsSum         int64                     `json:"gen_ms_sum"`
	GenSamples       int64                     `json:"gen_samples"`
	WallMsSum        int64                     `json:"wall_ms_sum"`
	WallSamples      int64                     `json:"wall_samples"`
	ByModel          map[string]map[string]any `json:"by_model"`
}

// Store is the usage log handle.
type Store struct {
	mu      sync.Mutex
	dir     string
	summary Summary
	// resultCache memoizes expensive reports briefly.
	reports map[string]cachedReport
	cacheMu sync.Mutex
}

type cachedReport struct {
	at   float64
	size int64
	data any
}

// NewStore binds a usage store to a directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:     dir,
		reports: map[string]cachedReport{},
		summary: Summary{Started: time.Now().Unix(), ByModel: map[string]map[string]any{}},
	}
	s.loadSummary()
	return s, nil
}

// Dir returns the usage directory.
func (s *Store) Dir() string { return s.dir }

// LogPath is usage.jsonl.
func (s *Store) LogPath() string { return filepath.Join(s.dir, "usage.jsonl") }

// SummaryPath is usage-summary.json.
func (s *Store) SummaryPath() string { return filepath.Join(s.dir, "usage-summary.json") }

func (s *Store) loadSummary() {
	raw, err := os.ReadFile(s.SummaryPath())
	if err != nil {
		return
	}
	var out Summary
	if json.Unmarshal(raw, &out) == nil {
		if out.ByModel == nil {
			out.ByModel = map[string]map[string]any{}
		}
		s.summary = out
	}
}

// Record appends one row and rewrites the summary atomically.
func (s *Store) Record(row Row) error {
	if row.At == 0 {
		row.At = float64(time.Now().Unix())
	}
	if row.ISO == "" {
		row.ISO = time.Now().Format("2006-01-02T15:04:05")
	}
	if row.GenMS != nil && *row.GenMS > 0 {
		row.TokensPerSec = roundTo(float64(row.CompletionTokens)/float64(*row.GenMS)*1000, 2)
	}
	if row.PromptTokens > 0 {
		row.CacheHitPct = roundTo(float64(row.CachedTokens)*100/float64(row.PromptTokens), 1)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	blob, err := json.Marshal(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(blob, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()

	s.foldIntoSummary(&row)
	out, err := json.MarshalIndent(s.summary, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", s.SummaryPath(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.SummaryPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.invalidateReports()
	return nil
}

func (s *Store) foldIntoSummary(row *Row) {
	s.summary.Requests++
	if row.ErrType != "" {
		s.summary.Errors++
	}
	s.summary.PromptTokens += int64(row.PromptTokens)
	s.summary.CompletionTokens += int64(row.CompletionTokens)
	s.summary.ReasoningTokens += int64(row.ReasoningTokens)
	s.summary.CachedTokens += int64(row.CachedTokens)
	s.summary.TotalTokens += int64(row.TotalTokens)
	s.summary.Credit += row.Credit
	s.summary.OriginalCredits += row.OriginalCredits
	if row.TTFTMS != nil {
		s.summary.TTFTMsSum += int64(*row.TTFTMS)
		s.summary.TTFTSamples++
	}
	if row.GenMS != nil {
		s.summary.GenMsSum += int64(*row.GenMS)
		s.summary.GenSamples++
	}
	if row.ElapsedMS != nil {
		s.summary.WallMsSum += int64(*row.ElapsedMS)
		s.summary.WallSamples++
	}
	per, ok := s.summary.ByModel[row.Model]
	if !ok {
		per = map[string]any{"requests": 0, "prompt_tokens": 0,
			"completion_tokens": 0, "total_tokens": 0, "cached_tokens": 0}
		s.summary.ByModel[row.Model] = per
	}
	per["requests"] = asInt64(per["requests"]) + 1
	per["prompt_tokens"] = asInt64(per["prompt_tokens"]) + int64(row.PromptTokens)
	per["completion_tokens"] = asInt64(per["completion_tokens"]) + int64(row.CompletionTokens)
	per["total_tokens"] = asInt64(per["total_tokens"]) + int64(row.TotalTokens)
	per["cached_tokens"] = asInt64(per["cached_tokens"]) + int64(row.CachedTokens)
}

// SummarySnapshot returns a deep copy of the running aggregate.
func (s *Store) SummarySnapshot() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, _ := json.Marshal(s.summary)
	var out Summary
	_ = json.Unmarshal(blob, &out)
	return out
}

func (s *Store) invalidateReports() {
	s.cacheMu.Lock()
	s.reports = map[string]cachedReport{}
	s.cacheMu.Unlock()
}

// logSize returns the current log size (used as a cache fingerprint).
func (s *Store) logSize() int64 {
	fi, err := os.Stat(s.LogPath())
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) cached(key string, ttl float64, build func() any) any {
	size := s.logSize()
	now := float64(time.Now().Unix())
	s.cacheMu.Lock()
	if hit, ok := s.reports[key]; ok && hit.size == size && now-hit.at < ttl {
		s.cacheMu.Unlock()
		return hit.data
	}
	s.cacheMu.Unlock()
	data := build()
	s.cacheMu.Lock()
	s.reports[key] = cachedReport{at: now, size: size, data: data}
	s.cacheMu.Unlock()
	return data
}

// ScanRows walks the log in order, calling fn for each parseable row.
func (s *Store) ScanRows(fn func(Row) bool) {
	f, err := os.Open(s.LogPath())
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row Row
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if !fn(row) {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Time windows
// ---------------------------------------------------------------------------

// Window is a [Since, Until) epoch range; zero values mean unbounded.
type Window struct {
	Since float64
	Until float64
}

// RangeWindow resolves the panel's range parameter.
func RangeWindow(rangeName string, since, until string) Window {
	now := float64(time.Now().Unix())
	switch strings.ToLower(strings.TrimSpace(rangeName)) {
	case "today", "day":
		start := localMidnight(0)
		return Window{Since: float64(start), Until: now}
	case "week":
		return Window{Since: float64(localMidnight(6)), Until: now}
	case "month":
		return Window{Since: float64(localMidnight(29)), Until: now}
	case "custom":
		var w Window
		if v, err := strconv.ParseFloat(strings.TrimSpace(since), 64); err == nil {
			w.Since = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(until), 64); err == nil {
			w.Until = v
		}
		return w
	}
	return Window{}
}

// Contains reports whether ts falls inside the window.
func (w Window) Contains(ts float64) bool {
	if w.Since > 0 && ts < w.Since {
		return false
	}
	if w.Until > 0 && ts > w.Until {
		return false
	}
	return true
}

func localMidnight(daysBack int) int64 {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return midnight.AddDate(0, 0, -daysBack).Unix()
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

// CreditsCoefficients returns the three per-1k-token credit factors.
//
// Defaults come from a fit over 244 real DeepSeek-Flash charges plus 110
// nominal Qwen3.8-Flash ones (residual under 1%). QD_CREDIT_COEF overrides them.
func CreditsCoefficients() [3]float64 {
	raw := strings.TrimSpace(os.Getenv("QD_CREDIT_COEF"))
	if raw != "" {
		parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
		if len(parts) == 3 {
			var out [3]float64
			ok := true
			for i, p := range parts {
				v, err := strconv.ParseFloat(p, 64)
				if err != nil || v < 0 {
					ok = false
					break
				}
				out[i] = v
			}
			if ok {
				return out
			}
		}
	}
	return [3]float64{1.069, 0.086, 4.277}
}

// SplitCredit converts tokens into the three credit components.
func SplitCredit(prompt, cached, completion int) map[string]float64 {
	coef := CreditsCoefficients()
	if cached > prompt {
		cached = prompt
	}
	fresh := prompt - cached
	return map[string]float64{
		"input":  roundTo(float64(fresh)*coef[0]/1000, 4),
		"cached": roundTo(float64(cached)*coef[1]/1000, 4),
		"output": roundTo(float64(completion)*coef[2]/1000, 4),
	}
}

// FullPriceThreshold is the prompt size above which a zero-cache request counts
// as a "full price" request.
func FullPriceThreshold() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("QD_FULL_PRICE_PROMPT_MIN"))); err == nil && v > 0 {
		return v
	}
	return 100000
}

// ProviderOf maps a model name onto an upstream vendor label.
func ProviderOf(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "qwen"), strings.Contains(m, "qmodel"),
		strings.Contains(m, "qfmodel"), strings.Contains(m, "q37fmodel"):
		return "Qwen"
	case strings.Contains(m, "glm"), strings.Contains(m, "gmodel"):
		return "智谱 GLM"
	case strings.Contains(m, "deepseek"), strings.Contains(m, "dmodel"),
		strings.Contains(m, "dfmodel"):
		return "DeepSeek"
	case strings.Contains(m, "kimi"), strings.Contains(m, "kmodel"):
		return "Kimi"
	case strings.Contains(m, "minimax"), strings.Contains(m, "mmodel"):
		return "MiniMax"
	case strings.Contains(m, "cantus"), strings.Contains(m, "cmodel"),
		strings.Contains(m, "sonus"), strings.Contains(m, "smodel"):
		return "Cantus"
	}
	return "其他"
}

// PerfStats computes latency and throughput percentiles.
func (s *Store) PerfStats(window Window, sampleLimit int) map[string]any {
	if sampleLimit <= 0 {
		sampleLimit = 5000
	}
	key := fmt.Sprintf("perf|%.0f|%.0f|%d", window.Since, window.Until, sampleLimit)
	return s.cached(key, 10, func() any {
		var ttfts, gens, walls, tps []float64
		var byModel = map[string][]float64{}
		var requestCount, errorCount int
		s.ScanRows(func(row Row) bool {
			if !window.Contains(row.At) {
				return true
			}
			if row.ErrType != "" {
				errorCount++
				return true
			}
			requestCount++
			if row.TTFTMS != nil {
				ttfts = append(ttfts, float64(*row.TTFTMS))
			}
			if row.GenMS != nil && *row.GenMS > 0 {
				gens = append(gens, float64(*row.GenMS))
			}
			if row.ElapsedMS != nil {
				walls = append(walls, float64(*row.ElapsedMS))
			}
			if row.TokensPerSec > 0 {
				tps = append(tps, row.TokensPerSec)
				byModel[row.Model] = append(byModel[row.Model], row.TokensPerSec)
			}
			return true
		})
		trim := func(v []float64) []float64 {
			if len(v) > sampleLimit {
				return v[len(v)-sampleLimit:]
			}
			return v
		}
		models := map[string]any{}
		for name, vals := range byModel {
			models[name] = percentileBlock(vals)
		}
		return map[string]any{
			"requests":       requestCount,
			"errors":         errorCount,
			"ttft":           percentileBlock(trim(ttfts)),
			"gen":            percentileBlock(trim(gens)),
			"wall":           percentileBlock(trim(walls)),
			"tokens_per_sec": percentileBlock(trim(tps)),
			"by_model":       models,
		}
	}).(map[string]any)
}

func percentileBlock(values []float64) map[string]any {
	if len(values) == 0 {
		return map[string]any{"n": 0, "p50": nil, "p90": nil, "p99": nil, "avg": nil}
	}
	cp := make([]float64, len(values))
	copy(cp, values)
	sort.Float64s(cp)
	sum := 0.0
	for _, v := range cp {
		sum += v
	}
	return map[string]any{
		"n": len(cp), "p50": percentile(cp, 50), "p90": percentile(cp, 90),
		"p99": percentile(cp, 99), "avg": roundTo(sum/float64(len(cp)), 2),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return roundTo(sorted[idx], 2)
}

// StatsReport is the overview page payload.
func (s *Store) StatsReport(window Window, heatmapDays, topN int) map[string]any {
	if heatmapDays <= 0 {
		heatmapDays = 365
	}
	if topN <= 0 {
		topN = 5
	}
	now := float64(time.Now().Unix())
	heatLo := float64(localMidnight(heatmapDays - 1))
	threshold := FullPriceThreshold()

	tot := struct {
		requests, errors, tokens, prompt, completion, cached int
	}{}
	fullPrice := struct {
		requests, prompt int
		credit           float64
	}{}
	totalCredit := 0.0
	models := map[string]*counter{}
	accounts := map[string]*counter{}
	providers := map[string]*counter{}
	heat := map[string]int{}
	allDays := map[string]bool{}
	activeDays := map[string]bool{}
	cacheWin := map[string][2]int{"10m": {}, "1h": {}, "24h": {}, "7d": {}}
	hourCached := [24]int{}
	hourPrompt := [24]int{}
	hourTokens := [24]int{}

	s.ScanRows(func(row Row) bool {
		isErr := row.ErrType != ""
		day := dayKey(row.At)
		if row.At >= heatLo && day != "" {
			if !isErr && row.TotalTokens > 0 {
				heat[day] += row.TotalTokens
			}
			allDays[day] = true
		}
		if !isErr && row.PromptTokens > 0 {
			for key, secs := range map[string]float64{"10m": 600, "1h": 3600, "24h": 86400, "7d": 604800} {
				if row.At >= now-secs {
					cur := cacheWin[key]
					cacheWin[key] = [2]int{cur[0] + row.CachedTokens, cur[1] + row.PromptTokens}
				}
			}
		}
		if row.At >= now-86400 && !isErr {
			h := time.Unix(int64(row.At), 0).Local().Hour()
			hourCached[h] += row.CachedTokens
			hourPrompt[h] += row.PromptTokens
			hourTokens[h] += row.TotalTokens
		}
		if !window.Contains(row.At) {
			return true
		}
		if day != "" {
			activeDays[day] = true
		}
		if isErr {
			tot.errors++
			return true
		}
		tot.requests++
		tot.tokens += row.TotalTokens
		tot.prompt += row.PromptTokens
		tot.completion += row.CompletionTokens
		tot.cached += row.CachedTokens
		totalCredit += row.Credit
		if row.PromptTokens >= threshold && row.CachedTokens <= 0 && row.CachedReported {
			fullPrice.requests++
			fullPrice.prompt += row.PromptTokens
			fullPrice.credit += row.Credit
		}
		bump(models, row.Model, row.TotalTokens)
		bump(accounts, firstNonEmpty(row.Account, "(unattributed)"), row.TotalTokens)
		bump(providers, ProviderOf(row.Model), row.TotalTokens)
		return true
	})

	topModels := rank(models, 0)
	hours := make([]map[string]any, 0, 24)
	for i := 23; i >= 0; i-- {
		ts := now - float64(i*3600)
		h := time.Unix(int64(ts), 0).Local().Hour()
		var pct any
		if hourPrompt[h] > 0 {
			pct = roundTo(float64(hourCached[h])*100/float64(hourPrompt[h]), 2)
		}
		hours = append(hours, map[string]any{
			"ts": int64(ts), "label": time.Unix(int64(ts), 0).Format("15:00"),
			"pct": pct, "tokens": hourTokens[h],
		})
	}

	cacheRows := []map[string]any{}
	for _, spec := range []struct{ key, label string }{
		{"10m", "近 10 分钟"}, {"1h", "近 1 小时"},
		{"24h", "近 24 小时"}, {"7d", "近 7 天"},
	} {
		v := cacheWin[spec.key]
		var pct any
		if v[1] > 0 {
			pct = roundTo(float64(v[0])*100/float64(v[1]), 1)
		}
		cacheRows = append(cacheRows, map[string]any{
			"key": spec.key, "label": spec.label,
			"hit": v[0], "input": v[1], "pct": pct,
		})
	}

	heatDays := []map[string]any{}
	cursor := heatLo
	for cursor <= now {
		d := dayKey(cursor)
		heatDays = append(heatDays, map[string]any{"date": d, "tokens": heat[d]})
		cursor += 86400
	}
	maxHeat := 0
	for _, d := range heatDays {
		if t, ok := d["tokens"].(int); ok && t > maxHeat {
			maxHeat = t
		}
	}

	var topModel any
	if len(topModels) > 0 {
		topModel = topModels[0]
	}
	var successPct, creditShare any
	if tot.requests+tot.errors > 0 {
		successPct = roundTo(float64(tot.requests)*100/float64(tot.requests+tot.errors), 2)
	}
	if totalCredit > 0 {
		creditShare = roundTo(fullPrice.credit*100/totalCredit, 1)
	}
	return map[string]any{
		"window": map[string]any{
			"from": window.Since, "to": window.Until,
		},
		"kpi": map[string]any{
			"requests": tot.requests, "success": tot.requests, "errors": tot.errors,
			"success_pct": successPct, "tokens": tot.tokens,
			"prompt": tot.prompt, "completion": tot.completion, "cached": tot.cached,
			"active_days": len(activeDays), "streak_days": streak(allDays, now),
			"top_model": topModel,
		},
		"top_accounts":  rank(accounts, topN),
		"top_providers": rank(providers, topN),
		"models":        rank(models, 10),
		"providers":     rank(providers, 10),
		"cache":         map[string]any{"windows": cacheRows},
		"full_price": map[string]any{
			"requests": fullPrice.requests, "prompt": fullPrice.prompt,
			"credit":       roundTo(fullPrice.credit, 2),
			"credit_share": creditShare, "threshold": threshold,
		},
		"hourly":  map[string]any{"points": hours},
		"heatmap": map[string]any{"days": heatDays, "max": maxHeat},
	}
}

// RecentRows returns the most recent rows, newest first.
func (s *Store) RecentRows(limit, page int, realm string) []Row {
	if limit <= 0 {
		limit = 100
	}
	if page <= 0 {
		page = 1
	}
	all := []Row{}
	s.ScanRows(func(row Row) bool {
		if realm != "" && realm != "all" && row.Realm != "" && row.Realm != realm {
			return true
		}
		all = append(all, row)
		return true
	})
	start := (page - 1) * limit
	if start >= len(all) {
		return nil
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	out := make([]Row, 0, end-start)
	for i := len(all) - 1 - start; i >= len(all)-end; i-- {
		if i < 0 {
			break
		}
		out = append(out, all[i])
	}
	return out
}

// DailyReport groups tokens by day and a chosen dimension.
func (s *Store) DailyReport(days int, group, realm string) map[string]any {
	if days <= 0 {
		days = 7
	}
	since := float64(localMidnight(days - 1))
	series := map[string]map[string]int{}
	totals := map[string]int{}
	s.ScanRows(func(row Row) bool {
		if row.ErrType != "" || row.At < since {
			return true
		}
		if realm != "" && realm != "all" && row.Realm != "" && row.Realm != realm {
			return true
		}
		day := dayKey(row.At)
		key := ""
		switch group {
		case "realm":
			key = firstNonEmpty(row.Realm, "?")
		case "key":
			key = firstNonEmpty(row.KeyID, "(none)")
		case "account":
			key = firstNonEmpty(row.Account, "(none)")
		default:
			key = firstNonEmpty(row.Model, "(unknown)")
		}
		if series[day] == nil {
			series[day] = map[string]int{}
		}
		series[day][key] += row.TotalTokens
		totals[key] += row.TotalTokens
		return true
	})
	names := make([]string, 0, len(totals))
	for k := range totals {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return totals[names[i]] > totals[names[j]] })
	if len(names) > 12 {
		names = names[:12]
	}
	points := []map[string]any{}
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		key := d.Format("2006-01-02")
		row := map[string]any{"date": key}
		for _, n := range names {
			row[n] = series[key][n]
		}
		points = append(points, row)
	}
	return map[string]any{"days": days, "group": group, "series": names, "points": points}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type counter struct {
	tokens   int
	requests int
}

func bump(m map[string]*counter, key string, tokens int) {
	c, ok := m[key]
	if !ok {
		c = &counter{}
		m[key] = c
	}
	c.tokens += tokens
	c.requests++
}

func rank(m map[string]*counter, limit int) []map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].tokens > m[keys[j]].tokens })
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	total := 0
	for _, k := range keys {
		total += m[k].tokens
	}
	if total == 0 {
		total = 1
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		c := m[k]
		out = append(out, map[string]any{
			"name": k, "key": k, "tokens": c.tokens, "requests": c.requests,
			"share": roundTo(float64(c.tokens)*100/float64(total), 2),
		})
	}
	return out
}

func streak(allDays map[string]bool, now float64) int {
	if len(allDays) == 0 {
		return 0
	}
	probe := time.Unix(int64(now), 0)
	// Today may still be in progress; start from yesterday in that case.
	if !allDays[probe.Format("2006-01-02")] {
		probe = probe.AddDate(0, 0, -1)
	}
	n := 0
	for {
		if !allDays[probe.Format("2006-01-02")] {
			return n
		}
		n++
		probe = probe.AddDate(0, 0, -1)
	}
}

func dayKey(ts float64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).Format("2006-01-02")
}

func roundTo(v float64, digits int) float64 {
	pow := 1.0
	for i := 0; i < digits; i++ {
		pow *= 10
	}
	return float64(int64(v*pow+0.5)) / pow
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	}
	return 0
}

// LocalMidnightUnix returns local midnight for a day offset (exported for
// callers that need the same boundary as the reports).
func LocalMidnightUnix(daysBack int) int64 { return localMidnight(daysBack) }
