package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CreditSource records which (host, path) pair produced a parseable quota body.
type CreditSource struct {
	Host   string   `json:"host"`
	Path   string   `json:"path"`
	Fields []string `json:"fields"`
	At     float64  `json:"at"`
}

// CreditsSourcePath returns accounts/credits-source.json.
func CreditsSourcePath(accountsDir string) string {
	return filepath.Join(accountsDir, CreditSourceFile)
}

// LoadCreditSources reads the per-scope endpoint discovery record.
func LoadCreditSources(accountsDir string) map[string]CreditSource {
	out := map[string]CreditSource{}
	raw, err := os.ReadFile(CreditsSourcePath(accountsDir))
	if err != nil {
		return out
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	for scope, body := range doc {
		var src CreditSource
		if err := json.Unmarshal(body, &src); err == nil && src.Host != "" {
			out[scope] = src
		}
	}
	return out
}

// SaveCreditSource remembers a working (host, path) pair for one scope.
func SaveCreditSource(accountsDir, scope, host, path string, fields []string) {
	doc := map[string]any{}
	if raw, err := os.ReadFile(CreditsSourcePath(accountsDir)); err == nil {
		_ = json.Unmarshal(raw, &doc)
	}
	doc[scope] = map[string]any{
		"host": host, "path": path, "fields": fields,
		"at": float64(time.Now().Unix()),
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(accountsDir, 0o755)
	tmp := CreditsSourcePath(accountsDir) + ".tmp"
	if os.WriteFile(tmp, out, 0o600) == nil {
		_ = os.Rename(tmp, CreditsSourcePath(accountsDir))
	}
}

// bucketFrom normalises one upstream quota container.
func bucketFrom(src any, kind, name string) *Composition {
	switch v := src.(type) {
	case float64:
		return &Composition{Name: name, Kind: kind, Remain: int(v), Size: int(v)}
	case map[string]any:
		remain := pickNum(v, "remaining", "remain", "balance")
		used := pickNum(v, "used", "consumed")
		size := pickNum(v, "total", "cap", "size")
		comp := &Composition{
			Name:   name,
			Kind:   kind,
			Remain: int(remain),
			Used:   int(used),
			Size:   int(size),
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

// QuotaContainers lists the recognised container names per category.
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
				// cap = -1 is the "no cap" sentinel, not a real amount; the
				// remaining field is usually a placeholder.
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

// QuotaFetchResult is the outcome of one credits refresh.
type QuotaFetchResult struct {
	OK      bool     `json:"ok"`
	HTTP    int      `json:"http,omitempty"`
	Error   string   `json:"error,omitempty"`
	Credits *Credits `json:"credits,omitempty"`
}

// FetchCredits GETs the quota endpoint and updates the account's snapshot.
//
// INTL frequently 404/403s on /api/v2/quota/usage, so a previously discovered
// (host, path) pair is tried first; on failure the candidate matrix is probed
// once and the hit is remembered for six hours.
func (a *Account) FetchCredits(accountsDir string) *QuotaFetchResult {
	cfg := a.RealmConfig()
	scope := a.EndpointScope()

	if accountsDir != "" {
		if src, ok := LoadCreditSources(accountsDir)[scope]; ok {
			if res := a.fetchCreditsVia(cfg, src.Host, src.Path); res != nil && res.OK {
				return res
			}
		}
	}

	url := cfg.OpenAPI + PathQuota
	headers := a.AccountHeaders()
	data, err := DefaultClient.DoJSON(Request{Method: "GET", URL: url, Headers: headers, Timeout: 30 * time.Second})
	if err != nil {
		if he, ok := err.(*HTTPStatusError); ok && (he.Code == 401 || he.Code == 403) {
			// A 403 here is usually WAF/egress blocking or a realm-mismatched
			// token. Retry once with the desktop header set, then surface an
			// actionable message instead of a bare 403.
			if d2, err2 := DefaultClient.DoJSON(Request{
				Method: "GET", URL: url, Headers: a.DesktopHeaders(), Timeout: 30 * time.Second,
			}); err2 == nil {
				return a.applyQuota(nil, d2, url, accountsDir, scope)
			}
			if accountsDir != "" {
				if probe := a.ProbeCreditEndpoints(accountsDir, 6); probe != nil && probe.OK {
					return probe
				}
			}
			return &QuotaFetchResult{OK: false, HTTP: he.Code,
				Error: fmt.Sprintf("HTTP %d %s", he.Code, truncate(err2String(err), 160))}
		}
		if accountsDir != "" {
			if probe := a.ProbeCreditEndpoints(accountsDir, 6); probe != nil && probe.OK {
				return probe
			}
		}
		if he, ok := err.(*HTTPStatusError); ok {
			return &QuotaFetchResult{OK: false, HTTP: he.Code, Error: err.Error()}
		}
		return &QuotaFetchResult{OK: false, Error: err.Error()}
	}
	return a.applyQuota(nil, data, url, accountsDir, scope)
}

func err2String(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (a *Account) fetchCreditsVia(cfg RealmConfig, host, path string) *QuotaFetchResult {
	full := strings.TrimRight(firstNonEmpty(host, cfg.OpenAPI), "/") + path
	data, err := DefaultClient.DoJSON(Request{
		Method: "GET", URL: full, Headers: a.AccountHeaders(), Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil
	}
	return a.applyQuota(nil, data, full, "", "")
}

// ProbeCreditEndpoints walks the candidate matrix and remembers the first hit.
func (a *Account) ProbeCreditEndpoints(accountsDir string, limit int) *QuotaFetchResult {
	cfg := a.RealmConfig()
	hosts := OpenAPICandidates(a.Realm, cfg)
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
			data, err := DefaultClient.DoJSON(Request{
				Method: "GET", URL: full, Headers: a.AccountHeaders(), Timeout: 15 * time.Second,
			})
			if err != nil {
				continue
			}
			composition, _ := NormalizeQuotaPayload(data)
			if len(composition) == 0 && !hasQuotaNumbers(data) {
				continue
			}
			fields := recognisedFields(data)
			SaveCreditSource(accountsDir, a.EndpointScope(), host, path, fields)
			return a.applyQuota(nil, data, full, accountsDir, a.EndpointScope())
		}
	}
	return nil
}

func hasQuotaNumbers(payload map[string]any) bool {
	for _, f := range append(append(append(planFields, addonFields...), sotaFields...), sharedFields...) {
		if v, ok := payload[f]; ok && v != nil {
			return true
		}
	}
	return false
}

func recognisedFields(payload map[string]any) []string {
	var out []string
	for _, f := range append(append(append(planFields, addonFields...), sotaFields...), sharedFields...) {
		if _, ok := payload[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

// applyQuota normalises the payload and stores the snapshot.
func (a *Account) applyQuota(_ []byte, q map[string]any, url, accountsDir, scope string) *QuotaFetchResult {
	if q == nil {
		return &QuotaFetchResult{OK: false, Error: "empty quota response"}
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
		return a.storeComposition(composition, shared, url, accountsDir)
	}
	hasShared := false
	for _, b := range composition {
		if b.Kind == "shared" {
			hasShared = true
		}
	}
	degenerate := bothMissing || (asBool(q["isQuotaExceeded"]) && remain == 0 && size == 0 && !hasShared)
	if degenerate {
		reason := "both quota objects missing"
		if !bothMissing {
			reason = "exceeded=true with zero size/remain"
		}
		a.mu.Lock()
		a.LastQuotaAnomaly = map[string]any{
			"reason": reason,
			"at":     float64(time.Now().Unix()),
			"http":   200,
		}
		a.mu.Unlock()
		return &QuotaFetchResult{OK: false, Error: "degenerate quota body: " + reason}
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
	// When the composition parser recognises containers the old two-field sum
	// misses (SOTA etc.), its totals win — the panel would otherwise under-report.
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
	a.storeCredits(creds, accountsDir)
	return &QuotaFetchResult{OK: true, Credits: creds}
}

func (a *Account) storeComposition(composition []Composition, shared *SharedQuota, url, accountsDir string) *QuotaFetchResult {
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
	creds := &Credits{
		Remain: r, RemainEffective: r + extra, Used: u, Size: s,
		Shared: shared, Composition: composition, Source: url,
		UpdatedAt:  time.Now().Unix(),
		UpdatedISO: time.Now().Format("2006-01-02 15:04:05"),
	}
	a.storeCredits(creds, accountsDir)
	return &QuotaFetchResult{OK: true, Credits: creds}
}

func (a *Account) storeCredits(creds *Credits, accountsDir string) {
	a.mu.Lock()
	prevEff := 0
	hadPrev := false
	if a.Credits != nil {
		prevEff = a.Credits.RemainEffective
		hadPrev = true
	}
	a.Credits = creds
	// The panel shows the last credit delta; only update when the number really
	// changed so a +100 check-in is not overwritten by a later refresh.
	if hadPrev && prevEff != creds.RemainEffective {
		delta := creds.RemainEffective - prevEff
		a.LastCreditsDelta = &delta
		a.LastCreditsDeltaAt = float64(time.Now().Unix())
	}
	a.LastQuotaAnomaly = nil
	a.CreditsDegraded = nil
	a.mu.Unlock()
	// A balance refresh is the only path that lifts a 402 park.
	a.ReviveBalanceCooldown()
	if a.Path != "" {
		_ = a.Save(dirOf(a.Path))
	}
}

// FetchPlan updates the plan name from /api/v2/user/plan.
func (a *Account) FetchPlan() string {
	cfg := a.RealmConfig()
	data, err := DefaultClient.DoJSON(Request{
		Method: "GET", URL: cfg.OpenAPI + PathPlan,
		Headers: a.AccountHeaders(), Timeout: 20 * time.Second,
	})
	if err != nil {
		return ""
	}
	name := firstNonEmpty(asString(data["plan"]), asString(data["name"]),
		asString(data["planName"]))
	if name != "" {
		a.mu.Lock()
		a.Plan = name
		a.mu.Unlock()
		_ = a.Save(dirOf(a.Path))
	}
	return name
}
