// Package settings persists the gateway's runtime configuration.
//
// Everything lives in accounts/settings.json so a change made from the web
// panel survives a restart. The panel password is never stored in clear text —
// only a PBKDF2-SHA256 digest.
package settings

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultPanelPassword is the password a fresh install accepts.
	DefaultPanelPassword = "admin"
	// PBKDF2Rounds is the work factor for the panel password digest.
	PBKDF2Rounds = 120000
	// SessionTTL is how long a panel token stays valid.
	SessionTTL = 7 * 24 * 3600
)

// Store is a settings file handle bound to one accounts directory.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// NewStore binds a settings store to accountsDir.
func NewStore(accountsDir string) *Store { return &Store{dir: accountsDir} }

// Dir returns the bound accounts directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the settings.json path.
func (s *Store) Path() string { return filepath.Join(s.dir, "settings.json") }

// Load returns the persisted settings, or an empty map on a fresh install.
func (s *Store) Load() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() map[string]any {
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		return map[string]any{}
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return map[string]any{}
	}
	if data == nil {
		return map[string]any{}
	}
	return data
}

// Save writes the whole document atomically (tmp + rename).
func (s *Store) Save(data map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(data)
}

func (s *Store) saveLocked(data map[string]any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path() + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	// The 0600 argument is advisory on Windows; hardenPerm applies the real
	// ACL. Do it before the rename so the final name is never world-readable.
	if err := hardenPerm(tmp, false); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.Path())
}

// Update reads, applies mutate under the write lock, and writes back.
func (s *Store) Update(mutate func(map[string]any)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.loadLocked()
	mutate(data)
	return s.saveLocked(data)
}

// ---------------------------------------------------------------------------
// Panel password
// ---------------------------------------------------------------------------

func digest(password, saltHex string, rounds int) (string, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

// PanelPasswordIsDefault reports whether the panel still accepts "admin".
func (s *Store) PanelPasswordIsDefault() bool {
	data := s.Load()
	if str(data["panel_password_hash"]) == "" {
		return true
	}
	return data["panel_password_default"] == true
}

// VerifyPanelPassword checks password against the stored digest.
func (s *Store) VerifyPanelPassword(password string) bool {
	data := s.Load()
	stored := str(data["panel_password_hash"])
	if stored == "" {
		return password == DefaultPanelPassword
	}
	if data["panel_password_default"] == true {
		return password == DefaultPanelPassword
	}
	salt := str(data["panel_password_salt"])
	if salt == "" {
		return false
	}
	rounds := int(num(data["panel_password_rounds"], PBKDF2Rounds))
	given, err := digest(password, salt, rounds)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(stored)) == 1
}

// SetPanelPassword stores a new password digest.
func (s *Store) SetPanelPassword(password string) error {
	return s.Update(func(data map[string]any) {
		if password == DefaultPanelPassword {
			delete(data, "panel_password_salt")
			delete(data, "panel_password_rounds")
			data["panel_password_hash"] = ""
			data["panel_password_default"] = true
			return
		}
		salt := randomHex(16)
		data["panel_password_salt"] = salt
		data["panel_password_rounds"] = PBKDF2Rounds
		h, _ := digest(password, salt, PBKDF2Rounds)
		data["panel_password_hash"] = h
		data["panel_password_default"] = false
	})
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// KeyEntry is one configured API key.
type KeyEntry struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Key               string   `json:"key"`
	Realm             string   `json:"realm"`
	Enabled           bool     `json:"enabled"`
	CreatedAt         string   `json:"created_at"`
	Models            []string `json:"models"`
	AccountIDs        []string `json:"account_ids"`
	AccountGroups     []string `json:"account_groups"`
	Note              string   `json:"note"`
	AnthropicThinking string   `json:"anthropic_thinking"`
	Source            string   `json:"source,omitempty"`
}

// Realms a key may be bound to (empty means "follow the panel switch").
var Realms = []string{"", "intl", "cn"}

