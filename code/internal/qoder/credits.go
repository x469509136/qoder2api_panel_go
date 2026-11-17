package qoder

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CreditSource records which (host, path) pair produced a parseable quota body.
// A host may persist this to avoid re-probing on every start.
type CreditSource struct {
	Host   string   `json:"host"`
	Path   string   `json:"path"`
	Fields []string `json:"fields"`
	At     float64  `json:"at"`
}

// CreditResult is the outcome of one credits refresh.
type CreditResult struct {
	OK      bool     `json:"ok"`
	HTTP    int      `json:"http,omitempty"`
	Error   string   `json:"error,omitempty"`
	Credits *Credits `json:"credits,omitempty"`
	// Source is the host+path that answered, when it was discovered.
	Source *CreditSource `json:"source,omitempty"`
}

// QuotaContainers lists the recognised container labels per category.
var QuotaContainers = map[string][]struct{ Kind, Label string }{
	"plan":   {{"plan", "套餐内"}},
	"addon":  {{"addon", "资源包"}},
	"sota":   {{"sota", "SOTA"}},
	"shared": {{"shared", "共享池"}},
}

var planFields = []string{"userQuota", "planQuota", "subscriptionQuota"}
var addonFields = []string{"addOnQuota", "resources"}
var sotaFields = []string{"sotaCredits", "sota", "sotaQuota"}
var sharedFields = []string{"orgResourcePackage", "sharedQuota", "teamQuota",
	"poolQuota", "sharedCredits", "sharedCredit"}

// NormalizeQuotaPayload splits an upstream quota body into the four categories.
// Unrecognised containers are simply absent — nothing is invented.
func NormalizeQuotaPayload(payload map[string]any) (composition []Composition, shared *SharedQuota) {
	if payload == nil {
		return nil, nil
	}
	for _, f := range planFields {
		if v, ok := payload[f]; ok {
			if b := bucketFrom(v, "plan", "套餐内"); b != nil {
				composition = append(composition, *b)
			}
			break
		}
	}
	for _, f := range addonFields {
		if v, ok := payload[f]; ok {
			if m, isMap := v.(map[string]any); isMap {
				if b := bucketFrom(m, "addon", "资源包"); b != nil {
					composition = append(composition, *b)
				}
			}
			if list, isList := v.([]any); isList {
				for i, item := range list {
					if b := bucketFrom(item, "addon", fmt.Sprintf("资源包 %d", i+1)); b != nil {
						composition = append(composition, *b)
					}
				}
			}
			break
		}
	}
	for _, f := range sotaFields {
		if v, ok := payload[f]; ok {
			if b := bucketFrom(v, "sota", "SOTA"); b != nil {
				composition = append(composition, *b)
			}
			break
		}
	}
	for _, f := range sharedFields {
		v, ok := payload[f]
		if !ok {
			continue
		}
		if m, isMap := v.(map[string]any); isMap {
			remain := pickNum(m, "remaining", "remain", "balance")
			used := pickNum(m, "used")
			size := pickNum(m, "cap")
			if size == 0 {
				size = pickNum(m, "total")
			}
			shared = &SharedQuota{
				Remain: int(remain), Used: int(used), Size: int(size),
				Available: truthyDefault(m["available"], true),
				Source:    f,
			}
			if size < 0 {
				// cap = -1 is the "no cap" sentinel, not a real amount.
				shared.Unlimited = true
			}
			break
		}
		if _, isBool := v.(bool); !isBool {
			if fv := asFloat(v); fv != 0 {
				shared = &SharedQuota{Remain: int(fv), Size: int(fv),
					Available: true, Source: f}
				break
			}
		}
	}
	return composition, shared
}

func bucketFrom(src any, kind, name string) *Composition {
	switch v := src.(type) {
	case float64:
		return &Composition{Name: name, Kind: kind, Remain: int(v), Size: int(v)}
	case map[string]any:
		remain := pickNum(v, "remaining", "remain", "balance")
		used := pickNum(v, "used", "consumed")
		size := pickNum(v, "total", "cap", "size")
		comp := &Composition{
			Name: name, Kind: kind,
			Remain: int(remain), Used: int(used), Size: int(size),
		}
		for _, k := range []string{"expiresAt", "expireAt", "expires_at"} {
			if v[k] != nil {
				comp.ExpiresAt = normalizeEpoch(v[k])
				break
			}
		}
		return comp
	}
	return nil
}

