// Package catalog holds the official per-realm model catalog.
//
// The data comes from the official Qoder client's model directory cache and is
// kept field-for-field (promotion peak/off-peak pricing, context_config
// windows, thinking_config effort tiers, is_free/is_new/icon, strategies).
//
// Runtime source priority for /v1/models:
//
//  1. the official live endpoint /algo/api/v2/model/list (needs an account)
//  2. the on-disk cache accounts/models-cache-<realm>.json (7 days)
//  3. the local official client's ~/.qoder*/.models/<uid>/catalog-v7 cache
//  4. the bundled snapshots assets/qoder_catalog_{cn,intl}.json
//
// This package owns 4 and provides the alias/display-name resolution every
// other layer uses. The live fetch lives in the upstream package, which feeds
// RegisterLiveDisplayNames so a renamed model still resolves during the window
// where the snapshot has not caught up.
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed snapshots/*.json
var snapshotFS embed.FS

// Realm names.
const (
	RealmCN   = "cn"
	RealmINTL = "intl"
)

// Model is one catalog entry. Only the fields the gateway reads are typed; the
// rest is preserved in Raw so /v1/models can pass the official payload through
// untouched ("never invent a field").
type Model struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         *bool   `json:"enable"`
	IsVL           bool    `json:"is_vl"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsDefault      bool    `json:"is_default"`
	IsFree         bool    `json:"is_free"`
	IsNew          bool    `json:"is_new"`
	Icon           string  `json:"icon"`
	PriceFactor    float64 `json:"price_factor"`
	OriginalPrice  float64 `json:"original_price_factor"`
	MaxInputTokens int64   `json:"max_input_tokens"`

	ContextConfig  map[string]ContextWindow `json:"context_config"`
	ThinkingConfig *ThinkingConfig          `json:"thinking_config"`
	Promotion      *Promotion               `json:"promotion"`
	Strategies     []map[string]any         `json:"strategies"`

	// Raw keeps every field exactly as the official payload had it.
	Raw map[string]any `json:"-"`
}

// ContextWindow is one selectable context tier.
type ContextWindow struct {
	TokenCount int64 `json:"token_count"`
	IsDefault  bool  `json:"is_default"`
}

// ThinkingConfig describes the reasoning effort tiers.
type ThinkingConfig struct {
	Disabled map[string]any  `json:"disabled"`
	Enabled  *ThinkingEnable `json:"enabled"`
}

// ThinkingEnable is the enabled branch of ThinkingConfig.
type ThinkingEnable struct {
	Description string                  `json:"description"`
	Efforts     map[string]EffortConfig `json:"efforts"`
	IsDefault   bool                    `json:"is_default"`
}

// EffortConfig marks the model's default effort.
type EffortConfig struct {
	IsDefault bool `json:"is_default"`
}

// Promotion is the official off-peak pricing metadata.
type Promotion struct {
	Active               bool              `json:"active"`
	Badge                map[string]string `json:"badge"`
	Description          map[string]string `json:"description"`
	Timezone             string            `json:"timezone"`
	RuleID               string            `json:"rule_id"`
	DiscountFactor       float64           `json:"discount_factor"`
	BeforePromotionPrice float64           `json:"before_promotion_price_factor"`
	LinkURL              map[string]string `json:"link_url"`
	WindowStart          string            `json:"window_start"`
	WindowEnd            string            `json:"window_end"`
}

// EffortOrder is the canonical ordering of reasoning tiers.
var EffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// EffortRank maps an effort name to a comparable integer.
var EffortRank = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// OfficialDisabledReason is what the official desktop shows for models the
// current plan does not include.
const OfficialDisabledReason = "需要升级或购买千问官方套餐开放"

// Aliases maps a client-friendly name to an upstream model key.
var Aliases = map[string]string{
	"auto":                "auto",
	"qoder-auto":          "auto",
	"qwen3.8-max":         "qmodel_38max",
	"qwen3.8-max-preview": "qmodel_38max",
	"qmodel_preview":      "qmodel_38max",
	"qwen3.8-flash":       "qfmodel",
	"qwen3.7-max":         "qmodel_latest",
	"qwen3.7-plus":        "qmodel",
	"qwen3.7-flash":       "q37fmodel",
	"qwen3.6-flash":       "q36fmodel",
	"deepseek-v4-pro":     "dmodel",
	"deepseek-v4-flash":   "dfmodel",
	"deepseek-flash":      "dfmodel",
	"glm-5.3":             "gmodel",
	"glm-5.3-flash":       "gfmodel",
	"glm-5.2":             "gm51model",
	"kimi-k3":             "kmodel_latest",
	"kimi-k3.1-pro":       "kmodel_latest",
	"kimi-k2.8-preview":   "kmodel",
	"kimi-k2.7":           "kmodel",
	"kimi-k2.7-code":      "kmodel",
	"minimax-m3":          "mmodel",
	"minimax-m2.7":        "mmodel",
	"minimax-m2.5":        "mmodel",
}

// Realm exclusivity tables (the official per-realm catalog difference).
var (
	INTLExclusive         = map[string]bool{"efficient": true, "cmodel": true, "smodel": true, "ultimate": true, "performance": true}
	CNExclusive           = map[string]bool{"q37fmodel": true, "gm51model": true}
	INTLExclusivePrefixes = []string{"ultimate", "performance", "efficient", "smodel", "cmodel"}
	CNExclusivePrefixes   = []string{"q37fmodel", "gm51model"}
)

// Official per-key text, aligned with the current Qoder documentation.
// Labels double as the localized display name the official client shows.
var officialDescriptions = map[string]string{
	"auto":          "智能选择最适合的模型，平衡性能与成本",
	"lite":          "基础推理能力，免费使用（高峰期可能响应较慢）",
	"cmodel":        "尝鲜体验全球顶级模型，擅长超长自主任务执行",
	"dmodel":        "复杂推理、代码生成与工程任务表现卓越",
	"gmodel":        "智谱开源旗舰模型，代码能力对标国际顶尖水平，兼具安全审查与漏洞推理能力",
	"kmodel":        "专为长上下文编程打造：精准遵循指令，可靠执行长链路任务",
	"mmodel":        "原生多模态感知、前沿编码能力，以及 1M 上下文深度，适合高强度工作流",
	"qmodel":        "推理能力、效率与多模态体验的全面跃迁",
	"dfmodel":       "推理速度快且成本低廉，综合能力均衡",
	"gfmodel":       "智谱全新原生多模态模型，深度理解图像与视频，自主研究分析、文档制作等复杂任务",
	"qfmodel":       "千问开源权重的多模态 MoE 模型，在能力、延迟与成本间取得出色平衡",
	"ultimate":      "专家级深度推理与思考能力，极致输出质量。",
	"efficient":     "标准推理能力，高性价比",
	"performance":   "高级推理能力，高质量输出",
	"qmodel_38max":  "Qwen 最新基座模型，拥有 2.4T 参数，在软件工程、办公与深度推理方面表现领先",
	"kmodel_latest": "Kimi 最强模型，拥有 2.8T 参数，专为软件工程、知识工作与深度推理打造",
	"qmodel_latest": "Agentic 能力的全面跃迁，可自主完成长程复杂任务",
}

var officialLabels = map[string]string{
	"auto": "Auto", "lite": "轻量", "cmodel": "Cantus",
	"dmodel": "DeepSeek-V4-Pro", "gmodel": "GLM-5.3",
	"kmodel": "Kimi-K2.8-Preview", "mmodel": "MiniMax-M3",
	"qmodel": "Qwen3.7-Plus", "dfmodel": "DeepSeek-V4-Flash",
	"gfmodel": "GLM-5.3-Flash", "qfmodel": "Qwen3.8-Flash",
	"ultimate": "极致", "efficient": "经济", "performance": "性能",
	"qmodel_38max": "Qwen3.8-Max", "kmodel_latest": "Kimi-K3",
	"qmodel_latest": "Qwen3.7-Max",
}

// Registry is the loaded catalog plus the dynamic display-name index.
type Registry struct {
	mu          sync.RWMutex
	cn          []map[string]any
	intl        []map[string]any
	liveDisplay map[string]string
	snapMissing []string
	warningSink func(string)
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
)

// Default returns the process-wide registry, loading the embedded snapshots.
func Default() *Registry {
	defaultOnce.Do(func() {
		defaultReg = &Registry{
			cn:          loadSnapshot("cn"),
			intl:        loadSnapshot("intl"),
			liveDisplay: map[string]string{},
		}
	})
	return defaultReg
}

// SetWarningSink routes snapshot-degradation warnings (nil disables them).
func (r *Registry) SetWarningSink(fn func(string)) {
	r.mu.Lock()
	r.warningSink = fn
	r.mu.Unlock()
}

func (r *Registry) warn(format string, args ...any) {
	r.mu.RLock()
	sink := r.warningSink
	r.mu.RUnlock()
	if sink != nil {
		sink(fmt.Sprintf(format, args...))
	}
}

func loadSnapshot(realm string) []map[string]any {
	name := "qoder_catalog_cn.json"
	if realm == RealmINTL {
		name = "qoder_catalog_intl.json"
	}
	var lastErr error
	// Prefer a user-provided file next to the binary (refreshed by the client).
	for _, dir := range []string{"assets", "data", "."} {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		if rows, err := parseSnapshot(raw); err == nil {
			return rows
		} else {
			lastErr = err
		}
	}
	raw, err := snapshotFS.ReadFile("snapshots/" + name)
	if err != nil {
		panic(fmt.Sprintf("catalog: cannot load %s: %v (last file error: %v)", name, err, lastErr))
	}
	rows, err := parseSnapshot(raw)
	if err != nil {
		panic(fmt.Sprintf("catalog: embedded %s is invalid: %v", name, err))
	}
	return rows
}

func parseSnapshot(raw []byte) ([]map[string]any, error) {
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("snapshot is empty")
	}
	return rows, nil
}

// Reload re-reads the snapshots from disk (falls back to the embedded copy).
func (r *Registry) Reload() {
	cn := loadSnapshot(RealmCN)
	intl := loadSnapshot(RealmINTL)
	r.mu.Lock()
	r.cn, r.intl = cn, intl
	r.mu.Unlock()
}

// ModelsForRealm returns the realm's catalog rows (official order, all fields).
func (r *Registry) ModelsForRealm(realm string) []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if realm == RealmINTL {
		return r.intl
	}
	if realm != RealmCN && realm != RealmINTL {
		return nil
	}
	return r.cn
}

// AllModels returns CN + INTL rows.
func (r *Registry) AllModels() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.cn)+len(r.intl))
	out = append(out, r.cn...)
	out = append(out, r.intl...)
	return out
}

// Model decodes one raw row into a typed Model.
func ModelInfo(raw map[string]any) Model {
	m := Model{Raw: raw}
	if raw == nil {
		return m
	}
	m.Key = asString(raw["key"])
	m.DisplayName = asString(raw["display_name"])
	if v, ok := raw["enable"].(bool); ok {
		m.Enable = &v
	}
	m.IsVL = asBool(raw["is_vl"])
	m.IsReasoning = asBool(raw["is_reasoning"])
	m.IsDefault = asBool(raw["is_default"])
	m.IsFree = asBool(raw["is_free"])
	m.IsNew = asBool(raw["is_new"])
	m.Icon = asString(raw["icon"])
	m.PriceFactor = asFloat(raw["price_factor"])
	m.OriginalPrice = asFloat(raw["original_price_factor"])
	m.MaxInputTokens = asInt(raw["max_input_tokens"])

	if cfg, ok := raw["context_config"].(map[string]any); ok {
		m.ContextConfig = map[string]ContextWindow{}
		for label, v := range cfg {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			m.ContextConfig[label] = ContextWindow{
				TokenCount: asInt(vm["token_count"]),
				IsDefault:  asBool(vm["is_default"]),
			}
		}
	}
	if tc, ok := raw["thinking_config"].(map[string]any); ok {
		m.ThinkingConfig = &ThinkingConfig{}
		if d, ok := tc["disabled"].(map[string]any); ok {
			m.ThinkingConfig.Disabled = d
		}
		if en, ok := tc["enabled"].(map[string]any); ok {
			te := &ThinkingEnable{
				Description: asString(en["description"]),
				Efforts:     map[string]EffortConfig{},
				IsDefault:   asBool(en["is_default"]),
			}
			if efforts, ok := en["efforts"].(map[string]any); ok {
				for name, cfg := range efforts {
					cm, _ := cfg.(map[string]any)
					te.Efforts[name] = EffortConfig{IsDefault: asBool(cm["is_default"])}
				}
			}
			m.ThinkingConfig.Enabled = te
		}
	}
	if p, ok := raw["promotion"].(map[string]any); ok {
		m.Promotion = &Promotion{
			Active:               asBool(p["active"]),
			Badge:                asStringMap(p["badge"]),
			Description:          asStringMap(p["description"]),
			Timezone:             asString(p["timezone"]),
			RuleID:               asString(p["rule_id"]),
			DiscountFactor:       asFloat(p["discount_factor"]),
			BeforePromotionPrice: asFloat(p["before_promotion_price_factor"]),
			LinkURL:              asStringMap(p["link_url"]),
			WindowStart:          asString(p["window_start"]),
			WindowEnd:            asString(p["window_end"]),
		}
	}
	if strategies, ok := raw["strategies"].([]any); ok {
		for _, s := range strategies {
			if sm, ok := s.(map[string]any); ok {
				m.Strategies = append(m.Strategies, sm)
			}
		}
	}
	return m
}

// DisplayID renders the public id "key (Official Name)".
func DisplayID(raw map[string]any) string {
	key := asString(raw["key"])
	name := asString(raw["display_name"])
	if name != "" && name != key {
		return key + " (" + name + ")"
	}
	return key
}

// DialogModels picks the conversational entries: chat first, then app-only
// additions. Inline/assistant/quest/BYOK scenes are never unioned, and app
// metadata never overwrites a chat entry.
func DialogModels(payload map[string]any) []map[string]any {
	if payload == nil {
		return nil
	}
	rows := []map[string]any{}
	seen := map[string]bool{}
	for _, scene := range []string{"chat", "app"} {
		cands, ok := payload[scene].([]any)
		if !ok {
			continue
		}
		for _, c := range cands {
			entry, ok := c.(map[string]any)
			if !ok {
				continue
			}
			key := asString(entry["key"])
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, entry)
		}
	}
	return rows
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// RegisterLiveDisplayNames maps dynamic display names onto upstream keys.
// Later registrations win: the caller guarantees the dynamic endpoint is
// fresher than the on-disk cache and the snapshot.
func (r *Registry) RegisterLiveDisplayNames(pairs [][2]string) int {
	updated := 0
	r.mu.Lock()
	for _, p := range pairs {
		n := strings.ToLower(strings.TrimSpace(p[0]))
		k := strings.TrimSpace(p[1])
		if n == "" || k == "" {
			continue
		}
		if r.liveDisplay[n] != k {
			r.liveDisplay[n] = k
			updated++
		}
	}
	r.mu.Unlock()
	return updated
}

// LiveDisplayNames returns a copy of the dynamic display-name index.
func (r *Registry) LiveDisplayNames() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.liveDisplay))
	for k, v := range r.liveDisplay {
		out[k] = v
	}
	return out
}

func (r *Registry) liveLookup(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.liveDisplay[strings.ToLower(strings.TrimSpace(name))]
}

// displayIndex builds {"display name (lower)": key} from both realms.
func (r *Registry) displayIndex() map[string]string {
	idx := map[string]string{}
	for _, raw := range r.AllModels() {
		name := strings.ToLower(strings.TrimSpace(asString(raw["display_name"])))
		if name == "" {
			continue
		}
		if _, exists := idx[name]; !exists {
			idx[name] = asString(raw["key"])
		}
	}
	return idx
}

// ModelNamePrefixes are namespace prefixes a client may prepend to a model name.
//
// A client that aggregates several gateways (for example a Codex model catalog
// that namespaces every upstream) sends "qoder:Qwen3.8-Flash". Without this
// strip the name matches nothing, and an unresolvable name is forwarded upstream
// verbatim — the upstream then silently routes it as "auto", so the caller pays
// for a model it did not ask for and never learns about it.
var ModelNamePrefixes = []string{"qoder:", "qoder/", "qoder-"}

// StripModelPrefix removes a recognised namespace prefix from a model name.
// Returns the bare name and whether a prefix was removed.
func StripModelPrefix(model string) (string, bool) {
	m := strings.TrimSpace(model)
	for _, p := range ModelNamePrefixes {
		if len(m) > len(p) && strings.EqualFold(m[:len(p)], p) {
			return strings.TrimSpace(m[len(p):]), true
		}
	}
	return m, false
}

// ResolveUpstreamKey maps a client-supplied model name onto an upstream key.
//
// Accepted forms: the official key, a display id "key (Name)", a client
// alias, the official display name, the official localized label, and any of
// those with a namespace prefix ("qoder:Qwen3.8-Flash").
func (r *Registry) ResolveUpstreamKey(model, realm string) string {
	if strings.TrimSpace(model) == "" {
		return "auto"
	}
	m, _ := StripModelPrefix(model)

	var realmKeys map[string]bool
	rows := r.ModelsForRealm(realm)
	if rows != nil {
		realmKeys = make(map[string]bool, len(rows))
		for _, raw := range rows {
			realmKeys[asString(raw["key"])] = true
		}
	}

	// "qmodel_38max (Qwen3.8-Max)" form.
	mKey := m
	if strings.Contains(m, " (") && strings.HasSuffix(m, ")") {
		if head := strings.TrimSpace(m[:strings.LastIndex(m, " (")]); head != "" {
			mKey = head
		}
	}

	if realmKeys != nil && realmKeys[mKey] {
		return mKey
	}
	low := strings.ToLower(mKey)
	if v, ok := Aliases[mKey]; ok {
		return v
	}
	if v, ok := Aliases[low]; ok {
		return v
	}
	tail := low
	if i := strings.LastIndex(low, " ("); i >= 0 {
		tail = strings.TrimSuffix(low[i+2:], ")")
	}
	idx := r.displayIndex()
	if v, ok := idx[tail]; ok {
		return v
	}
	if v, ok := idx[low]; ok {
		return v
	}
	if v := r.liveLookup(tail); v != "" {
		return v
	}
	if v := r.liveLookup(low); v != "" {
		return v
	}
	// Localized labels (zh.label).
	if labelKey := labelToKey(tail); labelKey != "" {
		if realmKeys == nil || realmKeys[labelKey] || r.isKnownStaticKey(labelKey) {
			return labelKey
		}
	}
	// A key that exists in the other realm: return it so the exclusivity check
	// can reject the cross-realm request with a clear message.
	if realmKeys != nil && r.isKnownStaticKey(mKey) {
		return mKey
	}
	// "provider/model" suffix.
	if strings.Contains(low, "/") {
		part := low[strings.LastIndex(low, "/")+1:]
		if v, ok := Aliases[part]; ok {
			return v
		}
		if v, ok := idx[part]; ok {
			return v
		}
		if v := r.liveLookup(part); v != "" {
			return v
		}
	}
	return mKey
}

func labelToKey(label string) string {
	target := strings.ToLower(strings.TrimSpace(label))
	if target == "" {
		return ""
	}
	for key, lbl := range officialLabels {
		if strings.ToLower(lbl) == target {
			return key
		}
	}
	return ""
}

func (r *Registry) isKnownStaticKey(key string) bool {
	for _, raw := range r.AllModels() {
		if asString(raw["key"]) == key {
			return true
		}
	}
	return false
}

// IsKnownUpstreamKey reports whether key is a known upstream model key.
func (r *Registry) IsKnownUpstreamKey(key, realm string) bool {
	if key == "" {
		return false
	}
	for _, raw := range r.ModelsForRealm(realm) {
		if asString(raw["key"]) == key {
			return true
		}
	}
	if r.isKnownStaticKey(key) {
		return true
	}
	for _, v := range Aliases {
		if v == key {
			return true
		}
	}
	for _, v := range r.LiveDisplayNames() {
		if v == key {
			return true
		}
	}
	return false
}

// ExclusiveRealm returns "intl"/"cn" when only that exit serves the model.
func (r *Registry) ExclusiveRealm(model string) string {
	if model == "" {
		return ""
	}
	resolved := strings.ToLower(r.ResolveUpstreamKey(model, ""))
	low := strings.ToLower(model)
	for _, cand := range []string{resolved, low} {
		if INTLExclusive[cand] || hasAnyPrefix(cand, INTLExclusivePrefixes) {
			return RealmINTL
		}
		if CNExclusive[cand] || hasAnyPrefix(cand, CNExclusivePrefixes) {
			return RealmCN
		}
	}
	return ""
}

// KeyRealmSpan returns the realms whose catalog contains key.
func (r *Registry) KeyRealmSpan(key string) []string {
	var out []string
	for _, realm := range []string{RealmCN, RealmINTL} {
		for _, raw := range r.ModelsForRealm(realm) {
			if asString(raw["key"]) == key {
				out = append(out, realm)
				break
			}
		}
	}
	return out
}

// AutoRealmForModel implements step 1 of automatic realm selection: switch only
// when the model exists in exactly one realm. A model present in both cannot be
// distinguished by name, so the current realm stays put.
//
// Returns (realm, reason); reason is "" when no switch is warranted.
func (r *Registry) AutoRealmForModel(model, current string) (string, string) {
	if model == "" {
		return "", ""
	}
	key := r.ResolveUpstreamKey(model, current)
	if key == "" {
		return "", ""
	}
	inCN := r.realmHasKey(RealmCN, key)
	inINTL := r.realmHasKey(RealmINTL, key)
	switch {
	case inCN && !inINTL && current != RealmCN:
		return RealmCN, "model_missing"
	case inINTL && !inCN && current != RealmINTL:
		return RealmINTL, "model_missing"
	}
	return "", ""
}

func (r *Registry) realmHasKey(realm, key string) bool {
	for _, raw := range r.ModelsForRealm(realm) {
		if asString(raw["key"]) == key {
			return true
		}
	}
	return false
}

// OfficialDescription returns the official zh detail text for key.
func OfficialDescription(key string) string { return officialDescriptions[key] }

// OfficialLabel returns the official localized label for key.
func OfficialLabel(key string) string { return officialLabels[key] }

// SupportedEfforts returns the model's effort tiers in canonical order.
func SupportedEfforts(meta Model) []string {
	if meta.ThinkingConfig == nil || meta.ThinkingConfig.Enabled == nil {
		return nil
	}
	present := meta.ThinkingConfig.Enabled.Efforts
	out := make([]string, 0, len(present))
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		if _, ok := present[e]; ok {
			out = append(out, e)
		}
	}
	return out
}

// DefaultEffort returns the model's default tier (is_default), else the middle
// of the allowed list.
func DefaultEffort(meta Model) string {
	if meta.ThinkingConfig != nil && meta.ThinkingConfig.Enabled != nil {
		for name, cfg := range meta.ThinkingConfig.Enabled.Efforts {
			if cfg.IsDefault {
				if _, ok := EffortRank[strings.ToLower(name)]; ok {
					return strings.ToLower(name)
				}
			}
		}
	}
	allowed := SupportedEfforts(meta)
	if len(allowed) == 0 {
		return ""
	}
	return allowed[len(allowed)/2]
}

// NormalizeEffort clamps a requested effort to what the model supports.
//
// Clamping is nearest-neighbour in both directions, breaking ties toward the
// model's default tier. Clamping only downward would silently downgrade
// "medium"/"high" on a model that offers low/high/max.
//
// Returns (normalized, note); note is non-empty when a change was made.
func NormalizeEffort(effort string, meta Model) (string, string) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return "", ""
	}
	allowed := SupportedEfforts(meta)
	if len(allowed) == 0 {
		if _, ok := EffortRank[e]; ok {
			return "", "model has no effort tiers; dropped"
		}
		return "", fmt.Sprintf("unknown effort %q; dropped", effort)
	}
	if !contains(allowed, e) {
		if _, ok := EffortRank[e]; !ok {
			return "", fmt.Sprintf("unknown effort %q; dropped", effort)
		}
		best := allowed[0]
		bestScore := -1
		want := EffortRank[e]
		ref, hasRef := EffortRank[DefaultEffort(meta)]
		if !hasRef {
			ref = want
		}
		for _, cand := range allowed {
			d1 := abs(EffortRank[cand] - want)
			d2 := abs(EffortRank[cand] - ref)
			score := d1*1000 + d2
			if bestScore < 0 || score < bestScore {
				bestScore = score
				best = cand
			}
		}
		return best, fmt.Sprintf("effort %q clamped to %q", e, best)
	}
	return e, ""
}

// OffPeakActiveNow reports whether the promotion window contains now.
func OffPeakActiveNow(windowStart, windowEnd, tz string, nowMinutes int) bool {
	start, ok1 := parseHHMM(windowStart)
	end, ok2 := parseHHMM(windowEnd)
	if !ok1 || !ok2 {
		return false
	}
	if start == end {
		return false
	}
	if start < end {
		return nowMinutes >= start && nowMinutes < end
	}
	// Window wraps midnight (22:00-08:00).
	return nowMinutes >= start || nowMinutes < end
}

func parseHHMM(v string) (int, bool) {
	v = strings.TrimSpace(v)
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// SortedKeys returns map keys in ascending order (used by API responses).
func SortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

func asStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = asString(val)
	}
	return out
}
