// Package scheduler runs the periodic check-in, claim and token-refresh cycle.
//
// Two hard-won design points:
//
//   - It does not poll "is the current minute 0". That approach misses a slot
//     whenever the container's timezone differs from the operator's, or whenever
//     one cycle runs past the top of the minute. Instead it wakes every 15 s and
//     fires any slot that has passed but not yet fired, catching up at most three
//     hours (so a restart still lands the same day).
//   - Fired slots are persisted. Without that, a restart re-runs a slot inside the
//     three-hour window, and the observed symptom was two check-ins three seconds
//     apart.
package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/tasks"
)

// StateSubdir keeps the state file out of AccountPool's top-level scan.
const StateSubdir = "scheduler"

// StateFileName is the persisted scheduler document.
const StateFileName = "state.json"

// Cycle origins.
const (
	CycleStartup = "startup"
	CycleHour    = "hour"
	CycleManual  = "manual"
)

// firedRetain bounds how far back fired slots are kept.
const firedRetain = 3 * 86400

// catchUpWindow is how late a missed slot may still run.
const catchUpWindow = 3 * 3600

// Scheduler is the periodic task runner.
type Scheduler struct {
	mu       sync.Mutex
	pool     *accounts.Pool
	stateDir string
	tasks    *tasks.Runner

	enabled         bool
	checkinTimes    []clockTime
	keepaliveTimes  []clockTime
	cron            string
	jitterSecs      int
	fired           map[int64]bool
	lastRunTime     string
	nextRunTime     string
	startupClaimDay string
	lastCycleDay    string
	logs            []string

	runMu  sync.Mutex
	stopCh chan struct{}
	// pendingFocus is the label of the slot being run.
	pendingFocus string
}

// clockTime is one HH:MM schedule entry.
type clockTime struct{ Hour, Minute int }

// Runner is the task executor (injected so the scheduler is testable).
type Runner interface {
	RunCheckin(targets []*accounts.Account) *tasks.BatchResult
	RunKeepalive(force bool) *tasks.KeepaliveResult
	RunCampaigns(targets []*accounts.Account) *tasks.BatchResult
	RefreshCredits(acc *accounts.Account)
}

// New builds a scheduler bound to a pool and accounts directory.
func New(pool *accounts.Pool, accountsDir string, runner Runner) *Scheduler {
	s := &Scheduler{
		pool:       pool,
		stateDir:   accountsDir,
		tasks:      nil,
		fired:      map[int64]bool{},
		jitterSecs: 8,
		logs:       []string{},
	}
	if runner != nil {
		if r, ok := runner.(*tasks.Runner); ok {
			s.tasks = r
		}
	}
	s.loadState()
	s.applySchedule(map[string]any{})
	return s
}

// StatePath returns accounts/scheduler/state.json.
func (s *Scheduler) StatePath() string {
	return filepath.Join(s.stateDir, StateSubdir, StateFileName)
}

// Enabled reports whether automatic runs are on.
func (s *Scheduler) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

func (s *Scheduler) loadState() {
	raw, err := os.ReadFile(s.StatePath())
	if err != nil {
		s.enabled = true
		return
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		s.enabled = true
		return
	}
	if v, ok := doc["enabled"].(bool); ok {
		s.enabled = v
	} else {
		s.enabled = true
	}
	s.applySchedule(doc)
	s.importFired(doc["fired"])
	s.lastRunTime = asString(doc["last_run_time"])
	s.startupClaimDay = asString(doc["startup_claim_date"])
	s.lastCycleDay = asString(doc["last_cycle_date"])
	if logs, ok := doc["logs"].([]any); ok {
		for _, item := range logs {
			if line, ok := item.(string); ok {
				s.logs = append(s.logs, line)
			}
		}
		if len(s.logs) > 60 {
			s.logs = s.logs[len(s.logs)-60:]
		}
	}
}