func pickNum(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if b, isBool := v.(bool); isBool && b {
				continue
			}
			return asFloat(v)
		}
	}
	return 0
}

// FetchCredits reads the quota endpoint and returns a normalised snapshot.
//
// INTL frequently 404/403s on /api/v2/quota/usage, so a previously discovered
// (host, path) pair can be supplied in hint to skip probing; when the direct
// path fails the candidate matrix is walked once and the winner is returned as
// CreditResult.Source for the caller to persist.
func (c *Client) FetchCredits(ctx context.Context, cred Credential, hint *CreditSource) *CreditResult {
	cred, err := requireCredential("fetch credits", cred)
	if err != nil {
		return &CreditResult{OK: false, Error: err.Error()}
	}
	cfg, err := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if err != nil {
		return &CreditResult{OK: false, Error: err.Error()}
	}

	// A remembered endpoint is tried first: it is the cheapest and the most
	// likely to still be right.
	if hint != nil && hint.Path != "" {
		full := strings.TrimRight(firstNonEmpty(hint.Host, primaryOpenAPI(cfg)), "/") + hint.Path
		if res := c.fetchCreditsAt(ctx, cfg, cred, full); res != nil && res.OK {
			return res
		}
	}

	url := primaryOpenAPI(cfg) + PathQuota
	data, err := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: url, Headers: c.AccountHeaders(cfg, cred),
		Timeout: 30 * time.Second,
	})
	if err == nil {
		return c.applyQuota(data, url)
	}

	// A 403 here is usually WAF/egress blocking or a realm-mismatched token.
	// Retrying once with the desktop header set distinguishes the two cases
	// before the more expensive probe.
	if he, isHTTP := err.(*HTTPStatusError); isHTTP && (he.Code == 401 || he.Code == 403) {
		if d2, retryErr := c.http.DoJSON(ctx, Request{
			Method: "GET", URL: url, Headers: c.DesktopHeaders(cfg, cred),
			Timeout: 30 * time.Second,
		}); retryErr == nil {
			return c.applyQuota(d2, url)
		}
	}

	if probe := c.ProbeCreditEndpoints(ctx, cred, 6); probe != nil && probe.OK {
		return probe
	}
	if he, isHTTP := err.(*HTTPStatusError); isHTTP {
		return &CreditResult{OK: false, HTTP: he.Code,
			Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(he.Body, 160))}
	}
	return &CreditResult{OK: false, Error: err.Error()}
}

func (c *Client) fetchCreditsAt(ctx context.Context, cfg RealmConfig,
	cred Credential, full string) *CreditResult {
	data, err := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: full, Headers: c.AccountHeaders(cfg, cred),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil
	}
	return c.applyQuota(data, full)
}

// ProbeCreditEndpoints walks the candidate host/path matrix and returns the
// first answer that carries recognisable quota numbers.
func (c *Client) ProbeCreditEndpoints(ctx context.Context, cred Credential, limit int) *CreditResult {
	cfg, err := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if err != nil {
		return nil
	}
	hosts := OpenAPICandidates(cred.Realm, cfg)
	if limit <= 0 || limit > len(CreditPaths) {
		limit = len(CreditPaths)
	}
	tried := 0
	for _, host := range hosts {
		for _, path := range CreditPaths {
			if tried >= limit {
				return nil
			}
			tried++
			full := strings.TrimRight(host, "/") + path
			data, doErr := c.http.DoJSON(ctx, Request{
				Method: "GET", URL: full, Headers: c.AccountHeaders(cfg, cred),
				Timeout: 15 * time.Second,
			})
			if doErr != nil {
				continue
			}
			composition, _ := NormalizeQuotaPayload(data)
			if len(composition) == 0 && !hasQuotaNumbers(data) {
				continue
			}
			res := c.applyQuota(data, full)
			if res != nil && res.OK {
				res.Source = &CreditSource{
					Host: host, Path: path, Fields: recognisedFields(data),
					At: float64(time.Now().Unix()),
				}
			}
			return res
		}
	}
	return nil
}

