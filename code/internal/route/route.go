// Package route implements the local routing workbench: pattern rules, account
// groups, key scoping and weighted tier selection.
//
// Two invariants: rules never rewrite API keys or models, and a rule can only
// narrow an API key's account scope, never widen it.
package route

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/settings"
)

// Strategies a rule may use.
var Strategies = []string{"round-robin", "weighted-round-robin", "fill-first"}

// DefaultPolicy is applied when a routing document has none.
var DefaultPolicy = Policy{
	Strategy:        "round-robin",
	SessionAffinity: true,
}

// Policy tunes account selection.
type Policy struct {
	Strategy         string `json:"strategy"`
	SessionAffinity  bool   `json:"session_affinity"`
	RequestRetry     int    `json:"request_retry"`
	MaxRetryAccounts int    `json:"max_retry_accounts"`
}

// Rule is one routing rule.
type Rule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Model    string   `json:"model"`
	Realm    string   `json:"realm"`
	Strategy string   `json:"strategy"`
	Groups   []string `json:"groups"`
	Accounts []string `json:"accounts"`
	KeyIDs   []string `json:"key_ids"`
	Priority int      `json:"priority"`
}

// Tier is one selection layer (preferred first).
type Tier struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Accounts []string       `json:"accounts"`
	Weights  map[string]int `json:"weights"`
}

// Config is the whole routing document.
type Config struct {
	Rules        []Rule `json:"rules"`
	Tiers        []Tier `json:"tiers"`
	Policy       Policy `json:"policy"`
	Configured   bool   `json:"configured"`
	BoardEnabled bool   `json:"board_enabled"`
	Revision     string `json:"revision"`
	UpdatedAt    int64  `json:"updated_at,omitempty"`
}

// Decision is the outcome of resolving a request against the rules.
type Decision struct {
	Matched     *Rule
	Realm       string
	Strategy    string
	AllowedUIDs map[string]bool
	Tiers       []Tier
	Policy      Policy
	Configured  bool
	ScopeID     string
}

// Manager loads, caches and saves the routing document.
//
// The cache is keyed on the file's stat tuple so an external edit invalidates it
// immediately — there is no TTL window in which a stale plan is served.
type Manager struct {
	mu        sync.Mutex
	dir       string
	cache     map[string]cachedConfig
	selectors map[string]map[string]int
	cursors   map[string]int
}

type cachedConfig struct {
	stamp  string
	result *Config
}

// NewManager binds a routing manager to the accounts directory.
func NewManager(accountsDir string) *Manager {
	return &Manager{
		dir:       accountsDir,
		cache:     map[string]cachedConfig{},
		selectors: map[string]map[string]int{},
		cursors:   map[string]int{},
	}
}

// Path returns accounts/admin/routing.json.
// The subdirectory deliberately avoids colliding with <uid>.json accounts.
func (m *Manager) Path() string {
	return filepath.Join(m.dir, "admin", "routing.json")
}

func statStamp(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d|%d|%d", fi.Size(), fi.ModTime().UnixNano(), fi.Mode()), true
}

// Load returns the parsed routing document, cached on its stat tuple.
func (m *Manager) Load() (*Config, error) {
	path := m.Path()
	stamp, ok := statStamp(path)
	if !ok {
		m.mu.Lock()
		delete(m.cache, path)
		m.mu.Unlock()
		return &Config{Policy: DefaultPolicy, Revision: "empty"}, nil
	}
	m.mu.Lock()
	if hit, exists := m.cache[path]; exists && hit.stamp == stamp {
		m.mu.Unlock()
		cp := *hit.result
		return &cp, nil
	}
	m.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return &Config{Policy: DefaultPolicy, Revision: "empty"}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("routing configuration invalid: %s", truncate(err.Error(), 120))
	}
	cfg, err := parseConfig(body, raw)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if len(m.cache) >= 16 {
		m.cache = map[string]cachedConfig{}
	}
	m.cache[path] = cachedConfig{stamp: stamp, result: cfg}
	m.mu.Unlock()
	cp := *cfg
	return &cp, nil
}

