// Package tasks orchestrates check-in, campaign claims, Pro benefit packs and
// token keepalive across the account pool.
//
// Every upstream write is separated by at least one second: the official service
// treats bursts as abuse.
package tasks

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/activity"
)

// ClaimGapDefault is the minimum gap between claim requests.
const ClaimGapDefault = 1.0

// PanelCacheTTL bounds the panel read cache.
//
// Each upstream query costs 1.2-1.5 s (TLS/DNS); these fields change at most
// daily, so a 20 s cache removes the lag without showing stale writes — the
// write paths invalidate explicitly.
const PanelCacheTTL = 20

// Logger routes task diagnostics to the gateway log.
type Logger func(level, msg string)

var (
	panelMu    sync.Mutex
	panelCache = map[string]panelEntry{}
)

type panelEntry struct {
	at   float64
	data any
}

// InvalidatePanelCache clears cached panel data; an empty uid clears everything.
func InvalidatePanelCache(uid string) {
	panelMu.Lock()
	defer panelMu.Unlock()
	if uid == "" {
		panelCache = map[string]panelEntry{}
		return
	}
	for k := range panelCache {
		if strings.HasPrefix(k, uid+"|") {
			delete(panelCache, k)
		}
	}
}

func panelGet(key string) (any, bool) {
	panelMu.Lock()
	defer panelMu.Unlock()
	hit, ok := panelCache[key]
	if !ok {
		return nil, false
	}
	if time.Since(time.Unix(int64(hit.at), 0)) > PanelCacheTTL*time.Second {
		delete(panelCache, key)
		return nil, false
	}
	return hit.data, true
}

func panelPut(key string, data any) {
	panelMu.Lock()
	defer panelMu.Unlock()
	panelCache[key] = panelEntry{at: float64(time.Now().Unix()), data: data}
}

// ViewResult is one account's task view.
type ViewResult struct {
	Account  *accounts.Account      `json:"-"`
	Tasks    []map[string]any       `json:"tasks"`
	Summary  map[string]any         `json:"summary"`
	Campaign *accounts.CampaignView `json:"campaign,omitempty"`
}

// FetchTaskView builds the panel's task rows for one account.
//
// The upstream calls are independent, so they run in parallel: serially they take
// 3-9 s and are the main cause of a laggy account switcher.
func FetchTaskView(acc *accounts.Account, accountsDir string) *ViewResult {
	cacheKey := acc.UID + "|view"
	if hit, ok := panelGet(cacheKey); ok {
		return hit.(*ViewResult)
	}
	var (
		wg      sync.WaitGroup
		camp    *accounts.CampaignView
		status  *accounts.CheckinStatusResult
		plan    string
		credits *accounts.QuotaFetchResult
	)
	wg.Add(4)
	go func() { defer wg.Done(); camp = acc.FetchCampaigns() }()
	go func() { defer wg.Done(); status = acc.CheckinStatusFor() }()
	go func() { defer wg.Done(); plan = acc.FetchPlan() }()
	go func() { defer wg.Done(); credits = acc.FetchCredits(accountsDir) }()
	wg.Wait()

	res := &ViewResult{Account: acc, Campaign: camp}
	res.Summary = map[string]any{
		"streak_days": 0, "energy": 0,
		"travel": map[string]any{"state": "unknown"},
		"plan":   firstNonEmpty(plan, acc.Plan, "-"),
		"realm":  acc.Realm,
		"campaigns": map[string]any{
			"show": camp != nil && camp.Show, "claimable": camp != nil && camp.Claimable,
			"url": campaignURL(camp), "items": campaignItems(camp),
		},
	}
	if status != nil && status.OK && status.Status != nil {
		res.Summary["streak_days"] = status.Status.StreakDays
	}
	if acc.Credits != nil {
		res.Summary["credits"] = acc.Credits
	}
	res.Tasks = buildTaskRows(acc, camp, status, credits)
	panelPut(cacheKey, res)
	return res
}

func campaignURL(camp *accounts.CampaignView) string {
	if camp == nil {
		return ""
	}
	return camp.URL
}

func campaignItems(camp *accounts.CampaignView) []map[string]any {
	if camp == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(camp.Campaigns))
	for _, c := range camp.Campaigns {
		out = append(out, map[string]any{
			"key": c.Key, "title": firstNonEmpty(c.TitleZH, c.Key),
			"description": c.Description, "claim_status": c.ClaimStatus,
			"benefit_kind": c.Benefit.Kind, "benefit_amount": c.Benefit.Amount,
		})
	}
	return out
}