func hasQuotaNumbers(payload map[string]any) bool {
	for _, f := range allQuotaFields() {
		if v, ok := payload[f]; ok && v != nil {
			return true
		}
	}
	return false
}

func recognisedFields(payload map[string]any) []string {
	var out []string
	for _, f := range allQuotaFields() {
		if _, ok := payload[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

func allQuotaFields() []string {
	out := make([]string, 0, 32)
	out = append(out, planFields...)
	out = append(out, addonFields...)
	out = append(out, sotaFields...)
	out = append(out, sharedFields...)
	return out
}

// applyQuota normalises the payload into a Credits snapshot.
func (c *Client) applyQuota(q map[string]any, url string) *CreditResult {
	if q == nil {
		return &CreditResult{OK: false, Error: "empty quota response"}
	}
	uq, _ := q["userQuota"].(map[string]any)
	aq, _ := q["addOnQuota"].(map[string]any)
	numOf := func(m map[string]any, k string) float64 {
		if m == nil {
			return 0
		}
		return asFloat(m[k])
	}
	remain := int(numOf(uq, "remaining") + numOf(aq, "remaining"))
	used := int(numOf(uq, "used") + numOf(aq, "used"))
	size := int(numOf(uq, "total") + numOf(aq, "total"))

	composition, shared := NormalizeQuotaPayload(q)

	bothMissing := q["userQuota"] == nil && q["addOnQuota"] == nil
	if bothMissing && len(composition) > 0 {
		// An org member may get only orgResourcePackage; a missing personal
		// quota does not mean the query failed.
		return creditFromComposition(composition, shared, url)
	}

	hasShared := false
	for _, b := range composition {
		if b.Kind == "shared" {
			hasShared = true
		}
	}
	degenerate := bothMissing ||
		(asBool(q["isQuotaExceeded"]) && remain == 0 && size == 0 && !hasShared)
	if degenerate {
		reason := "both quota objects missing"
		if !bothMissing {
			reason = "exceeded=true with zero size/remain"
		}
		// Deliberately not zero: the caller must treat this as "unknown", not
		// "empty", or a revoked credential would trip balance guardrails.
		return &CreditResult{OK: false, Error: "degenerate quota body: " + reason}
	}

	remainEffective := remain
	if shared != nil && shared.Available && !shared.Unlimited {
		remainEffective += shared.Remain
	}
	creds := &Credits{
		Remain:          remain,
		RemainEffective: remainEffective,
		Used:            used,
		Size:            size,
		Exceeded:        asBool(q["isQuotaExceeded"]),
		ExpiresAt:       normalizeEpoch(q["expiresAt"]),
		Shared:          shared,
		Composition:     composition,
		Source:          url,
		UpdatedAt:       time.Now().Unix(),
		UpdatedISO:      time.Now().Format("2006-01-02 15:04:05"),
	}
	if v, ok := q["totalUsagePercentage"]; ok && v != nil {
		f := asFloat(v)
		creds.UsagePct = &f
	}
	creds.Packages = append(creds.Packages, map[string]any{
		"name": "基础额度", "remain": int(numOf(uq, "remaining")),
		"used": int(numOf(uq, "used")), "size": int(numOf(uq, "total")),
	})
	creds.Packages = append(creds.Packages, map[string]any{
		"name": "赠送/签到额度", "remain": int(numOf(aq, "remaining")),
		"used": int(numOf(aq, "used")), "size": int(numOf(aq, "total")),
	})
	if shared != nil {
		creds.Packages = append(creds.Packages, map[string]any{
			"name": "共享额度", "remain": shared.Remain, "used": shared.Used,
			"size": shared.Size, "available": shared.Available,
			"unlimited": shared.Unlimited, "source": shared.Source,
		})
	}
	// When the composition parser recognises containers the two-field sum
	// misses (SOTA etc.), its totals win.
	if len(composition) > 0 {
		var r, u, s int
		for _, b := range composition {
			if b.Kind == "shared" {
				continue
			}
			r += b.Remain
			u += b.Used
			s += b.Size
		}
		if r != 0 || s != 0 {
			creds.Remain, creds.Used, creds.Size = r, u, s
			extra := 0
			if shared != nil && shared.Available {
				extra = shared.Remain
			}
			creds.RemainEffective = r + extra
		}
	}
	return &CreditResult{OK: true, Credits: creds}
}

func creditFromComposition(composition []Composition, shared *SharedQuota, url string) *CreditResult {
	var r, u, s int
	for _, b := range composition {
		if b.Kind == "shared" {
			continue
		}
		r += b.Remain
		u += b.Used
		s += b.Size
	}
	extra := 0
	if shared != nil && shared.Available && !shared.Unlimited {
		extra = shared.Remain
	}
	return &CreditResult{OK: true, Credits: &Credits{
		Remain: r, RemainEffective: r + extra, Used: u, Size: s,
		Shared: shared, Composition: composition, Source: url,
		UpdatedAt:  time.Now().Unix(),
		UpdatedISO: time.Now().Format("2006-01-02 15:04:05"),
	}}
}

// FetchPlan reads the account's plan name.
func (c *Client) FetchPlan(ctx context.Context, cred Credential) string {
	cred, err := requireCredential("fetch plan", cred)
	if err != nil {
		return ""
	}
	cfg, cfgErr := RealmConfigFor(cred.Realm, cred.VPCOverride)
	if cfgErr != nil {
		return ""
	}
	data, doErr := c.http.DoJSON(ctx, Request{
		Method: "GET", URL: primaryOpenAPI(cfg) + PathPlan,
		Headers: c.AccountHeaders(cfg, cred), Timeout: 20 * time.Second,
	})
	if doErr != nil {
		return ""
	}
	return firstNonEmpty(asString(data["plan"]), asString(data["name"]),
		asString(data["planName"]))
}

// CreditExpiry is the human-facing answer to "when do these credits expire?".
//
// It is derived, not guessed: a snapshot without an expiry returns
// Known=false rather than a zero date.
type CreditExpiry struct {
	Known     bool   `json:"known"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	ISO       string `json:"iso,omitempty"`
	DaysLeft  int    `json:"days_left,omitempty"`
	// Source names which container supplied the date (plan / add-on / shared).
	Source string `json:"source,omitempty"`
}

// ExpiryOf reports when a snapshot's credits expire.
//
// The top-level expiresAt wins when present; otherwise the earliest expiring
// non-shared container is used, which is what an operator needs to know
// ("something runs out on the 3rd"). Containers without a date are ignored.
func ExpiryOf(c *Credits, now time.Time) CreditExpiry {
	if c == nil {
		return CreditExpiry{}
	}
	if c.ExpiresAt > 0 {
		return CreditExpiry{
			Known: true, ExpiresAt: c.ExpiresAt,
			ISO:      time.Unix(c.ExpiresAt, 0).In(UTC8).Format("2006-01-02 15:04"),
			DaysLeft: c.ExpiresInDays(now), Source: "top-level",
		}
	}
	var best *Composition
	for i := range c.Composition {
		b := &c.Composition[i]
		if b.Kind == "shared" || b.ExpiresAt <= 0 {
			continue
		}
		if best == nil || b.ExpiresAt < best.ExpiresAt {
			best = b
		}
	}
	if best == nil {
		return CreditExpiry{}
	}
	days := int(time.Until(time.Unix(best.ExpiresAt, 0)).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return CreditExpiry{
		Known: true, ExpiresAt: best.ExpiresAt,
		ISO:      time.Unix(best.ExpiresAt, 0).In(UTC8).Format("2006-01-02 15:04"),
		DaysLeft: days, Source: best.Kind,
	}
}