// CleanKeyEntry normalises one stored entry; nil means unusable.
func CleanKeyEntry(entry map[string]any) *KeyEntry {
	if entry == nil {
		return nil
	}
	key := strings.TrimSpace(str(entry["key"]))
	if key == "" {
		return nil
	}
	realm := strings.ToLower(strings.TrimSpace(str(entry["realm"])))
	if !contains(Realms, realm) {
		realm = ""
	}
	id := str(entry["id"])
	if id == "" {
		id = randomHex(6)
	}
	name := strings.TrimSpace(str(entry["name"]))
	if name == "" {
		name = "未命名"
	}
	groups := cleanSelectors(entry["account_groups"], true)
	if len(groups) == 0 {
		if g, ok := entry["group"]; ok && g != nil {
			groups = cleanSelectors([]any{g}, true)
		}
	}
	thinking := strings.ToLower(strings.TrimSpace(str(entry["anthropic_thinking"])))
	if thinking != "empty" && thinking != "off" {
		thinking = ""
	}
	created := str(entry["created_at"])
	if created == "" {
		created = time.Now().Format("2006/01/02 15:04")
	}
	return &KeyEntry{
		ID:                id,
		Name:              name,
		Key:               key,
		Realm:             realm,
		Enabled:           !(entry["enabled"] == false),
		CreatedAt:         created,
		Models:            CleanModelPatterns(entry["models"]),
		AccountIDs:        cleanSelectors(entry["account_ids"], false),
		AccountGroups:     groups,
		Note:              truncate(strings.TrimSpace(str(entry["note"])), 120),
		AnthropicThinking: thinking,
	}
}