func (s *Scheduler) saveState() {
	s.mu.Lock()
	doc := map[string]any{
		"enabled":            s.enabled,
		"checkin_times":      formatTimes(s.checkinTimes),
		"keepalive_times":    formatTimes(s.keepaliveTimes),
		"cron":               s.cron,
		"jitter_secs":        s.jitterSecs,
		"last_run_time":      s.lastRunTime,
		"next_run_time":      s.nextRunTime,
		"startup_claim_date": s.startupClaimDay,
		"last_cycle_date":    s.lastCycleDay,
		"logs":               s.logs,
		"fired":              s.exportFired(),
		"updated_at":         time.Now().Format("2006-01-02 15:04:05"),
	}
	path := s.StatePath()
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func (s *Scheduler) exportFired() []int64 {
	now := float64(time.Now().Unix())
	var out []int64
	for ts := range s.fired {
		if now-float64(ts) <= firedRetain {
			out = append(out, ts)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Scheduler) importFired(raw any) {
	list, ok := raw.([]any)
	if !ok {
		return
	}
	now := float64(time.Now().Unix())
	for _, item := range list {
		ts := asFloat(item)
		delta := ts - now
		if delta <= 0 && delta >= -firedRetain {
			s.fired[int64(ts)] = true
		}
	}
}

var timeRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)

func parseTimes(value string, fallback []clockTime) []clockTime {
	var out []clockTime
	seen := map[clockTime]bool{}
	for _, chunk := range strings.Split(value, ",") {
		chunk = strings.TrimSpace(chunk)
		m := timeRe.FindStringSubmatch(chunk)
		if m == nil {
			continue
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h < 0 || h > 23 || mi < 0 || mi > 59 {
			continue
		}
		ct := clockTime{Hour: h, Minute: mi}
		if !seen[ct] {
			seen[ct] = true
			out = append(out, ct)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hour != out[j].Hour {
			return out[i].Hour < out[j].Hour
		}
		return out[i].Minute < out[j].Minute
	})
	return out
}

func formatTimes(times []clockTime) string {
	parts := make([]string, 0, len(times))
	for _, t := range times {
		parts = append(parts, fmt.Sprintf("%02d:%02d", t.Hour, t.Minute))
	}
	return strings.Join(parts, ",")
}

// applySchedule reads the schedule fields; caller holds the lock (or is the
// constructor).
func (s *Scheduler) applySchedule(doc map[string]any) {
	s.checkinTimes = parseTimes(asString(doc["checkin_times"]),
		[]clockTime{{9, 0}, {21, 0}})
	s.keepaliveTimes = parseTimes(asString(doc["keepalive_times"]),
		[]clockTime{{22, 0}})
	cron := strings.TrimSpace(asString(doc["cron"]))
	if cron != "" && !validCron(cron) {
		// An invalid expression is ignored rather than silently disabling checks.
		cron = ""
	}
	s.cron = cron
	jitter := int(asFloat(doc["jitter_secs"]))
	if jitter <= 0 && doc["jitter_secs"] == nil {
		jitter = 8
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 60 {
		jitter = 60
	}
	s.jitterSecs = jitter
	// Fired slots are deliberately NOT cleared: clearing would re-run a slot
	// inside the catch-up window (a duplicate claim).
	s.calcNextFire()
}

// SetSchedule validates and persists a new schedule from the panel.
func (s *Scheduler) SetSchedule(checkinTimes, keepaliveTimes, cron string, jitter int) map[string]any {
	s.mu.Lock()
	doc := map[string]any{
		"checkin_times":   firstNonEmpty(checkinTimes, formatTimes(s.checkinTimes)),
		"keepalive_times": firstNonEmpty(keepaliveTimes, formatTimes(s.keepaliveTimes)),
		"cron":            firstNonEmpty(cron, s.cron),
	}
	if jitter > 0 {
		doc["jitter_secs"] = jitter
	} else {
		doc["jitter_secs"] = s.jitterSecs
	}
	s.applySchedule(doc)
	s.mu.Unlock()
	s.saveState()
	return s.Status()
}

// SetEnabled turns automatic runs on or off.
func (s *Scheduler) SetEnabled(on bool) {
	s.mu.Lock()
	s.enabled = on
	s.mu.Unlock()
	s.saveState()
}

// Log appends one line to the panel log and the gateway log.
func (s *Scheduler) Log(msg string) {
	entry := "[" + time.Now().Format("2006-01-02 15:04:05") + "] " + msg
	s.mu.Lock()
	s.logs = append(s.logs, entry)
	if len(s.logs) > 60 {
		s.logs = s.logs[len(s.logs)-60:]
	}
	s.mu.Unlock()
	// Logging is state: persist so the panel toggle and last-run time survive.
	s.saveState()
}

// ---------------------------------------------------------------------------
// Schedule calculation
// ---------------------------------------------------------------------------

type occurrence struct {
	kind  string
	label string
	ts    int64
}

// occurrences lists every slot on a given day offset.
func (s *Scheduler) occurrences(dayOffset int) []occurrence {
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, dayOffset)
	var out []occurrence
	if s.cron != "" {
		for minute := 0; minute < 24*60; minute++ {
			ts := base.Add(time.Duration(minute) * time.Minute)
			if ts.Day() != base.Day() && minute > 0 {
				break
			}
			if cronMatches(s.cron, ts) {
				out = append(out, occurrence{CycleHour,
					fmt.Sprintf("cron %02d:%02d", ts.Hour(), ts.Minute()), ts.Unix()})
			}
		}
	} else {
		for _, t := range s.checkinTimes {
			ts := base.Add(time.Duration(t.Hour)*time.Hour + time.Duration(t.Minute)*time.Minute)
			out = append(out, occurrence{CycleHour,
				fmt.Sprintf("签到期 %02d:%02d", t.Hour, t.Minute), ts.Unix()})
		}
	}
	for _, t := range s.keepaliveTimes {
		ts := base.Add(time.Duration(t.Hour)*time.Hour + time.Duration(t.Minute)*time.Minute)
		out = append(out, occurrence{CycleHour,
			fmt.Sprintf("保活期 %02d:%02d", t.Hour, t.Minute), ts.Unix()})
	}
	return out
}

// nextTarget returns a slot that has passed but has not fired yet.
func (s *Scheduler) nextTarget() *occurrence {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	var cands []occurrence
	for _, off := range []int{-1, 0, 1} {
		cands = append(cands, s.occurrences(off)...)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ts < cands[j].ts })
	for i := range cands {
		if cands[i].ts > now {
			break
		}
		if s.fired[cands[i].ts] {
			continue
		}
		if now-cands[i].ts > catchUpWindow {
			// Too old to replay; mark it so it is not reconsidered.
			s.fired[cands[i].ts] = true
			continue
		}
		return &cands[i]
	}
	return nil
}

func (s *Scheduler) calcNextFire() {
	now := time.Now().Unix()
	var cands []occurrence
	for _, off := range []int{-1, 0, 1, 2} {
		cands = append(cands, s.occurrences(off)...)
	}
	best := int64(0)
	for _, c := range cands {
		if c.ts > now+1 && (best == 0 || c.ts < best) {
			best = c.ts
		}
	}
	if best > 0 {
		s.nextRunTime = time.Unix(best, 0).Format("2006-01-02 15:04:05")
	} else {
		s.nextRunTime = ""
	}
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// Start launches the background loop.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.stopCh != nil {
		s.mu.Unlock()
		return
	}
	s.stopCh = make(chan struct{})
	stop := s.stopCh
	s.mu.Unlock()

	go func() {
		// Give the HTTP server a moment to come up before the first pass.
		select {
		case <-stop:
			return
		case <-time.After(10 * time.Second):
		}
		s.ExecuteCycle("启动初次初始化巡检", CycleStartup)

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !s.Enabled() {
					continue
				}
				due := s.nextTarget()
				if due == nil {
					continue
				}
				s.mu.Lock()
				s.fired[due.ts] = true
				s.pendingFocus = due.label
				s.mu.Unlock()
				s.saveState()
				s.ExecuteCycle("整点排程命中 ("+due.label+")", due.kind)
			}
		}
	}()
}

