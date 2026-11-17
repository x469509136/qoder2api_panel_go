package qoder

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CheckinStatus is the normalised legacy daily check-in state.
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

// CheckinStatusResult is what a caller shows the operator.
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
	// Campaign carries the campaign-platform detail.
	Campaign *CampaignCheckinResult `json:"campaign,omitempty"`
	// Stamped reports whether the caller should persist today's date.
	Stamped bool `json:"stamped,omitempty"`
}

// CheckinReport summarises a daily check-in for one credential.
type CheckinReport struct {
	// Already reports the reward was already taken today.
	Already bool `json:"already"`
	// Earned is the credit granted in THIS run.
	Earned int `json:"earned"`
	// Message is the human summary.
	Message string `json:"message"`
	// Stamped means "today's check-in is settled" — persist the date even when
	// Earned is 0 (already claimed or deduplicated per person).
	Stamped bool `json:"stamped"`
	// NextAvailableAt is the next 10:00 UTC+8 boundary.
	NextAvailableAt   int64  `json:"next_available_at"`
	NextAvailableNote string `json:"next_available_note"`
	// Campaign carries the detail (claimed / pending / locked / blocked).
	Campaign *CampaignCheckinResult `json:"campaign,omitempty"`
	// UsesLegacy is true when the fallback endpoint answered.
	UsesLegacy bool `json:"uses_legacy,omitempty"`
	// Error is set when nothing succeeded.
	Error string `json:"error,omitempty"`
}

// CheckinStatus reads the legacy /sash daily check-in state.
//
// Availability is probed at runtime rather than hardcoded per realm: the
// official service has moved these endpoints before, in both directions.
func (c *Client) CheckinStatus(ctx context.Context, cred Credential) *CheckinStatusResult {
	cred, err := requireCredential("checkin status", cred)
	if err != nil {
		return &CheckinStatusResult{OK: false, Error: err.Error()}
	}
	cfg, cfgErr := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if cfgErr != nil {
		return &CheckinStatusResult{OK: false, Error: cfgErr.Error()}
	}
	data, doErr := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(cfg) + PathCheckinStatus,
		Headers: c.AccountHeaders(cfg, cred), Timeout: 8 * time.Second,
	})
	if doErr != nil {
		if he, isHTTP := doErr.(*HTTPStatusError); isHTTP {
			if he.IsNotFound() {
				return &CheckinStatusResult{
					OK: false, Unavailable: true, Reason: "checkin_endpoint_not_found",
					HTTP:  he.Code,
					Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(he.Body, 160)),
				}
			}
			if he.Code == 403 {
				return &CheckinStatusResult{
					OK: false, Unavailable: true, HTTP: 403,
					Reason: "上游拒绝访问（HTTP 403）：可能是出口 IP/代理被拦截，或本区域未开放该接口",
					Error:  fmt.Sprintf("HTTP 403 %s", truncate(he.Body, 160)),
				}
			}
			return &CheckinStatusResult{OK: false, HTTP: he.Code,
				Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(he.Body, 160))}
		}
		return &CheckinStatusResult{OK: false, Error: doErr.Error()}
	}
	last := ""
	if v := data["lastClaimedAt"]; v != nil && asInt(v) > 0 {
		last = time.Unix(asInt(v), 0).In(UTC8).Format("2006-01-02")
	}
	today := time.Now().In(UTC8).Format("2006-01-02")
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