func cleanSelectors(value any, allowEmpty bool) []string {
	if value == nil {
		return nil
	}
	var items []any
	switch v := value.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if (s != "" || allowEmpty) && !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// APIKeys returns every configured key, newest shape first.
//
// A settings file written by an older build only has the single
// api_key/api_key_set pair; it is surfaced as one unbound entry so upgrades
// keep working without a migration step.
func (s *Store) APIKeys() []*KeyEntry {
	data := s.Load()
	if stored, ok := data["api_keys"].([]any); ok {
		out := make([]*KeyEntry, 0, len(stored))
		seen := map[string]bool{}
		for _, raw := range stored {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			entry := CleanKeyEntry(m)
			if entry != nil && !seen[entry.Key] {
				seen[entry.Key] = true
				out = append(out, entry)
			}
		}
		return out
	}
	if data["api_key_set"] == true {
		legacy := strings.TrimSpace(str(data["api_key"]))
		if legacy != "" {
			return []*KeyEntry{{
				ID: "legacy", Name: "默认（跟随面板切换）", Key: legacy,
				Realm: "", Enabled: true,
			}}
		}
	}
	return nil
}

// SetAPIKeys replaces the whole key list and drops the legacy single-key pair
// so there is exactly one source of truth.
func (s *Store) SetAPIKeys(keys []*KeyEntry) ([]*KeyEntry, error) {
	cleaned := make([]*KeyEntry, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if k == nil || k.Key == "" || seen[k.Key] {
			continue
		}
		seen[k.Key] = true
		cp := *k
		cleaned = append(cleaned, &cp)
	}
	err := s.Update(func(data map[string]any) {
		data["api_keys"] = cleaned
		delete(data, "api_key")
		delete(data, "api_key_set")
	})
	return cleaned, err
}

// MatchAPIKey finds which configured key a request presented, if any.
func (s *Store) MatchAPIKey(supplied string, extraKeys []string) *KeyEntry {
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return nil
	}
	for _, entry := range s.APIKeys() {
		if entry.Enabled && constantTimeEqual(supplied, entry.Key) {
			cp := *entry
			cp.Source = "panel"
			return &cp
		}
	}
	for _, candidate := range extraKeys {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && constantTimeEqual(supplied, candidate) {
			return &KeyEntry{
				ID: "launcher", Name: "启动参数", Key: candidate,
				Realm: "", Enabled: true, Source: "launcher",
			}
		}
	}
	return nil
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// APIKeyOverride returns the panel-managed single key (legacy installs).
func (s *Store) APIKeyOverride() (string, bool) {
	data := s.Load()
	if data["api_key_set"] != true {
		return "", false
	}
	return str(data["api_key"]), true
}

// SetAPIKey stores the legacy single key.
func (s *Store) SetAPIKey(key string) error {
	return s.Update(func(data map[string]any) {
		data["api_key"] = key
		data["api_key_set"] = true
	})
}

// EnsureLauncherKey returns the persisted LAN key, creating one on first use.
//
// LAN mode must never ship a well-known default: the gateway spends the
// account's own upstream quota, so anyone on the network could drain it.
func (s *Store) EnsureLauncherKey() (string, bool, error) {
	var created bool
	var key string
	err := s.Update(func(data map[string]any) {
		if existing := strings.TrimSpace(str(data["launcher_key"])); existing != "" {
			key = existing
			return
		}
		key = "qd-" + randomURLSafe(24)
		data["launcher_key"] = key
		created = true
	})
	return key, created, err
}

// ---------------------------------------------------------------------------
// Auth / toggles
// ---------------------------------------------------------------------------

// AuthDisabled reports whether API-key checking was switched off entirely.
func (s *Store) AuthDisabled() bool { return s.Load()["auth_disabled"] == true }

// SetAuthDisabled flips the global API-key requirement.
func (s *Store) SetAuthDisabled(disabled bool) error {
	return s.Update(func(data map[string]any) { data["auth_disabled"] = disabled })
}

// WebToolsEnabled reports the persisted "local web tools" switch.
func (s *Store) WebToolsEnabled() bool { return s.Load()["web_tools"] == true }

// SetWebToolsEnabled persists the local web-tools switch.
func (s *Store) SetWebToolsEnabled(on bool) error {
	return s.Update(func(data map[string]any) { data["web_tools"] = on })
}

// ToolEchoMode returns the persisted tool-fragment policy ("" = follow env).
func (s *Store) ToolEchoMode() string {
	v := strings.ToLower(strings.TrimSpace(str(s.Load()["tool_echo_mode"])))
	if v == "strict" || v == "lenient" {
		return v
	}
	return ""
}

// SetToolEchoMode persists the tool-fragment policy ("" clears it).
func (s *Store) SetToolEchoMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	err := s.Update(func(data map[string]any) {
		if mode == "strict" || mode == "lenient" {
			data["tool_echo_mode"] = mode
		} else {
			delete(data, "tool_echo_mode")
		}
	})
	return s.ToolEchoMode(), err
}

// AnthropicThinking returns the persisted Anthropic thinking policy.
func (s *Store) AnthropicThinking() string {
	v := strings.ToLower(strings.TrimSpace(str(s.Load()["anthropic_thinking"])))
	if v == "empty" || v == "off" {
		return v
	}
	return ""
}

// SetAnthropicThinking persists the Anthropic thinking policy.
func (s *Store) SetAnthropicThinking(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	err := s.Update(func(data map[string]any) {
		if mode == "empty" || mode == "off" {
			data["anthropic_thinking"] = mode
		} else {
			delete(data, "anthropic_thinking")
		}
	})
	return s.AnthropicThinking(), err
}

// ModelContext returns {modelKey: "1M"|"400K"|200K|<int>}.
func (s *Store) ModelContext() map[string]any {
	raw, ok := s.Load()["model_context"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range raw {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		switch tv := v.(type) {
		case bool:
			continue
		case float64:
			if tv > 0 {
				out[key] = int(tv)
			}
		case string:
			if label := strings.TrimSpace(tv); label != "" {
				out[key] = label
			}
		}
	}
	return out
}

// SetModelContext sets or clears one model's context tier.
func (s *Store) SetModelContext(model, label string) (map[string]any, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("model required")
	}
	cur := s.ModelContext()
	label = strings.TrimSpace(label)
	if label == "" || label == "0" {
		delete(cur, model)
	} else if n, err := strconv.Atoi(label); err == nil && n > 0 {
		cur[model] = n
	} else {
		if len(label) > 12 {
			label = label[:12]
		}
		cur[model] = label
	}
	err := s.Update(func(data map[string]any) { data["model_context"] = cur })
	return cur, err
}