// Stop halts the loop.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	s.stopCh = nil
}

// TriggerNow runs one cycle right away.
func (s *Scheduler) TriggerNow() map[string]any {
	if !s.runMu.TryLock() {
		return map[string]any{"ok": false, "msg": "已有巡检正在执行，请稍候再试"}
	}
	go func() {
		defer s.runMu.Unlock()
		s.runCycle("手动立即触发", CycleManual)
	}()
	return map[string]any{"ok": true, "msg": "已触发后台调度执行"}
}

// ExecuteCycle runs one cycle unless another is in flight.
func (s *Scheduler) ExecuteCycle(reason, kind string) {
	if !s.runMu.TryLock() {
		s.Log("跳过本次巡检 (" + reason + ")：上一轮仍在执行")
		return
	}
	defer s.runMu.Unlock()
	s.runCycle(reason, kind)
}

// allowComplementCheckin gates the startup pass.
//
// The startup pass sends real claim requests for accounts that still need one,
// so a restart loop would claim repeatedly. The marker is written before the
// action: a crash afterwards still prevents a replay.
func (s *Scheduler) allowComplementCheckin(kind string) bool {
	if kind != CycleStartup {
		return true
	}
	today := time.Now().In(accounts.UTC8()).Format("2006-01-02")
	s.mu.Lock()
	if s.startupClaimDay == today {
		s.mu.Unlock()
		return false
	}
	s.startupClaimDay = today
	s.mu.Unlock()
	s.saveState()
	return true
}

