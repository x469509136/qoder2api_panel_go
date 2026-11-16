package accounts

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// CheckinReasonNotFound marks an unavailable check-in endpoint.
const CheckinReasonNotFound = "checkin_endpoint_not_found"

// CheckinStatus is the normalized daily check-in state.
type CheckinStatus struct {
	Status            string `json:"status"`
	Active            bool   `json:"active"`
	TodayCheckedIn    bool   `json:"today_checked_in"`
	StreakDays        int    `json:"streak_days"`
	TotalClaimDays    int    `json:"total_claim_days"`
	RewardCredits     int    `json:"reward_credits"`
	TotalRewardCredit int    `json:"total_reward_credits"`
	NextClaimAt       int64  `json:"next_claim_at"`
	LastClaimedAt     int64  `json:"last_claimed_at"`
	RewardExpiresAt   int64  `json:"reward_expires_at"`
}

// CheckinStatusResult is what the panel shows.
type CheckinStatusResult struct {
	OK          bool           `json:"ok"`
	Unavailable bool           `json:"unavailable,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	HTTP        int            `json:"http,omitempty"`
	Error       string         `json:"error,omitempty"`
	Status      *CheckinStatus `json:"status,omitempty"`
}

// CheckinResult is one claim attempt.
type CheckinResult struct {
	OK            bool   `json:"ok"`
	Already       bool   `json:"already,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Unavailable   bool   `json:"unavailable,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Status        string `json:"status,omitempty"`
	Msg           string `json:"msg,omitempty"`
	Error         string `json:"error,omitempty"`
	RewardCredits int    `json:"reward_credits,omitempty"`
	StreakDays    int    `json:"streak_days,omitempty"`

	// Campaign carries the campaign-platform detail (which campaigns were
	// claimed, pending, locked or deduplicated). The panel forwards it so an
	// operator can tell "already claimed" from "nothing to claim".
	Campaign *CampaignCheckinResult `json:"campaign,omitempty"`
}

var openAPI404Mu sync.Mutex
var openAPI404 = map[string]float64{}

// OpenAPIMissing reports whether (scope, path) is known to 404 here.
func OpenAPIMissing(scope, path string) bool {
	openAPI404Mu.Lock()
	defer openAPI404Mu.Unlock()
	ts := openAPI404[scope+"|"+path]
	return time.Since(time.Unix(int64(ts), 0)) < 6*time.Hour
}

// MarkOpenAPIMissing records a 404 for (scope, path) with a six-hour TTL.
func MarkOpenAPIMissing(scope, path string) {
	openAPI404Mu.Lock()
	openAPI404[scope+"|"+path] = float64(time.Now().Unix())
	openAPI404Mu.Unlock()
}