// ---------------------------------------------------------------------------
// Model gates
// ---------------------------------------------------------------------------

var modelSplit = regexp.MustCompile(`[,;\n]`)

// CleanModelPatterns normalises a string / list / set of patterns.
// Invalid types yield nil (fail-open: reading config never throws).
func CleanModelPatterns(value any) []string {
	var raw []string
	switch v := value.(type) {
	case string:
		raw = modelSplit.Split(v, -1)
	case []any:
		for _, it := range v {
			raw = append(raw, str(it))
		}
	case []string:
		raw = v
	default:
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		p := strings.ToLower(strings.TrimSpace(item))
		if p != "" && !contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// KeyAllowsModel reports whether the key's whitelist permits model.
//
// An empty list (or a missing field) means unrestricted. A restricted key with
// an empty model name is rejected: there is no basis to judge, and letting it
// through would burn an upstream call on a nonsense request.
func KeyAllowsModel(entry *KeyEntry, model string) bool {
	if entry == nil {
		return true
	}
	patterns := CleanModelPatterns(entry.Models)
	if len(patterns) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, p := range patterns {
		if fnmatch(name, p) {
			return true
		}
	}
	return false
}

// BannedModels returns the global ban list.
func (s *Store) BannedModels() []string {
	return CleanModelPatterns(s.Load()["banned_models"])
}

// SetBannedModels persists the global ban list.
func (s *Store) SetBannedModels(value any) ([]string, error) {
	patterns := CleanModelPatterns(value)
	err := s.Update(func(data map[string]any) { data["banned_models"] = patterns })
	return patterns, err
}

// IsModelBanned reports whether model matches any global ban pattern.
func (s *Store) IsModelBanned(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	for _, p := range s.BannedModels() {
		if fnmatch(name, p) {
			return true
		}
	}
	return false
}

// fnmatch implements the subset of Python's fnmatch used by the ban lists:
// '*' and '?' wildcards, everything else literal, case-sensitive on inputs
// that are already lower-cased.
func fnmatch(name, pattern string) bool {
	return fnmatchRec([]rune(name), []rune(pattern))
}

func fnmatchRec(name, pat []rune) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			for len(pat) > 1 && pat[1] == '*' {
				pat = pat[1:]
			}
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

// ---------------------------------------------------------------------------
// Panel sessions
// ---------------------------------------------------------------------------

// PanelSecretKey is the settings key holding the token-signing secret.
const PanelSecretKey = "panel_session_secret"

var safeTokenPart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// settingsRewritable reports whether settings.json is absent or parseable.
//
// Why this exists: Load() turns both "missing file" and "parse error" into an
// empty map, but Save() writes the whole document. Handing a parse-failure
// empty map to Save would erase the password hash, the API keys and the limits
// — one transient I/O error would be unrecoverable.
func (s *Store) settingsRewritable() bool {
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		return os.IsNotExist(err)
	}
	var probe any
	return json.Unmarshal(raw, &probe) == nil
}

// PanelSessionSecret returns the persisted token-signing secret, minting one
// on first use. It refuses to overwrite an unparseable settings.json.
func (s *Store) PanelSessionSecret() (string, error) {
	if !s.settingsRewritable() {
		return "", errors.New("settings.json unreadable; refusing to overwrite it")
	}
	var secret string
	err := s.Update(func(data map[string]any) {
		if existing := str(data[PanelSecretKey]); len(existing) >= 32 {
			secret = existing
			return
		}
		secret = randomURLSafe(48)
		data[PanelSecretKey] = secret
	})
	return secret, err
}

// PanelSessions issues and verifies self-contained panel tokens.
//
// The token is "<expiry>.<nonce>.<hmac_sha256>": there is no server-side
// session table, so the gateway restarting does not log every browser out.
type PanelSessions struct {
	ttl       int64
	store     *Store
	mu        sync.RWMutex
	secret    string
	ephemeral bool
	revoked   map[string]int64
}

// NewPanelSessions builds a session manager over the settings store.
func NewPanelSessions(store *Store) *PanelSessions {
	return &PanelSessions{ttl: SessionTTL, store: store, revoked: map[string]int64{}}
}

// TTL exposes the token lifetime in seconds.
func (p *PanelSessions) TTL() int64 { return p.ttl }

// Ephemeral reports whether the signing secret could not be persisted.
func (p *PanelSessions) Ephemeral() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ephemeral
}

