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
func (a *Account) Checkin(accountsDir string) CheckinResult {
	res := a.CheckinStatusFor()
	if !res.OK {
		if res.Unavailable {
			return CheckinResult{OK: true, Unavailable: true, Reason: res.Reason,
				Msg: fmt.Sprintf("本区域未开放 /sash/api/v1/me/daily-check-in 接口（HTTP %d）：每日领取活动改由官方客户端承接", res.HTTP)}
		}
		return CheckinResult{OK: false, Error: firstNonEmpty(res.Error, "check-in status failed")}
	}
	if res.Status.TodayCheckedIn {
		// The upstream already claimed today: stamp locally, otherwise
		// lastCheckin stays null and every scheduler cycle re-walks this account.
		a.StampCheckin()
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到",
			StreakDays: res.Status.StreakDays, RewardCredits: res.Status.RewardCredits}
	}
	if !res.Status.Active {
		return CheckinResult{OK: true, Disabled: true, Status: res.Status.Status,
			Msg: fmt.Sprintf("官方签到活动未开放 (status=%s)", firstNonEmpty(res.Status.Status, "?"))}
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
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到"}
		}
		return CheckinResult{OK: false, Error: truncate(body, 160)}
	}
	if asString(data["result"]) == "ALREADY_CLAIMED" ||
		(data["success"] == false && strings.Contains(strings.ToUpper(asString(data["error"])), "ALREADY")) {
		a.StampCheckin()
		a.refreshCreditsQuietly(accountsDir)
		return CheckinResult{OK: true, Already: true, Msg: "今日已签到"}
	}
	if data["success"] == false {
		// Re-check once: the upstream may have booked it anyway.
		if res2 := a.CheckinStatusFor(); res2.OK && res2.Status.TodayCheckedIn {
			a.StampCheckin()
			return CheckinResult{OK: true, Already: true, Msg: "今日已签到（复查确认）"}
		}
		return CheckinResult{OK: false, Error: truncate(asString(data["error"]), 160)}
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