// CheckinStatusFor probes the legacy daily check-in endpoint.
//
// Availability is probed at runtime rather than hardcoded per realm: the
// official service has moved these endpoints before, in both directions.
func (a *Account) CheckinStatusFor() *CheckinStatusResult {
	cfg := a.RealmConfig()
	url := cfg.OpenAPI + PathCheckinStatus
	data, err := DefaultClient.DoJSON(Request{
		Method: "GET", URL: url, Headers: a.AccountHeaders(), Timeout: 8 * time.Second,
	})
	if err != nil {
		he, isHTTP := err.(*HTTPStatusError)
		if isHTTP {
			switch he.Code {
			case 404, 405, 410:
				MarkOpenAPIMissing(a.EndpointScope(), PathCheckinStatus)
				a.MarkCheckinCapability(false, fmt.Sprintf("%s (HTTP %d)", CheckinReasonNotFound, he.Code))
				return &CheckinStatusResult{
					OK: false, Unavailable: true, Reason: CheckinReasonNotFound,
					HTTP:  he.Code,
					Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(he.Body, 160)),
				}
			case 403:
				// Usually an egress/WAF block, not a missing endpoint: do not
				// cache the capability so the next run probes again.
				return &CheckinStatusResult{
					OK: false, Unavailable: true, HTTP: 403,
					Reason: "上游拒绝访问（HTTP 403）：可能是出口 IP/代理被拦截，或本区域未开放该接口",
					Error:  fmt.Sprintf("HTTP 403 %s", truncate(he.Body, 160)),
				}
			default:
				return &CheckinStatusResult{OK: false, HTTP: he.Code,
					Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(he.Body, 160))}
			}
		}
		return &CheckinStatusResult{OK: false, Error: err.Error()}
	}
	a.MarkCheckinCapability(true, "")
	last := ""
	if v := data["lastClaimedAt"]; v != nil && asInt(v) > 0 {
		last = time.Unix(asInt(v), 0).In(utc8).Format("2006-01-02")
	}
	today := time.Now().In(utc8).Format("2006-01-02")
	status := asString(data["status"])
	return &CheckinStatusResult{OK: true, Status: &CheckinStatus{
		Status:            status,
		Active:            status == "CLAIMABLE" || status == "CLAIMED",
		TodayCheckedIn:    status == "CLAIMED" && last == today,
		StreakDays:        int(asInt(data["currentStreakDays"])),
		TotalClaimDays:    int(asInt(data["totalClaimDays"])),
		RewardCredits:     int(asInt(data["rewardCredits"])),
		TotalRewardCredit: int(asInt(data["totalRewardCredits"])),
		NextClaimAt:       asInt(data["nextClaimAt"]),
		LastClaimedAt:     asInt(data["lastClaimedAt"]),
		RewardExpiresAt:   asInt(data["rewardExpiresAt"]),
	}}
}

// Checkin claims today's daily credits.
//
// Order is deliberate and mirrors the Python original:
//
//  1. the campaign platform, which is what the official desktop client uses
//     now. The legacy /sash daily-check-in endpoint was retired upstream, so
//     leading with it left every account reporting "endpoint not open" and no
//     credits were ever claimed;
//  2. the legacy sash endpoint, only as a fallback for old accounts/campaigns
//     that still answer there.
//
// The campaign leg reports success for all of: claimed, already claimed,
// per-person dedup, and nothing-to-claim. Only an unfetchable list falls
// through to the legacy path, so a normal account never triggers two upstream
// claims.
func (a *Account) Checkin(accountsDir string) CheckinResult {
	camp := a.CampaignCheckin(1.0, true)
	if camp.OK && campaignHasConclusion(camp) {
		a.refreshCreditsQuietly(accountsDir)
		return CheckinResult{
			OK:            true,
			Already:       len(camp.Already) > 0 && len(camp.Claimed) == 0,
			Msg:           camp.Message,
			Error:         "",
			RewardCredits: camp.Earned,
			Campaign:      camp,
		}
	}
	return a.legacyCheckin(accountsDir, camp)
}

// campaignHasConclusion reports whether the campaign run produced a
// definitive answer rather than "could not ask".
//
// A list that was fetched successfully always counts, including the
// nothing-to-claim case: that is a real conclusion (the account already
// claimed, or the quota is out) and retrying the legacy endpoint would only
// add a second, contradictory data point.
func campaignHasConclusion(res *CampaignCheckinResult) bool {
	if res == nil {
		return false
	}
	return len(res.Claimed) > 0 || len(res.Already) > 0 ||
		len(res.Blocked) > 0 || len(res.Pending) > 0 ||
		len(res.Locked) > 0 || res.Message != ""
}