// LegacyCheckin performs the retired /sash daily claim.
//
// Kept because older accounts and some realms still answer here; it is a
// fallback, never the primary path.
func (c *Client) LegacyCheckin(ctx context.Context, cred Credential) CheckinResult {
	status := c.CheckinStatus(ctx, cred)
	if !status.OK {
		if status.Unavailable {
			return CheckinResult{OK: true, Unavailable: true, Reason: status.Reason,
				Msg: fmt.Sprintf("本区域未开放 /sash/api/v1/me/daily-check-in 接口（HTTP %d）", status.HTTP)}
		}
		return CheckinResult{OK: false, Error: firstNonEmpty(status.Error, "check-in status failed")}
	}
	if status.Status.TodayCheckedIn {
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到",
			StreakDays: status.Status.StreakDays, Stamped: true}
	}
	if !status.Status.Active {
		return CheckinResult{OK: true, Disabled: true, Status: status.Status.Status,
			Msg: fmt.Sprintf("官方签到活动未开放 (status=%s)", firstNonEmpty(status.Status.Status, "?"))}
	}

	cfg, _ := RealmConfigFor(cred.Realm, cred.VPCOverride)
	data, err := c.http.DoJSON(ctx, Request{
		Method: "POST", URL: primaryOpenAPI(cfg) + PathCheckinClaim,
		Headers: c.AccountHeaders(cfg, cred), Body: []byte("{}"),
		Timeout: 15 * time.Second,
	})
	if err != nil {
		body := ""
		if he, ok := err.(*HTTPStatusError); ok {
			body = he.Body
		}
		// Concurrent/duplicate claims come back as 409 ALREADY_CLAIMED.
		if strings.Contains(body, "ALREADY_CLAIMED") {
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到", Stamped: true}
		}
		return CheckinResult{OK: false, Error: truncate(body, 160)}
	}
	if asString(data["result"]) == "ALREADY_CLAIMED" ||
		(data["success"] == false && strings.Contains(strings.ToUpper(asString(data["error"])), "ALREADY")) {
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到", Stamped: true}
	}
	if data["success"] == false {
		// Re-check once: the upstream may have booked it anyway.
		if again := c.CheckinStatus(ctx, cred); again.OK && again.Status.TodayCheckedIn {
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到（复查确认）", Stamped: true}
		}
		return CheckinResult{OK: false, Error: truncate(asString(data["error"]), 160)}
	}
	reward := int(asInt(data["rewardCredits"]))
	return CheckinResult{OK: true, Stamped: true, RewardCredits: reward,
		Msg: fmt.Sprintf("签到成功 +%d 积分", reward)}
}

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
	// UnavailableReason carries the upstream's why-not code.
	UnavailableReason string `json:"unavailable_reason"`
	// RequiredAchievement names the task gating a locked campaign.
	RequiredAchievement string `json:"required_achievement_key"`
	// AchievementCompleted is a tri-state: nil when upstream omits the field.
	AchievementCompleted *bool `json:"achievement_completed"`
}

// Label is the campaign's display name.
func (c Campaign) Label() string {
	if v := strings.TrimSpace(c.TitleZH); v != "" {
		return v
	}
	return firstNonEmpty(c.Key, c.ID)
}

// CampaignList is the normalised campaign platform state.
type CampaignList struct {
	OK        bool       `json:"ok"`
	Show      bool       `json:"show_campaign"`
	Claimable bool       `json:"claimable"`
	URL       string     `json:"url"`
	Campaigns []Campaign `json:"campaigns"`
	Error     string     `json:"error,omitempty"`
}