func parseConfig(body map[string]any, raw []byte) (*Config, error) {
	sum := sha256.Sum256(raw)
	cfg := &Config{Revision: hex.EncodeToString(sum[:])}
	rules, ok := body["rules"].([]any)
	if !ok && body["rules"] != nil {
		return nil, fmt.Errorf("routing configuration invalid: rules must be a list")
	}
	parsedRules, err := NormalizeRules(rules)
	if err != nil {
		return nil, err
	}
	cfg.Rules = parsedRules
	tiers, _ := body["tiers"].([]any)
	parsedTiers, err := NormalizeTiers(tiers)
	if err != nil {
		return nil, err
	}
	cfg.Tiers = parsedTiers
	policy, err := NormalizePolicy(body["policy"])
	if err != nil {
		return nil, err
	}
	cfg.Policy = policy
	if _, hasPolicy := body["policy"]; hasPolicy {
		cfg.Configured = true
	}
	if len(parsedTiers) > 0 {
		cfg.Configured = true
	}
	cfg.BoardEnabled = len(parsedTiers) > 0
	if v, ok := body["board_enabled"].(bool); ok {
		cfg.BoardEnabled = v
	}
	if v, ok := body["updated_at"]; ok && v != nil {
		if f, ok := v.(float64); ok {
			cfg.UpdatedAt = int64(f)
		}
	}
	return cfg, nil
}

func cleanStrings(value any, field string, allowEmpty bool) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of at most 256 strings", field)
	}
	if len(list) > 256 {
		return nil, fmt.Errorf("%s must be a list of at most 256 strings", field)
	}
	var out []string
	for _, item := range list {
		s, ok := item.(string)
		if !ok || len(s) > 160 {
			return nil, fmt.Errorf("%s contains an invalid value", field)
		}
		s = strings.TrimSpace(s)
		if (s != "" || allowEmpty) && !contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// NormalizeRules validates and sorts a rule list by priority.
func NormalizeRules(items []any) ([]Rule, error) {
	if len(items) > 128 {
		return nil, fmt.Errorf("rules must contain at most 128 entries")
	}
	var out []Rule
	ids := map[string]bool{}
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each routing rule must be an object")
		}
		id := strings.TrimSpace(asString(m["id"]))
		if id == "" {
			id = randomHex(16)
		}
		if len(id) > 80 {
			id = id[:80]
		}
		if ids[id] {
			return nil, fmt.Errorf("duplicate routing rule id")
		}
		ids[id] = true
		model := strings.TrimSpace(asString(m["model"]))
		if model == "" {
			model = "*"
		}
		if len(model) > 160 {
			return nil, fmt.Errorf("invalid model pattern")
		}
		realm := strings.ToLower(strings.TrimSpace(asString(m["realm"])))
		if realm != "" && realm != "cn" && realm != "intl" {
			return nil, fmt.Errorf("realm must be cn, intl or empty")
		}
		strategy := asString(m["strategy"])
		if strategy == "" {
			strategy = "round-robin"
		}
		if !contains(Strategies, strategy) {
			return nil, fmt.Errorf("unsupported routing strategy")
		}
		priority := idx
		if v, ok := m["priority"]; ok && v != nil {
			priority = int(asInt(v))
		}
		if priority < -10000 {
			priority = -10000
		}
		if priority > 10000 {
			priority = 10000
		}
		name := strings.TrimSpace(asString(m["name"]))
		if name == "" {
			name = "未命名规则"
		}
		if len(name) > 100 {
			name = name[:100]
		}
		groups, err := cleanStrings(m["groups"], "groups", true)
		if err != nil {
			return nil, err
		}
		accounts, err := cleanStrings(m["accounts"], "accounts", false)
		if err != nil {
			return nil, err
		}
		keyIDs, err := cleanStrings(m["key_ids"], "key_ids", false)
		if err != nil {
			return nil, err
		}
		enabled := true
		if v, ok := m["enabled"].(bool); ok {
			enabled = v
		}
		out = append(out, Rule{
			ID: id, Name: name, Enabled: enabled, Model: model, Realm: realm,
			Strategy: strategy, Groups: groups, Accounts: accounts,
			KeyIDs: keyIDs, Priority: priority,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}

// NormalizeTiers validates the layered selection config.
func NormalizeTiers(items []any) ([]Tier, error) {
	if len(items) > 64 {
		return nil, fmt.Errorf("tiers must be a list of at most 64 layers")
	}
	var out []Tier
	seenAccounts := map[string]bool{}
	tierIDs := map[string]bool{}
	for idx, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each tier must be an object")
		}
		id := strings.TrimSpace(asString(m["id"]))
		if id == "" {
			id = randomHex(16)
		}
		if tierIDs[id] {
			return nil, fmt.Errorf("duplicate tier id")
		}
		tierIDs[id] = true
		accounts, err := cleanStrings(m["accounts"], "accounts", false)
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			if seenAccounts[a] {
				return nil, fmt.Errorf("an account may belong to only one tier")
			}
			seenAccounts[a] = true
		}
		weights := map[string]int{}
		if wm, ok := m["weights"].(map[string]any); ok {
			for account, v := range wm {
				w := int(asInt(v))
				if w < 0 {
					w = 0
				}
				if w > 1000000 {
					w = 1000000
				}
				weights[account] = w
			}
		}
		for _, a := range accounts {
			if _, ok := weights[a]; !ok {
				weights[a] = 1
			}
		}
		name := strings.TrimSpace(asString(m["name"]))
		if name == "" {
			if idx == 0 {
				name = "首选"
			} else {
				name = "备用层"
			}
		}
		if len(name) > 80 {
			name = name[:80]
		}
		out = append(out, Tier{ID: id, Name: name, Accounts: accounts, Weights: weights})
	}
	return out, nil
}