func (s *Scheduler) runCycle(reason, kind string) {
	s.mu.Lock()
	s.lastRunTime = time.Now().Format("2006-01-02 15:04:05")
	s.lastCycleDay = time.Now().In(accounts.UTC8()).Format("2006-01-02")
	focus := s.pendingFocus
	s.pendingFocus = ""
	keepaliveHours := map[int]bool{}
	for _, t := range s.keepaliveTimes {
		keepaliveHours[t.Hour] = true
	}
	checkinHours := map[int]bool{}
	for _, t := range s.checkinTimes {
		checkinHours[t.Hour] = true
	}
	s.mu.Unlock()

	s.Log("开始执行任务 (" + reason + ")...")
	if s.tasks == nil {
		s.Log("任务执行器未就绪，跳过本次巡检")
		return
	}
	if s.pool == nil || len(s.pool.Accounts()) == 0 {
		s.Log("暂无可用的活跃账号，跳过本次巡检")
		return
	}

	curHour := time.Now().Hour()
	var keepaliveDue, checkinDue bool
	switch {
	case strings.HasPrefix(focus, "保活期"):
		keepaliveDue, checkinDue = true, false
	case strings.HasPrefix(focus, "签到期"), strings.HasPrefix(focus, "cron"):
		keepaliveDue, checkinDue = false, true
	default:
		keepaliveDue = keepaliveHours[curHour]
		checkinDue = checkinHours[curHour]
	}
	allowComplement := s.allowComplementCheckin(kind)

	// 1. Token keepalive.
	ka := s.tasks.RunKeepalive(keepaliveDue)
	if ka != nil {
		s.Log(fmt.Sprintf("Token 保活：刷新 %d 个，失败 %d 个%s",
			ka.Refreshed, ka.Failed, map[bool]string{true: "（集中保活）", false: "（临近过期）"}[keepaliveDue]))
		for _, line := range ka.Logs {
			s.Log(line)
		}
	}

	failed := map[string]bool{}
	if ka != nil {
		for _, uid := range ka.FailedUIDs {
			failed[uid] = true
		}
	}
	stale := tasks.StaleUIDs(s.pool, failed)
	if len(stale) > 0 {
		s.Log(fmt.Sprintf("跳过 %d 个失效凭证账号（本轮不签到、不刷额度）", len(stale)))
	}

	// 2. Daily check-in.
	var targets []*accounts.Account
	for _, acc := range s.pool.Accounts() {
		if stale[acc.UID] || !acc.CheckinEligible() {
			continue
		}
		targets = append(targets, acc)
	}
	var pending []*accounts.Account
	switch {
	case checkinDue:
		pending = targets
	case allowComplement:
		for _, acc := range targets {
			if acc.CanCheckin() {
				pending = append(pending, acc)
			}
		}
	}
	checkinCount, earned := 0, 0
	if len(pending) > 0 {
		s.Log(fmt.Sprintf("检测到 %d 个账号需要签到，执行自动签到...", len(pending)))
		res := s.tasks.RunCheckin(pending)
		if res != nil {
			checkinCount = res.AccountsCount
			earned = res.CreditAdded
			for _, line := range res.Logs {
				if strings.HasPrefix(line, "✓") || strings.HasPrefix(line, "!") {
					s.Log(line)
				}
			}
		}
	} else if checkinDue {
		s.Log("所有账号今日已签到")
	}

	// 3. Refresh balance snapshots (the panel cards depend on them).
	for _, acc := range targets {
		s.tasks.RefreshCredits(acc)
		time.Sleep(500 * time.Millisecond)
	}

	s.Log(fmt.Sprintf("巡检完成：Token 保活 %d 个，签到 %d 个，本次新增积分 +%d",
		ka.Refreshed, checkinCount, earned))
	s.saveState()
}