func buildTaskRows(acc *accounts.Account, camp *accounts.CampaignView,
	status *accounts.CheckinStatusResult, credits *accounts.QuotaFetchResult) []map[string]any {
	rows := []map[string]any{}

	// Daily claim: the campaign platform is authoritative; the legacy sash
	// endpoint (when still open) adds a row with the streak.
	if camp != nil && camp.Show {
		st := "ready"
		if !camp.Claimable {
			st = "done"
		}
		row := map[string]any{
			"task_code": "daily_claim", "name": "每日领取",
			"description": "官方活动平台每日奖励", "status": st,
		}
		if len(camp.Campaigns) > 0 {
			row["name"] = firstNonEmpty(camp.Campaigns[0].TitleZH, "每日领取")
			row["reward_credit"] = camp.Campaigns[0].Benefit.Amount
		}
		rows = append(rows, row)
	}
	if status != nil {
		switch {
		case !status.OK && status.Unavailable:
			rows = append(rows, map[string]any{
				"task_code": "daily_checkin", "name": "每日签到",
				"description": firstNonEmpty(status.Reason, "本区域未开放该接口"),
				"status":      "unavailable",
			})
		case status.OK && status.Status != nil:
			st := status.Status
			state := "ready"
			if st.TodayCheckedIn {
				state = "claimed"
			} else if !st.Active {
				state = "disabled"
			}
			rows = append(rows, map[string]any{
				"task_code": "daily_checkin", "name": "每日签到",
				"description": fmt.Sprintf("连续 %d 天，累计 %d 天", st.StreakDays, st.TotalClaimDays),
				"status":      state, "reward_credit": st.RewardCredits,
				"current": st.StreakDays,
			})
		}
	}
	if credits != nil && credits.OK && credits.Credits != nil {
		rows = append(rows, map[string]any{
			"task_code": "balance", "name": "余额快照",
			"description": fmt.Sprintf("剩余 %d / 共 %d", credits.Credits.Remain, credits.Credits.Size),
			"status":      "info", "current": credits.Credits.Remain,
		})
	}
	return rows
}

// BatchResult summarises a batch run.
type BatchResult struct {
	AccountsCount int      `json:"accounts_count"`
	CreditAdded   int      `json:"credit_added"`
	Failed        int      `json:"failed"`
	Logs          []string `json:"logs"`
}

// RunBatchCheckin checks in for each target, separated by gap seconds.
func RunBatchCheckin(targets []*accounts.Account, gap, interGap float64, accountsDir, usageDir string, log Logger) *BatchResult {
	if gap <= 0 {
		gap = ClaimGapDefault
	}
	if interGap <= 0 {
		interGap = 1.5
	}
	res := &BatchResult{}
	for i, acc := range targets {
		if i > 0 {
			time.Sleep(time.Duration(interGap * float64(time.Second)))
		}
		result := acc.Checkin(accountsDir)
		res.AccountsCount++
		name := displayName(acc)
		switch {
		case result.OK && result.Already:
			res.Logs = append(res.Logs, "✓ "+name+" 今日已签到")
			activity.Record(usageDir, "checkin", true, acc.UID, acc.Nickname, acc.Realm, "already", 0)
		case result.OK && result.Unavailable:
			res.Logs = append(res.Logs, "! "+name+" "+result.Msg)
			activity.Record(usageDir, "checkin", true, acc.UID, acc.Nickname, acc.Realm, "unavailable", 0)
		case result.OK && result.Disabled:
			res.Logs = append(res.Logs, "! "+name+" "+result.Msg)
		case result.OK:
			res.CreditAdded += result.RewardCredits
			res.Logs = append(res.Logs, "✓ "+name+" "+result.Msg)
			activity.Record(usageDir, "checkin", true, acc.UID, acc.Nickname, acc.Realm, result.Msg, result.RewardCredits)
		default:
			res.Failed++
			res.Logs = append(res.Logs, "! "+name+" 失败: "+firstNonEmpty(result.Error, "unknown"))
			activity.Record(usageDir, "checkin", false, acc.UID, acc.Nickname, acc.Realm, result.Error, 0)
		}
		if log != nil {
			log("", res.Logs[len(res.Logs)-1])
		}
		time.Sleep(time.Duration(gap * float64(time.Second)))
	}
	return res
}