// FetchCampaigns lists the official campaign platform's live campaigns.
func (c *Client) FetchCampaigns(ctx context.Context, cred Credential) *CampaignList {
	cred, err := requireCredential("fetch campaigns", cred)
	if err != nil {
		return &CampaignList{OK: false, Error: err.Error()}
	}
	cfg, cfgErr := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if cfgErr != nil {
		return &CampaignList{OK: false, Error: cfgErr.Error()}
	}
	var lastErr string
	for _, base := range OpenAPICandidates(cred.Realm, cfg) {
		data, doErr := c.http.DoJSON(ctx, Request{
			Method: "GET", URL: base + PathCampaigns,
			Headers: c.DesktopHeaders(cfg, cred), Timeout: 20 * time.Second,
		})
		if doErr != nil {
			lastErr = doErr.Error()
			if he, isHTTP := doErr.(*HTTPStatusError); isHTTP {
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
	return &CampaignList{OK: false, Error: lastErr}
}

func parseCampaigns(data map[string]any) *CampaignList {
	out := &CampaignList{OK: true}
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
			// The upstream mixes camelCase and snake_case across realms.
			ActionType: strings.ToUpper(firstNonEmpty(
				asString(m["actionType"]), asString(m["action_type"]))),
			UnavailableReason: strings.ToUpper(firstNonEmpty(
				asString(m["unavailableReason"]), asString(m["unavailable_reason"]))),
			RequiredAchievement: firstNonEmpty(
				asString(m["requiredAchievementKey"]), asString(m["required_achievement_key"])),
		}
		if c.Key == "" {
			c.Key = c.ID
		}
		if v, ok := m["achievementCompleted"]; ok {
			b := asBool(v)
			c.AchievementCompleted = &b
		} else if v, ok := m["achievement_completed"]; ok {
			b := asBool(v)
			c.AchievementCompleted = &b
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
	// FailureCode is the raw upstream code, kept for logging.
	FailureCode string `json:"failure_code,omitempty"`
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
func (c *Client) ClaimCampaign(ctx context.Context, cred Credential, campaignID string) CampaignClaimResult {
	if campaignID == "" {
		return CampaignClaimResult{OK: false, Error: "campaign id required"}
	}
	cred, err := requireCredential("claim campaign", cred)
	if err != nil {
		return CampaignClaimResult{OK: false, Error: err.Error()}
	}
	cfg, cfgErr := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if cfgErr != nil {
		return CampaignClaimResult{OK: false, Error: cfgErr.Error()}
	}
	data, doErr := c.http.DoJSON(ctx, Request{
		Method:  "POST",
		URL:     primaryOpenAPI(cfg) + fmt.Sprintf(PathCampaignClaim, campaignID),
		Headers: c.DesktopHeaders(cfg, cred), Body: []byte("{}"),
		Timeout: 20 * time.Second,
	})
	if doErr != nil {
		return CampaignClaimResult{OK: false, Error: doErr.Error()}
	}

	status := strings.ToUpper(asString(data["status"]))
	failure := strings.ToUpper(asString(data["failure_code"]))
	if failure == "" {
		failure = strings.ToUpper(asString(data["reason"]))
	}
	if status == "CLAIMED" && asBool(data["replayed"]) {
		return CampaignClaimResult{OK: true, Already: true, Message: "今日已领取",
			Code: asString(data["redemption_code"]), FailureCode: failure}
	}
	if msg, known := CampaignFailureCN[failure]; known {
		blocked := failure == "RISK_BLOCKED" || failure == "ACHIEVEMENT_NOT_COMPLETED"
		return CampaignClaimResult{OK: false, Error: msg, Blocked: blocked, FailureCode: failure}
	}
	if data["success"] == false {
		return CampaignClaimResult{OK: false, FailureCode: failure,
			Error: firstNonEmpty(asString(data["error"]), asString(data["message"]), "claim failed")}
	}
	credits := int(asInt(data["credits"]) + asInt(data["rewardCredits"]))
	return CampaignClaimResult{OK: true, Credits: credits, FailureCode: failure,
		Code: asString(data["redemption_code"]), Message: "领取成功"}
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
	// Stamped reports whether today's check-in should be recorded locally.
	Stamped bool `json:"stamped"`
}

// CampaignCode is one redemption code produced by a claim.
type CampaignCode struct {
	Campaign string `json:"campaign"`
	Code     string `json:"code"`
}

// Summary renders the log line a panel shows, in the same order of precedence
// as the official flow: claimed > blocked > already > pending > locked > error.
func (r *CampaignCheckinResult) Summary() string {
	if r == nil {
		return ""
	}
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

// CampaignCheckin claims every CLAIMABLE credits campaign.
//
// This is the official daily check-in path. Ordering mirrors the official
// client and must not be reordered:
//
//  1. list campaigns (forced, no cache) — the claim path must see live state;
//  2. retry an EMPTY list, because upstream intermittently returns one and a
//     single empty answer would silently lose the day's reward;
//  3. classify BEFORE filtering on action type: after a claim the upstream
//     downgrades actionType to VIEW_DETAILS while the reward is booked;
//  4. claim sequentially with a gap — bursts are treated as abuse.
//
// onlyCredits restricts the run to CREDITS-kind campaigns. gap is the delay
// between adjacent claims; zero is legal and honoured.
func (c *Client) CampaignCheckin(ctx context.Context, cred Credential,
	gap float64, onlyCredits bool) *CampaignCheckinResult {
	res := &CampaignCheckinResult{
		Pending: []string{}, Locked: []string{}, Claimed: []string{},
		Already: []string{}, Blocked: []string{}, Codes: []CampaignCode{},
	}
	if gap < 0 {
		gap = 0
	}

	st := c.FetchCampaigns(ctx, cred)
	for _, wait := range []float64{0.8, 2.0} {
		if !campaignListNeedsRetry(st, onlyCredits) {
			break
		}
		if err := sleepCtx(ctx, time.Duration(wait*float64(time.Second))); err != nil {
			break
		}
		if retry := c.FetchCampaigns(ctx, cred); retry.OK {
			st = retry
		}
	}
	if st == nil || !st.OK {
		res.OK = false
		res.Error = "campaign list unavailable"
		if st != nil && st.Error != "" {
			res.Error = st.Error
		}
		return res
	}

	for _, camp := range st.Campaigns {
		if onlyCredits && camp.Benefit.Kind != "" && camp.Benefit.Kind != "CREDITS" {
			continue
		}
		label := camp.Label()
		if camp.ClaimStatus == "CLAIMED" {
			// Classify before the action-type filter.
			if camp.Benefit.Amount > 0 || camp.ActionType == "" || camp.ActionType == "CLAIM_BENEFIT" {
				res.Already = append(res.Already, label)
			}
			continue
		}
		if camp.ActionType != "" && camp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if camp.ClaimStatus != "CLAIMABLE" {
			switch {
			case camp.UnavailableReason == "REDEMPTION_CODE_OUT_OF_STOCK":
				res.Pending = append(res.Pending, label)
			case camp.UnavailableReason == "ACHIEVEMENT_NOT_COMPLETED",
				camp.AchievementCompleted != nil && !*camp.AchievementCompleted:
				res.Locked = append(res.Locked, label)
			}
			continue
		}

		claim := c.ClaimCampaign(ctx, cred, camp.ID)
		switch {
		case claim.OK && claim.Already:
			res.Already = append(res.Already, label)
			res.Stamped = true
		case claim.OK:
			res.Claimed = append(res.Claimed, label)
			amount := claim.Credits
			if amount == 0 {
				amount = camp.Benefit.Amount
			}
			res.Earned += amount
			res.Stamped = true
			if claim.Code != "" {
				res.Codes = append(res.Codes, CampaignCode{Campaign: label, Code: claim.Code})
			}
		case claim.Blocked:
			// Server-side dedup is per person, not per account: a single-account
			// pool still sees this. Stamping stops needless retries.
			res.Blocked = append(res.Blocked, label)
			res.Stamped = true
		default:
			res.Errors = append(res.Errors, label+": "+firstNonEmpty(claim.Error, "claim failed"))
		}
		if gap > 0 {
			if err := sleepCtx(ctx, time.Duration(gap*float64(time.Second))); err != nil {
				break
			}
		}
	}

	if len(res.Already) > 0 {
		res.Stamped = true
	}
	res.OK = len(res.Errors) == 0
	res.Message = res.Summary()
	return res
}

// Checkin is the one-call daily check-in a host wires to a button or scheduler.
//
// It runs the campaign platform first and falls back to the legacy endpoint
// only when the campaign list could not be fetched. A successful campaign list
// — including "nothing to claim" — is a conclusion and never falls through, so
// an account is never claimed twice by two mechanisms.
func (c *Client) Checkin(ctx context.Context, cred Credential) *CheckinReport {
	camp := c.CampaignCheckin(ctx, cred, 1.0, true)
	if camp.OK && campaignHasConclusion(camp) {
		window := NextCheckinWindow(time.Now())
		return &CheckinReport{
			Already:           len(camp.Already) > 0 && len(camp.Claimed) == 0,
			Earned:            camp.Earned,
			Message:           camp.Message,
			Stamped:           camp.Stamped,
			Campaign:          camp,
			NextAvailableAt:   window.Epoch,
			NextAvailableNote: window.Note,
		}
	}

	legacy := c.LegacyCheckin(ctx, cred)
	window := NextCheckinWindow(time.Now())
	report := &CheckinReport{
		Already:           legacy.Already,
		Earned:            legacy.RewardCredits,
		Message:           firstNonEmpty(legacy.Msg, legacy.Error),
		Stamped:           legacy.Stamped,
		UsesLegacy:        true,
		NextAvailableAt:   window.Epoch,
		NextAvailableNote: window.Note,
		Campaign:          camp,
	}
	if !legacy.OK {
		report.Error = legacy.Error
		if camp != nil && camp.Error != "" {
			report.Error = "活动平台：" + camp.Error + "；旧接口：" + legacy.Error
		}
	}
	return report
}

// campaignHasConclusion reports whether the campaign run produced a definitive
// answer rather than "could not ask".
func campaignHasConclusion(res *CampaignCheckinResult) bool {
	if res == nil {
		return false
	}
	return len(res.Claimed) > 0 || len(res.Already) > 0 ||
		len(res.Blocked) > 0 || len(res.Pending) > 0 ||
		len(res.Locked) > 0 || res.Message != ""
}

// campaignListNeedsRetry reports whether a successful-but-empty answer should
// be retried. Only empty results are retried: a query failure has its own
// error path.
func campaignListNeedsRetry(st *CampaignList, onlyCredits bool) bool {
	if st == nil || !st.OK {
		return false
	}
	if len(st.Campaigns) == 0 {
		return true
	}
	if !st.Show && !st.Claimable {
		return true
	}
	for _, camp := range st.Campaigns {
		if camp.ClaimStatus != "CLAIMABLE" {
			continue
		}
		if onlyCredits && camp.Benefit.Kind != "" && camp.Benefit.Kind != "CREDITS" {
			continue
		}
		return false
	}
	return true
}

// sleepCtx sleeps unless the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