func (p *PanelSessions) key() string {
	p.mu.RLock()
	if p.secret != "" {
		defer p.mu.RUnlock()
		return p.secret
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.secret != "" {
		return p.secret
	}
	if s, err := p.store.PanelSessionSecret(); err == nil {
		p.secret = s
	} else {
		p.secret = randomURLSafe(48)
		p.ephemeral = true
	}
	return p.secret
}

func (p *PanelSessions) sign(expiry int64, nonce string) string {
	mac := hmac.New(sha256.New, []byte(p.key()))
	fmt.Fprintf(mac, "%d.%s", expiry, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// Create issues a fresh panel token.
func (p *PanelSessions) Create() string {
	expiry := time.Now().Unix() + p.ttl
	nonce := randomURLSafe(12)
	return fmt.Sprintf("%d.%s.%s", expiry, nonce, p.sign(expiry, nonce))
}

// Valid verifies a token's shape, expiry, signature and revocation.
func (p *PanelSessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expiryS, nonce, signature := parts[0], parts[1], parts[2]
	if len(expiryS) == 0 || len(expiryS) > 12 {
		return false
	}
	for _, r := range expiryS {
		if r < '0' || r > '9' {
			return false
		}
	}
	if len(nonce) < 1 || len(nonce) > 64 || !safeTokenPart.MatchString(nonce) {
		return false
	}
	if len(signature) != 64 || !safeTokenPart.MatchString(signature) {
		return false
	}
	expiry, err := strconv.ParseInt(expiryS, 10, 64)
	if err != nil || expiry < time.Now().Unix() {
		return false
	}
	if !hmac.Equal([]byte(signature), []byte(p.sign(expiry, nonce))) {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, revoked := p.revoked[token]
	return !revoked
}

// Revoke blacklists one token for the rest of its lifetime (process-local).
func (p *PanelSessions) Revoke(token string) {
	if token == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().Unix()
	if len(p.revoked) > 256 {
		for k, v := range p.revoked {
			if v <= now {
				delete(p.revoked, k)
			}
		}
	}
	p.revoked[token] = now + p.ttl
}

// RevokeAll rotates the signing secret so every issued token dies at once.
// This is what "change the password" needs; a plain revoke would be undone by
// a restart because the blacklist is process-local.
func (p *PanelSessions) RevokeAll() {
	newSecret := randomURLSafe(48)
	p.mu.Lock()
	p.secret = newSecret
	p.revoked = map[string]int64{}
	p.mu.Unlock()

	data := p.store.Load()
	data[PanelSecretKey] = newSecret
	_ = p.store.Save(data)
}

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

// LimitKeys are the four guardrails plus the expiring-window preference.
var LimitKeys = []string{
	"reserve_credits", "daily_token_limit", "daily_credit_limit",
	"model_daily_token_limit", "expiring_window_days",
}

// LimitRealms are the per-realm override slots.
var LimitRealms = []string{"intl", "cn"}

// LimitScopes are the three layers a limit can be set at.
var LimitScopes = []string{"global", "intl", "cn"}

// LimitsKey is the settings key holding the limits map.
const LimitsKey = "limits"

// LimitEntry is one guardrail's three-layer value. nil override = inherit.
type LimitEntry struct {
	Global int  `json:"global"`
	Intl   *int `json:"intl"`
	CN     *int `json:"cn"`
}

func emptyLimitEntry() LimitEntry { return LimitEntry{Global: 0} }

func coerceGlobal(v any) int {
	n := int(num(v, 0))
	if n < 0 {
		return 0
	}
	return n
}

func coerceOverride(v any) *int {
	if v == nil || v == "" {
		return nil
	}
	n := int(num(v, 0))
	if n < 0 {
		n = 0
	}
	return &n
}

func normalizeLimits(raw map[string]any) map[string]LimitEntry {
	out := map[string]LimitEntry{}
	for _, key := range LimitKeys {
		entry, _ := raw[key].(map[string]any)
		e := emptyLimitEntry()
		if entry != nil {
			e.Global = coerceGlobal(entry["global"])
			e.Intl = coerceOverride(entry["intl"])
			e.CN = coerceOverride(entry["cn"])
		}
		out[key] = e
	}
	return out
}

// LimitsData returns the grouped limits map, always complete.
func (s *Store) LimitsData() map[string]LimitEntry {
	raw, _ := s.Load()[LimitsKey].(map[string]any)
	return normalizeLimits(raw)
}

// LimitValue returns the effective value of one guardrail for one realm.
func (s *Store) LimitValue(key, realm string) int {
	entry, ok := s.LimitsData()[key]
	if !ok {
		return 0
	}
	if realm == "intl" && entry.Intl != nil {
		return *entry.Intl
	}
	if realm == "cn" && entry.CN != nil {
		return *entry.CN
	}
	return entry.Global
}

// LimitValues returns {"global": g, "intl": x, "cn": y} with overrides filled.
func (s *Store) LimitValues(key string) map[string]int {
	entry, ok := s.LimitsData()[key]
	if !ok {
		entry = emptyLimitEntry()
	}
	out := map[string]int{"global": entry.Global}
	if entry.Intl != nil {
		out["intl"] = *entry.Intl
	} else {
		out["intl"] = entry.Global
	}
	if entry.CN != nil {
		out["cn"] = *entry.CN
	} else {
		out["cn"] = entry.Global
	}
	return out
}

// SetLimit persists one guardrail's value for one scope.
func (s *Store) SetLimit(key, scope string, value any) (LimitEntry, error) {
	if !contains(LimitKeys, key) {
		return LimitEntry{}, fmt.Errorf("unknown limit: %s", key)
	}
	if !contains(LimitScopes, scope) {
		return LimitEntry{}, fmt.Errorf("unknown scope: %s", scope)
	}
	var result LimitEntry
	err := s.Update(func(data map[string]any) {
		raw, _ := data[LimitsKey].(map[string]any)
		limits := normalizeLimits(raw)
		entry := limits[key]
		switch scope {
		case "global":
			entry.Global = coerceGlobal(value)
		case "intl":
			entry.Intl = coerceOverride(value)
		case "cn":
			entry.CN = coerceOverride(value)
		}
		limits[key] = entry
		asMap := map[string]any{}
		for k, v := range limits {
			cell := map[string]any{"global": v.Global}
			if v.Intl != nil {
				cell["intl"] = *v.Intl
			} else {
				cell["intl"] = nil
			}
			if v.CN != nil {
				cell["cn"] = *v.CN
			} else {
				cell["cn"] = nil
			}
			asMap[k] = cell
		}
		data[LimitsKey] = asMap
		result = entry
	})
	return result, err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func num(v any, def float64) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return def
		}
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return def
	}
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
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(buf)[:n]
}

const urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func randomURLSafe(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("x", n)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = urlAlphabet[int(b)%len(urlAlphabet)]
	}
	return string(out)
}