// NormalizePolicy validates the selection policy.
func NormalizePolicy(value any) (Policy, error) {
	if value == nil {
		return DefaultPolicy, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return Policy{}, fmt.Errorf("policy must be an object")
	}
	strategy := "round-robin"
	if s := asString(m["strategy"]); s != "" {
		strategy = s
	}
	if !contains(Strategies, strategy) {
		return Policy{}, fmt.Errorf("unsupported routing strategy")
	}
	affinity := true
	if v, ok := m["session_affinity"].(bool); ok {
		affinity = v
	}
	out := Policy{Strategy: strategy, SessionAffinity: affinity}
	for name, maximum := range map[string]int{"request_retry": 3, "max_retry_accounts": 256} {
		v := 0
		if raw, ok := m[name]; ok && raw != nil {
			v = int(asInt(raw))
		}
		if v < 0 || v > maximum {
			return Policy{}, fmt.Errorf("%s is outside supported range", name)
		}
		if name == "request_retry" {
			out.RequestRetry = v
		} else {
			out.MaxRetryAccounts = v
		}
	}
	return out, nil
}

// Save persists the routing document with optional optimistic concurrency.
//
// Load and the file write take the lock themselves, so Save must not hold it
// across them (a re-entrant Load would deadlock).
func (m *Manager) Save(rules []Rule, tiers []Tier, policy *Policy, expectedRevision string) (*Config, error) {
	old, err := m.Load()
	if err != nil {
		return nil, err
	}
	if expectedRevision != "" && expectedRevision != old.Revision {
		return nil, fmt.Errorf("routing_conflict: 配置已被其他窗口修改，请重新加载后再保存")
	}
	document := map[string]any{"rules": old.Rules, "updated_at": time.Now().Unix()}
	if rules != nil {
		document["rules"] = rules
	}
	if policy != nil || old.Configured {
		document["policy"] = old.Policy
		if policy != nil {
			document["policy"] = *policy
		}
		document["tiers"] = old.Tiers
		if tiers != nil {
			document["tiers"] = tiers
		}
		document["board_enabled"] = old.BoardEnabled
	}
	path := m.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	delete(m.cache, path)
	return m.Load()
}

// ScopeUIDs resolves explicit ids plus group membership into a uid set.
// nil means "unrestricted".
func ScopeUIDs(accounts []AccountLike, ids, groups []string) map[string]bool {
	if len(ids) == 0 && len(groups) == 0 {
		return nil
	}
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}
	groupSet := map[string]bool{}
	for _, g := range groups {
		groupSet[g] = true
	}
	out := map[string]bool{}
	for _, a := range accounts {
		if idSet[a.UID()] || groupSet[a.Group()] {
			out[a.UID()] = true
		}
	}
	return out
}

// AccountLike is the minimal account surface routing needs.
type AccountLike interface {
	UID() string
	Group() string
	RealmOf() string
}