// legacyCheckin is the old /sash daily-check-in path, kept as a fallback.
func (a *Account) legacyCheckin(accountsDir string, camp *CampaignCheckinResult) CheckinResult {
	res := a.CheckinStatusFor()
	if !res.OK {
		if res.Unavailable {
			// Both paths are unavailable: report the campaign error, which is
			// the one that matters today, instead of a misleading "endpoint
			// not open" for a legacy API nobody uses any more.
			msg := fmt.Sprintf("本区域未开放 /sash/api/v1/me/daily-check-in 接口（HTTP %d）", res.HTTP)
			if camp != nil && camp.Error != "" {
				msg = "活动平台查询失败：" + camp.Error + "；旧签到接口同样不可用"
			}
			return CheckinResult{OK: true, Unavailable: true, Reason: res.Reason,
				Msg: msg, Campaign: camp}
		}
		detail := firstNonEmpty(res.Error, "check-in status failed")
		if camp != nil && camp.Error != "" {
			detail = "活动平台：" + camp.Error + "；旧接口：" + detail
		}
		return CheckinResult{OK: false, Error: detail, Campaign: camp}
	}
	if res.Status.TodayCheckedIn {
		// The upstream already claimed today: stamp locally, otherwise
		// lastCheckin stays null and every scheduler cycle re-walks this account.
		a.StampCheckin()
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到",
			StreakDays: res.Status.StreakDays, RewardCredits: res.Status.RewardCredits,
			Campaign: camp}
	}
	if !res.Status.Active {
		return CheckinResult{OK: true, Disabled: true, Status: res.Status.Status,
			Msg:      fmt.Sprintf("官方签到活动未开放 (status=%s)", firstNonEmpty(res.Status.Status, "?")),
			Campaign: camp}
	}

	cfg := a.RealmConfig()
	url := cfg.OpenAPI + PathCheckinClaim
	data, err := DefaultClient.DoJSON(Request{
		Method: "POST", URL: url,
		Headers: mergeHeaders(a.AccountHeaders(), map[string]string{"Content-Type": "application/json"}),
		Body:    []byte("{}"),
		Timeout: 15 * time.Second,
	})
	if err != nil {
		body := ""
		if he, ok := err.(*HTTPStatusError); ok {
			body = he.Body
		}
		// Concurrent/duplicate claims come back as 409 ALREADY_CLAIMED.
		if strings.Contains(body, "ALREADY_CLAIMED") {
			a.StampCheckin()
			a.refreshCreditsQuietly(accountsDir)
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到", Campaign: camp}
		}
		return CheckinResult{OK: false, Error: truncate(body, 160), Campaign: camp}
	}
	if asString(data["result"]) == "ALREADY_CLAIMED" ||
		(data["success"] == false && strings.Contains(strings.ToUpper(asString(data["error"])), "ALREADY")) {
		a.StampCheckin()
		a.refreshCreditsQuietly(accountsDir)
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到", Campaign: camp}
	}
	if data["success"] == false {
		// Re-check once: the upstream may have booked it anyway.
		if res2 := a.CheckinStatusFor(); res2.OK && res2.Status.TodayCheckedIn {
			a.StampCheckin()
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到（复查确认）", Campaign: camp}
		}
		return CheckinResult{OK: false, Error: truncate(asString(data["error"]), 160), Campaign: camp}
	}
	reward := int(asInt(data["rewardCredits"]))
	a.StampCheckin()
	// The reward lands in addOnQuota; without a refresh the panel keeps showing
	// the old number ("says +100 but the balance did not move").
	a.refreshCreditsQuietly(accountsDir)
	return CheckinResult{OK: true, Msg: fmt.Sprintf("签到成功 +%d 积分", reward), RewardCredits: reward}
}

func (a *Account) refreshCreditsQuietly(accountsDir string) {
	defer func() { _ = recover() }()
	a.FetchCredits(accountsDir)
}

// ---------------------------------------------------------------------------
// Campaign platform
// ---------------------------------------------------------------------------

// CampaignBenefit is one campaign's reward description.
type CampaignBenefit struct {
	Kind   string `json:"kind"`
	Amount int    `json:"amount,omitempty"`
}