// RunBatchCampaigns claims every claimable campaign for each target.
func RunBatchCampaigns(targets []*accounts.Account, gap, interGap float64, accountsDir, usageDir string, log Logger) *BatchResult {
	if gap <= 0 {
		gap = ClaimGapDefault
	}
	if interGap <= 0 {
		interGap = 1.5
	}
	res := &BatchResult{}
	now := float64(time.Now().Unix())
	for i, acc := range targets {
		if i > 0 {
			time.Sleep(time.Duration(interGap * float64(time.Second)))
		}
		name := displayName(acc)
		camp := acc.FetchCampaigns()
		if camp == nil || !camp.OK {
			res.Failed++
			res.Logs = append(res.Logs, "! "+name+" 活动列表取不到")
			continue
		}
		for _, c := range camp.Campaigns {
			if c.ClaimStatus != "CLAIMABLE" {
				continue
			}
			if acc.IsCampaignBlocked(c.ID, now) {
				res.Logs = append(res.Logs, "· "+name+" "+c.TitleZH+" 冷却中，跳过")
				continue
			}
			time.Sleep(time.Duration(gap * float64(time.Second)))
			claim := acc.ClaimCampaign(c.ID, accountsDir)
			res.AccountsCount++
			switch {
			case claim.OK && claim.Already:
				res.Logs = append(res.Logs, "✓ "+name+" "+c.TitleZH+" 已领取")
			case claim.OK:
				res.CreditAdded += claim.Credits
				res.Logs = append(res.Logs, "✓ "+name+" "+c.TitleZH+" 领取成功")
				activity.Record(usageDir, "campaign", true, acc.UID, acc.Nickname, acc.Realm, c.TitleZH, claim.Credits)
			default:
				res.Failed++
				res.Logs = append(res.Logs, "! "+name+" "+c.TitleZH+" 失败: "+firstNonEmpty(claim.Error, "unknown"))
				activity.Record(usageDir, "campaign", false, acc.UID, acc.Nickname, acc.Realm, claim.Error, 0)
			}
			InvalidatePanelCache(acc.UID)
		}
	}
	return res
}

// KeepaliveResult summarises a token refresh pass.
type KeepaliveResult struct {
	Refreshed  int      `json:"refreshed"`
	Failed     int      `json:"failed"`
	FailedUIDs []string `json:"failed_uids"`
	Logs       []string `json:"logs"`
}

// RunKeepalive refreshes tokens; force refreshes every account, otherwise only
// those within four hours of expiry.
func RunKeepalive(pool *accounts.Pool, force bool, threshold time.Duration, log Logger) *KeepaliveResult {
	if threshold <= 0 {
		threshold = 4 * time.Hour
	}
	res := &KeepaliveResult{}
	if pool == nil {
		return res
	}
	now := time.Now().Unix()
	for _, acc := range pool.Accounts() {
		if !acc.InferenceEligible() && !acc.CheckinEligible() {
			continue
		}
		exp := acc.ExpiryUnix()
		due := force
		if !due && exp > 0 && time.Duration(exp-now)*time.Second < threshold {
			due = true
		}
		if !due {
			continue
		}
		if err := acc.Refresh(); err != nil {
			res.Failed++
			res.FailedUIDs = append(res.FailedUIDs, acc.UID)
			res.Logs = append(res.Logs, "! "+displayName(acc)+" 刷新失败: "+err.Error())
			continue
		}
		res.Refreshed++
	}
	return res
}

// StaleUIDs returns accounts whose credential is known-expired this cycle.
//
// They are excluded from check-in and balance snapshots as a whole: skipping only
// the upstream call is not enough, because a degraded quota response would still
// overwrite a healthy balance with zeros.
func StaleUIDs(pool *accounts.Pool, failed map[string]bool) map[string]bool {
	out := map[string]bool{}
	for uid := range failed {
		out[uid] = true
	}
	if pool == nil {
		return out
	}
	now := time.Now().Unix()
	for _, acc := range pool.Accounts() {
		exp := acc.ExpiryUnix()
		if acc.AccessTokenValue() != "" && exp > 0 && exp <= now {
			out[acc.UID] = true
		}
	}
	return out
}

// SortedUIDs returns a sorted slice for deterministic logs.
func SortedUIDs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for uid := range set {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

func displayName(acc *accounts.Account) string {
	if acc.Nickname != "" {
		return acc.Nickname
	}
	if len(acc.UID) >= 8 {
		return acc.UID[:8]
	}
	return acc.UID
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