// Resolve applies the rules to one request.
func (m *Manager) Resolve(accounts []AccountLike, model string, key *settings.KeyEntry,
	explicitRealm string) (*Decision, error) {
	cfg, err := m.Load()
	if err != nil {
		return nil, err
	}
	keyRealm := ""
	keyID := ""
	if key != nil {
		keyRealm = key.Realm
		keyID = key.ID
	}
	if keyRealm != "" && explicitRealm != "" && keyRealm != explicitRealm {
		return nil, fmt.Errorf("API Key realm conflicts with requested realm")
	}
	forced := keyRealm
	if forced == "" {
		forced = explicitRealm
	}
	var allowed map[string]bool
	if key != nil {
		allowed = ScopeUIDs(accounts, key.AccountIDs, key.AccountGroups)
	}

	var matched *Rule
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		if !rule.Enabled || !fnmatch(strings.ToLower(model), strings.ToLower(rule.Model)) {
			continue
		}
		if len(rule.KeyIDs) > 0 && !contains(rule.KeyIDs, keyID) {
			continue
		}
		if forced != "" && rule.Realm != "" && forced != rule.Realm {
			continue
		}
		matched = rule
		scope := ScopeUIDs(accounts, rule.Accounts, rule.Groups)
		if scope != nil {
			if allowed == nil {
				allowed = scope
			} else {
				intersect := map[string]bool{}
				for uid := range allowed {
					if scope[uid] {
						intersect[uid] = true
					}
				}
				allowed = intersect
			}
		}
		break
	}

	realm := forced
	if realm == "" && matched != nil {
		realm = matched.Realm
	}
	if realm != "" {
		within := map[string]bool{}
		for _, a := range accounts {
			if a.RealmOf() == realm {
				within[a.UID()] = true
			}
		}
		if allowed == nil {
			allowed = within
		} else {
			intersect := map[string]bool{}
			for uid := range allowed {
				if within[uid] {
					intersect[uid] = true
				}
			}
			allowed = intersect
		}
	}
	if len(cfg.Tiers) > 0 || cfg.BoardEnabled {
		tierScope := map[string]bool{}
		for _, t := range cfg.Tiers {
			for _, uid := range t.Accounts {
				tierScope[uid] = true
			}
		}
		if allowed == nil {
			allowed = tierScope
		} else {
			intersect := map[string]bool{}
			for uid := range allowed {
				if tierScope[uid] {
					intersect[uid] = true
				}
			}
			allowed = intersect
		}
	}
	strategy := cfg.Policy.Strategy
	if len(cfg.Tiers) == 0 && !cfg.Configured && matched != nil && matched.Strategy != "" {
		strategy = matched.Strategy
	}
	return &Decision{
		Matched: matched, Realm: realm, Strategy: strategy,
		AllowedUIDs: allowed, Tiers: cfg.Tiers, Policy: cfg.Policy,
		Configured: cfg.Configured,
		ScopeID:    m.dir + ":" + keyID + ":" + ruleID(matched),
	}, nil
}

func ruleID(r *Rule) string {
	if r == nil {
		return ""
	}
	return r.ID
}

// Candidate is one selectable account.
type Candidate interface {
	AccountLike
	IsReady(model string) bool
	IsDegraded() bool
}