// Campaign is one entry in the official campaign list.
type Campaign struct {
	Key         string          `json:"campaign_key"`
	ID          string          `json:"campaign_id"`
	TitleZH     string          `json:"title_zh"`
	Description string          `json:"description_zh"`
	ClaimStatus string          `json:"claim_status"`
	Benefit     CampaignBenefit `json:"benefit"`
	URL         string          `json:"campaign_url"`

	// ActionType decides whether this campaign can be claimed at all.
	// VIEW_DETAILS entries are informational; claiming them is rejected.
	ActionType string `json:"action_type"`
	// UnavailableReason carries the upstream's why-not code, e.g.
	// REDEMPTION_CODE_OUT_OF_STOCK or ACHIEVEMENT_NOT_COMPLETED.
	UnavailableReason string `json:"unavailable_reason"`
	// RequiredAchievement names the task gating a locked campaign.
	RequiredAchievement string `json:"required_achievement_key"`
	// AchievementCompleted is a tri-state: nil when upstream omits the field.
	AchievementCompleted *bool `json:"achievement_completed"`
}

// CampaignView is the normalized campaign platform state.
type CampaignView struct {
	OK        bool       `json:"ok"`
	Show      bool       `json:"show_campaign"`
	Claimable bool       `json:"claimable"`
	URL       string     `json:"url"`
	Campaigns []Campaign `json:"campaigns"`
	Error     string     `json:"error,omitempty"`
}

// FetchCampaigns lists the official campaign platform's live campaigns.
func (a *Account) FetchCampaigns() *CampaignView {
	cfg := a.RealmConfig()
	var lastErr string
	for _, base := range OpenAPICandidates(a.Realm, cfg) {
		data, err := DefaultClient.DoJSON(Request{
			Method: "GET", URL: base + PathCampaigns,
			Headers: a.DesktopHeaders(), Timeout: 20 * time.Second,
		})
		if err != nil {
			lastErr = err.Error()
			if he, ok := err.(*HTTPStatusError); ok {
				lastErr = fmt.Sprintf("HTTP %d", he.Code)
				// A business 4xx is not a host problem: do not fall through.
				if he.Code != 404 {
					break
				}
			}
			continue
		}
		return parseCampaigns(data)
	}
	return &CampaignView{OK: false, Error: lastErr}
}

func parseCampaigns(data map[string]any) *CampaignView {
	out := &CampaignView{OK: true}
	out.Show = truthyDefault(data["showCampaign"], true)
	out.URL = firstNonEmpty(asString(data["campaignUrl"]), asString(data["url"]))
	items, _ := data["campaigns"].([]any)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := Campaign{
			Key:         asString(m["campaign_key"]),
			ID:          asString(m["campaign_id"]),
			ClaimStatus: strings.ToUpper(asString(m["claim_status"])),
			// The upstream mixes camelCase and snake_case across realms; accept
			// either rather than silently classifying every campaign as
			// claimable.
			ActionType: strings.ToUpper(firstNonEmpty(
				asString(m["actionType"]), asString(m["action_type"]))),
			UnavailableReason: strings.ToUpper(firstNonEmpty(
				asString(m["unavailableReason"]), asString(m["unavailable_reason"]))),
			RequiredAchievement: firstNonEmpty(
				asString(m["requiredAchievementKey"]), asString(m["required_achievement_key"])),
		}
		if v, ok := m["achievementCompleted"]; ok {
			b := asBool(v)
			c.AchievementCompleted = &b
		} else if v, ok := m["achievement_completed"]; ok {
			b := asBool(v)
			c.AchievementCompleted = &b
		}
		if c.Key == "" {
			c.Key = c.ID
		}
		if placements, ok := m["placements"].(map[string]any); ok {
			if content, ok := placements["content"].(map[string]any); ok {
				if zh, ok := content["zh"].(map[string]any); ok {
					c.TitleZH = asString(zh["title"])
					c.Description = asString(zh["description"])
				}
			}
		}
		if benefit, ok := m["benefit"].(map[string]any); ok {
			c.Benefit = CampaignBenefit{
				Kind:   strings.ToUpper(asString(benefit["kind"])),
				Amount: int(asInt(benefit["amount"])),
			}
		}
		if c.ClaimStatus == "CLAIMABLE" {
			out.Claimable = true
		}
		out.Campaigns = append(out.Campaigns, c)
	}
	return out
}