// Status renders the panel's scheduler state.
func (s *Scheduler) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := make([]string, 0, len(s.logs))
	if len(s.logs) > 20 {
		logs = append(logs, s.logs[len(s.logs)-20:]...)
	} else {
		logs = append(logs, s.logs...)
	}
	lastRun := s.lastRunTime
	if lastRun == "" {
		lastRun = "尚未运行"
	}
	nextRun := s.nextRunTime
	if nextRun == "" {
		nextRun = "待调度"
	}
	mode := fmt.Sprintf("排程：每日签到 %s · Token 保活 %s",
		formatTimes(s.checkinTimes), formatTimes(s.keepaliveTimes))
	if s.cron != "" {
		mode += " · cron " + s.cron
	}
	return map[string]any{
		"enabled":            s.enabled,
		"checkin_times":      formatTimes(s.checkinTimes),
		"keepalive_times":    formatTimes(s.keepaliveTimes),
		"cron":               s.cron,
		"jitter_secs":        s.jitterSecs,
		"timezone":           time.Now().Format("MST -0700"),
		"mode":               mode,
		"last_run_time":      lastRun,
		"next_run_time":      nextRun,
		"startup_claim_date": s.startupClaimDay,
		"last_cycle_date":    s.lastCycleDay,
		"state_file":         s.StatePath(),
		"logs":               logs,
	}
}

// ---------------------------------------------------------------------------
// cron
// ---------------------------------------------------------------------------

type cronSpec struct {
	minute, hour, dom, month, dow map[int]bool
}

var cronCacheMu sync.Mutex
var cronCache = map[string]*cronSpec{}

func validCron(expr string) bool { _, ok := parseCron(expr); return ok }

func parseCron(expr string) (*cronSpec, bool) {
	cronCacheMu.Lock()
	if hit, ok := cronCache[expr]; ok {
		cronCacheMu.Unlock()
		return hit, hit != nil
	}
	cronCacheMu.Unlock()

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		cronCacheMu.Lock()
		cronCache[expr] = nil
		cronCacheMu.Unlock()
		return nil, false
	}
	ranges := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	sets := make([]map[int]bool, 5)
	for i, field := range fields {
		set, ok := parseCronField(field, ranges[i][0], ranges[i][1])
		if !ok {
			cronCacheMu.Lock()
			cronCache[expr] = nil
			cronCacheMu.Unlock()
			return nil, false
		}
		sets[i] = set
	}
	spec := &cronSpec{minute: sets[0], hour: sets[1], dom: sets[2],
		month: sets[3], dow: sets[4]}
	cronCacheMu.Lock()
	cronCache[expr] = spec
	cronCacheMu.Unlock()
	return spec, true
}

// parseCronField supports *, a-b, a,b, */n and a-b/n.
func parseCronField(spec string, lo, hi int) (map[int]bool, bool) {
	out := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		step := 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			stepStr := part[idx+1:]
			part = part[:idx]
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, false
			}
			step = n
		}
		var start, end int
		switch {
		case part == "*":
			start, end = lo, hi
		case strings.Contains(part, "-"):
			bits := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(bits[0])
			b, err2 := strconv.Atoi(bits[1])
			if err1 != nil || err2 != nil {
				return nil, false
			}
			start, end = a, b
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}
			start, end = n, n
		}
		if start < lo || end > hi || start > end {
			return nil, false
		}
		for v := start; v <= end; v += step {
			out[v] = true
		}
	}
	return out, len(out) > 0
}

func cronMatches(expr string, t time.Time) bool {
	spec, ok := parseCron(expr)
	if !ok {
		return false
	}
	if !spec.minute[t.Minute()] || !spec.hour[t.Hour()] ||
		!spec.month[int(t.Month())] || !spec.dom[t.Day()] {
		return false
	}
	// Go's Sunday is 0, which matches cron's convention.
	return spec.dow[int(t.Weekday())]
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		var f float64
		if _, err := fmt.Sscanf(t, "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