// AsCandidates widens a typed candidate slice into the interface slice Pick and
// Candidates accept.
func AsCandidates[T Candidate](items []T) []Candidate {
	out := make([]Candidate, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

// Candidates returns the ready accounts for a tier, in tier order.
func (m *Manager) Candidates(decision *Decision, realm, model string, pool []Candidate,
	exclude map[string]bool) ([]Candidate, *Tier) {
	var ready []Candidate
	for _, a := range pool {
		if realm != "" && a.RealmOf() != realm {
			continue
		}
		if exclude != nil && exclude[a.UID()] {
			continue
		}
		if decision.AllowedUIDs != nil && !decision.AllowedUIDs[a.UID()] {
			continue
		}
		if !a.IsReady(model) {
			continue
		}
		ready = append(ready, a)
	}
	if len(decision.Tiers) == 0 {
		return ready, nil
	}
	for i := range decision.Tiers {
		tier := &decision.Tiers[i]
		var rows []Candidate
		for _, a := range ready {
			if !contains(tier.Accounts, a.UID()) {
				continue
			}
			if decision.Strategy == "weighted-round-robin" && tier.Weights[a.UID()] <= 0 {
				continue
			}
			rows = append(rows, a)
		}
		if len(rows) > 0 {
			return rows, tier
		}
	}
	return nil, nil
}

// Pick selects one account according to the decision's strategy.
func (m *Manager) Pick(decision *Decision, realm, model string, pool []Candidate,
	sessionKey string, exclude map[string]bool) Candidate {
	rows, tier := m.Candidates(decision, realm, model, pool, exclude)
	if len(rows) == 0 {
		return nil
	}
	switch decision.Strategy {
	case "fill-first":
		return rows[0]
	case "weighted-round-robin":
		return m.weightedPick(decision, realm, model, rows, tier)
	default:
		return m.roundRobinPick(decision, realm, model, rows, tier)
	}
}

func (m *Manager) roundRobinPick(decision *Decision, realm, model string, rows []Candidate, tier *Tier) Candidate {
	key := fmt.Sprintf("%s|%s|%s|%s|rr", decision.ScopeID, realm, model, tierKey(tier))
	m.mu.Lock()
	cursor := m.cursors[key]
	m.cursors[key] = cursor + 1
	if len(m.cursors) > 512 {
		m.cursors = map[string]int{}
	}
	m.mu.Unlock()
	return rows[cursor%len(rows)]
}

func (m *Manager) weightedPick(decision *Decision, realm, model string, rows []Candidate, tier *Tier) Candidate {
	key := fmt.Sprintf("%s|%s|%s|%s|wrr", decision.ScopeID, realm, model, tierKey(tier))
	weights := map[string]int{}
	total := 0
	for _, a := range rows {
		w := 1
		if tier != nil {
			if v, ok := tier.Weights[a.UID()]; ok && v > 0 {
				w = v
			}
		}
		weights[a.UID()] = w
		total += w
	}
	m.mu.Lock()
	state, ok := m.selectors[key]
	if !ok || len(m.selectors) >= 512 {
		state = map[string]int{}
		m.selectors[key] = state
	}
	for _, a := range rows {
		state[a.UID()] += weights[a.UID()]
	}
	var chosen Candidate
	best := -1
	for _, a := range rows {
		if state[a.UID()] > best {
			best = state[a.UID()]
			chosen = a
		}
	}
	if chosen != nil {
		state[chosen.UID()] -= total
	}
	m.mu.Unlock()
	return chosen
}

func tierKey(t *Tier) string {
	if t == nil {
		return "all"
	}
	return t.ID
}

// Preview renders a side-effect-free routing preview for the panel.
func (m *Manager) Preview(accounts []AccountLike, model string, key *settings.KeyEntry,
	realm, fallbackRealm string) (map[string]any, error) {
	decision, err := m.Resolve(accounts, model, key, realm)
	if err != nil {
		return nil, err
	}
	if decision.Realm == "" && fallbackRealm != "" {
		decision.Realm = fallbackRealm
		within := map[string]bool{}
		for _, a := range accounts {
			if a.RealmOf() == fallbackRealm {
				within[a.UID()] = true
			}
		}
		if decision.AllowedUIDs == nil {
			decision.AllowedUIDs = within
		} else {
			intersect := map[string]bool{}
			for uid := range decision.AllowedUIDs {
				if within[uid] {
					intersect[uid] = true
				}
			}
			decision.AllowedUIDs = intersect
		}
	}
	rows := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		inScope := decision.AllowedUIDs == nil || decision.AllowedUIDs[a.UID()]
		inRealm := decision.Realm == "" || a.RealmOf() == decision.Realm
		rows = append(rows, map[string]any{
			"uid": a.UID(), "realm": a.RealmOf(),
			"in_scope": inScope, "in_realm": inRealm,
			"eligible": inScope && inRealm,
		})
	}
	return map[string]any{
		"model": model, "realm": decision.Realm,
		"strategy":     decision.Strategy,
		"matched_rule": decision.Matched,
		"tiers":        decision.Tiers,
		"policy":       decision.Policy,
		"accounts":     rows,
	}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func asString(v any) string {
	s, _ := v.(string)
	return s
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

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func randomHex(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = "0123456789abcdef"[i%16]
	}
	return string(out)
}

// fnmatch implements '*' and '?' globbing.
func fnmatch(name, pattern string) bool {
	return fnmatchRec([]rune(name), []rune(pattern))
}

func fnmatchRec(name, pat []rune) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if fnmatchRec(name[i:], pat[1:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			name, pat = name[1:], pat[1:]
		default:
			if len(name) == 0 || name[0] != pat[0] {
				return false
			}
			name, pat = name[1:], pat[1:]
		}
	}
	return len(name) == 0
}