// CampaignClaimResult reports one claim attempt.
type CampaignClaimResult struct {
	OK      bool   `json:"ok"`
	Already bool   `json:"already,omitempty"`
	Code    string `json:"redemption_code,omitempty"`
	Message string `json:"msg,omitempty"`
	Error   string `json:"error,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
	Credits int    `json:"credits,omitempty"`
}

// CampaignFailureCN maps upstream failure codes onto the official front end's
// Chinese wording.
var CampaignFailureCN = map[string]string{
	"REDEMPTION_CODE_OUT_OF_STOCK": "今日名额已发完（每日 10:00 刷新，次日再来）",
	"ACHIEVEMENT_NOT_COMPLETED":    "需先完成新人任务（成就未完成）",
	"CAMPAIGN_NOT_ACTIVE":          "活动已结束/未开始",
	"RISK_BLOCKED":                 "风控拦截（当前设备/账号不可领取）",
	"RISK_DEPENDENCY_UNAVAILABLE":  "风控服务不可用，稍后重试",
}

// ClaimCampaign claims one campaign's reward.
//
// The claim is idempotent upstream: a repeat returns {"status":"CLAIMED",
// "replayed":true} and does not grant twice.
func (a *Account) ClaimCampaign(campaignID string, accountsDir string) CampaignClaimResult {
	if campaignID == "" {
		return CampaignClaimResult{OK: false, Error: "campaign id required"}
	}
	cfg := a.RealmConfig()
	url := cfg.OpenAPI + fmt.Sprintf(PathCampaignClaim, campaignID)
	data, err := DefaultClient.DoJSON(Request{
		Method: "POST", URL: url, Headers: a.DesktopHeaders(),
		Body: []byte("{}"), Timeout: 20 * time.Second,
	})
	if err != nil {
		return CampaignClaimResult{OK: false, Error: err.Error()}
	}
	status := strings.ToUpper(asString(data["status"]))
	if status == "CLAIMED" && asBool(data["replayed"]) {
		return CampaignClaimResult{OK: true, Already: true, Message: "今日已领取"}
	}
	if code := asString(data["redemption_code"]); code != "" {
		a.mu.Lock()
		if a.CampaignCodes == nil {
			a.CampaignCodes = map[string]string{}
		}
		a.CampaignCodes[campaignID] = code
		a.mu.Unlock()
		_ = a.Save(dirOf(a.Path))
	}
	failure := strings.ToUpper(asString(data["failure_code"]))
	if failure == "" {
		failure = strings.ToUpper(asString(data["reason"]))
	}
	if msg, ok := CampaignFailureCN[failure]; ok {
		if failure == "RISK_BLOCKED" || failure == "ACHIEVEMENT_NOT_COMPLETED" {
			// Server-side dedup is per person; retrying repeatedly only
			// produces log noise.
			a.mu.Lock()
			if a.CampaignBlockedUntil == nil {
				a.CampaignBlockedUntil = map[string]float64{}
			}
			a.CampaignBlockedUntil[campaignID] = float64(time.Now().Unix()) + 6*3600
			a.mu.Unlock()
		}
		return CampaignClaimResult{OK: false, Error: msg, Blocked: true}
	}
	if data["success"] == false {
		return CampaignClaimResult{OK: false,
			Error: firstNonEmpty(asString(data["error"]), asString(data["message"]), "claim failed")}
	}
	credits := int(asInt(data["credits"]) + asInt(data["rewardCredits"]))
	a.refreshCreditsQuietly(accountsDir)
	return CampaignClaimResult{OK: true,
		Code: asString(data["redemption_code"]), Credits: credits, Message: "领取成功"}
}

func mergeHeaders(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// CampaignCheckin claims every CLAIMABLE credits campaign for one account.
//
// This is the official daily check-in path: the legacy /sash daily-check-in
// endpoint was retired, and the campaign platform is what the desktop client
// actually calls now. Ordering matters and mirrors the Python original
// (qoder_accounts.campaign_checkin):
//
//  1. list campaigns (forced, no cache) — the claim path must see live state;
//  2. retry on an EMPTY/claimable-less list, because upstream intermittently
//     returns an empty list (~2 in 10 in practice) and one empty answer would
//     silently lose the day's reward;
//  3. classify each campaign BEFORE filtering on action type: after a claim the
//     upstream downgrades actionType to VIEW_DETAILS while the reward is
//     already booked, so filtering first would report nothing-to-claim for an
//     account that did claim today;
//  4. claim sequentially with a gap — the service treats bursts as abuse.
//
// onlyCredits restricts the run to CREDITS-kind campaigns (the panel's daily
// check-in button does not touch coupon/redemption-code campaigns).
//
// gap is the delay between adjacent claim requests; zero means no delay and is
// legal, so an explicit 0 from the caller is honoured instead of clamped.
func (a *Account) CampaignCheckin(gap float64, onlyCredits bool) *CampaignCheckinResult {
	res := &CampaignCheckinResult{
		Pending: []string{},
		Locked:  []string{},
		Claimed: []string{},
		Already: []string{},
		Blocked: []string{},
		Codes:   []CampaignCode{},
	}
	if gap < 0 {
		gap = 0
	}

	// The claim path must not read a cached list.
	st := a.FetchCampaigns()
	for _, wait := range []float64{0.8, 2.0} {
		if !campaignListNeedsRetry(st, onlyCredits) {
			break
		}
		time.Sleep(time.Duration(wait * float64(time.Second)))
		if retry := a.FetchCampaigns(); retry.OK {
			st = retry
		}
	}
	if st == nil || !st.OK {
		res.OK = false
		res.Error = "campaign list unavailable"
		if st != nil && st.Error != "" {
			res.Error = st.Error
		}
		window := NextCheckinWindow(time.Now())
		res.NextAvailableAt, res.NextAvailableNote = window.Epoch, window.Note
		return res
	}

	for _, c := range st.Campaigns {
		if onlyCredits && c.Benefit.Kind != "" && c.Benefit.Kind != "CREDITS" {
			continue
		}
		label := c.Label()
		if c.ClaimStatus == "CLAIMED" {
			// Classify before the action-type filter: a claimed campaign whose
			// actionType was downgraded to VIEW_DETAILS is still already-claimed.
			if c.Benefit.Amount > 0 || c.ActionType == "" || c.ActionType == "CLAIM_BENEFIT" {
				res.Already = append(res.Already, label)
			}
			if code := a.CampaignCode(c.ID); code != "" {
				res.Codes = append(res.Codes, CampaignCode{Campaign: label, Code: code})
			}
			continue
		}
		// Detail-only entries have nothing to claim.
		if c.ActionType != "" && c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if c.ClaimStatus != "CLAIMABLE" {
			switch {
			case c.UnavailableReason == "REDEMPTION_CODE_OUT_OF_STOCK":
				res.Pending = append(res.Pending, label)
			case c.UnavailableReason == "ACHIEVEMENT_NOT_COMPLETED",
				c.AchievementCompleted != nil && !*c.AchievementCompleted:
				res.Locked = append(res.Locked, label)
			}
			continue
		}
		if a.IsCampaignBlocked(c.ID, float64(time.Now().Unix())) {
			res.Blocked = append(res.Blocked, label)
			continue
		}

		claim := a.ClaimCampaign(c.ID, "")
		switch {
		case claim.OK && claim.Already:
			res.Already = append(res.Already, label)
			res.Stamped = true
		case claim.OK:
			res.Claimed = append(res.Claimed, label)
			amount := claim.Credits
			if amount == 0 {
				amount = c.Benefit.Amount
			}
			res.Earned += amount
			res.Stamped = true
			if claim.Code != "" {
				res.Codes = append(res.Codes, CampaignCode{Campaign: label, Code: claim.Code})
			}
		case claim.Blocked:
			// Server-side dedup is per person, not per account: a single-account
			// pool still sees SAME_PERSON_ALREADY_CLAIMED. Stamping stops the
			// scheduler from re-hitting upstream every cycle for the same answer.
			res.Blocked = append(res.Blocked, label)
			res.Stamped = true
		default:
			res.Errors = append(res.Errors, label+": "+firstNonEmpty(claim.Error, "claim failed"))
		}
		if gap > 0 {
			time.Sleep(time.Duration(gap * float64(time.Second)))
		}
	}

	if len(res.Already) > 0 {
		res.Stamped = true
	}
	// A stamped run is proof today's check-in happened; write the local date
	// once here rather than inside the loop, so a multi-campaign run touches
	// the credential file a single time.
	if res.Stamped && a.CanCheckin() {
		a.StampCheckin()
	}
	res.OK = len(res.Errors) == 0
	res.Message = res.Summary()
	window := NextCheckinWindow(time.Now())
	res.NextAvailableAt, res.NextAvailableNote = window.Epoch, window.Note
	return res
}

// CampaignCheckinResult is the outcome of one campaign-based check-in.
type CampaignCheckinResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
	Earned  int    `json:"earned"`
	// Claimed lists campaigns that granted a reward in THIS run.
	Claimed []string `json:"claimed"`
	// Already lists campaigns already claimed today.
	Already []string `json:"already"`
	// Blocked lists campaigns the upstream deduplicated per person.
	Blocked []string `json:"blocked"`
	// Pending lists campaigns whose daily quota is exhausted.
	Pending []string `json:"pending"`
	// Locked lists campaigns gated on an unfinished newcomer task.
	Locked []string       `json:"locked"`
	Codes  []CampaignCode `json:"codes"`
	Errors []string       `json:"errors,omitempty"`
	// Stamped reports whether the local lastCheckin date was written.
	Stamped bool `json:"stamped"`

	NextAvailableAt   int64  `json:"next_available_at"`
	NextAvailableNote string `json:"next_available_note"`
}

// CampaignCode is one redemption code produced by a claim.
type CampaignCode struct {
	Campaign string `json:"campaign"`
	Code     string `json:"code"`
}

// Label is the campaign's display name: the official Chinese title when
// present, otherwise the key.
func (c Campaign) Label() string {
	if v := strings.TrimSpace(c.TitleZH); v != "" {
		return v
	}
	return firstNonEmpty(c.Key, c.ID)
}

// Summary renders the log line the panel shows, in the same order of
// precedence as the Python original: claimed > blocked > already > pending >
// locked > errors > nothing to do.
func (r *CampaignCheckinResult) Summary() string {
	switch {
	case len(r.Claimed) > 0:
		return fmt.Sprintf("活动领取成功 +%d Credits（%s）", r.Earned, strings.Join(r.Claimed, "、"))
	case len(r.Blocked) > 0:
		return "同人已领取：同一设备/身份下的其他账号本轮已领过（服务端按人去重）"
	case len(r.Already) > 0:
		return "今日活动奖励已领取（" + strings.Join(r.Already, "、") + "）"
	case len(r.Pending) > 0:
		return "名额已发完，次日 10:00 后可再领：" + strings.Join(r.Pending, "、")
	case len(r.Locked) > 0:
		return "需先在官方桌面端完成新人任务后可领：" + strings.Join(r.Locked, "、")
	case len(r.Errors) > 0:
		return "活动领取失败：" + truncate(strings.Join(r.Errors, "; "), 200)
	default:
		return "当前账号暂无可领取的官方活动"
	}
}

// campaignListNeedsRetry reports whether a successful-but-empty answer should
// be retried. Only empty results are retried: a query failure has its own error
// path, and retrying it would just multiply the latency.
func campaignListNeedsRetry(st *CampaignView, onlyCredits bool) bool {
	if st == nil || !st.OK {
		return false
	}
	if len(st.Campaigns) == 0 {
		return true
	}
	if !st.Show && !st.Claimable {
		return true
	}
	for _, c := range st.Campaigns {
		if c.ClaimStatus != "CLAIMABLE" {
			continue
		}
		if onlyCredits && c.Benefit.Kind != "" && c.Benefit.Kind != "CREDITS" {
			continue
		}
		return false
	}
	return true
}
