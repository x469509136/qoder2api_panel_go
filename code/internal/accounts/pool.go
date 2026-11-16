package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AffinityTTL is how long a conversation stays pinned to one account.
const AffinityTTL = 7200

// AffinityMaxEntries bounds the session->uid map.
const AffinityMaxEntries = 5000

// SessionAffinity pins a conversation to one upstream account so the upstream
// prompt cache is hit.
type SessionAffinity struct {
	mu         sync.Mutex
	ttl        float64
	maxEntries int
	entries    map[string]affinityEntry
}

type affinityEntry struct {
	uid     string
	expires float64
}

// NewSessionAffinity builds an affinity map.
func NewSessionAffinity(ttl float64, maxEntries int) *SessionAffinity {
	if ttl <= 0 {
		ttl = AffinityTTL
	}
	if maxEntries <= 0 {
		maxEntries = AffinityMaxEntries
	}
	return &SessionAffinity{ttl: ttl, maxEntries: maxEntries,
		entries: map[string]affinityEntry{}}
}

// Get returns the bound uid for a session key, or "".
func (s *SessionAffinity) Get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || e.expires < float64(time.Now().Unix()) {
		delete(s.entries, key)
		return ""
	}
	return e.uid
}

// Bind pins a session key to a uid, evicting expired entries first.
func (s *SessionAffinity) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := float64(time.Now().Unix())
	if len(s.entries) >= s.maxEntries {
		for k, e := range s.entries {
			if e.expires < now {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= s.maxEntries {
			// Evict the soonest-expiring entry: it is the least likely to be
			// needed again, and dropping the oldest random key would cause a
			// cache miss for an active conversation.
			var victim string
			var victimExp float64
			for k, e := range s.entries {
				if victim == "" || e.expires < victimExp {
					victim, victimExp = k, e.expires
				}
			}
			if victim != "" {
				delete(s.entries, victim)
			}
		}
	}
	s.entries[key] = affinityEntry{uid: uid, expires: now + s.ttl}
}

// Unbind drops a session key (used when the bound account fails).
func (s *SessionAffinity) Unbind(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// Size reports how many live bindings exist.
func (s *SessionAffinity) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ---------------------------------------------------------------------------
// Pool
// ---------------------------------------------------------------------------

// Pool is the dual-realm account set.
type Pool struct {
	mu       sync.RWMutex
	dir      string
	accounts []*Account
	byUID    map[string]*Account
	// Affinity pins conversations to accounts.
	Affinity *SessionAffinity
	// round-robin cursor for plain rotation
	cursor int
	// SetEnabledFn is called when an account changes state (wired by the server).
	OnChange func(*Account)
	log      func(level, msg string)
}

// NewPool loads every credential file in dir.
func NewPool(dir string, logFn func(level, msg string)) (*Pool, error) {
	p := &Pool{
		dir:      dir,
		byUID:    map[string]*Account{},
		Affinity: NewSessionAffinity(AffinityTTL, AffinityMaxEntries),
		log:      logFn,
	}
	if logFn == nil {
		p.log = func(string, string) {}
	}
	// Tighten the directory before loading: a permissive parent re-grants
	// inherited read access to every file created afterwards, so fixing
	// individual files alone would not hold. Failure is logged, not fatal —
	// a read-only volume must still let the gateway start.
	if err := hardenCredentialPath(dir, true); err != nil {
		p.log("WARN", "could not restrict permissions on "+dir+": "+err.Error())
	}
	if err := p.Load(); err != nil {
		return p, err
	}
	return p, nil
}

// Dir returns the accounts directory.
func (p *Pool) Dir() string { return p.dir }

// Load scans the accounts directory for credential files.
//
// Only top-level *.json files are accounts; subdirectories (scheduler/, admin/)
// are skipped so their state files are never mistaken for credentials.
func (p *Pool) Load() error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	accounts := make([]*Account, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := entry.Name()
		if name == "settings.json" || name == "machine_identity.json" ||
			strings.HasPrefix(name, "models-cache-") ||
			name == CreditSourceFile {
			continue
		}
		path := filepath.Join(p.dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			p.log("WARN", fmt.Sprintf("skip unparseable account file %s: %v", name, err))
			continue
		}
		if asString(data["uid"]) == "" || asString(data["accessToken"]) == "" {
			continue
		}
		acc, err := NewAccount(data, path)
		if err != nil {
			p.log("WARN", fmt.Sprintf("skip invalid account %s: %v", name, err))
			continue
		}
		accounts = append(accounts, acc)
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		return accounts[i].AddedAt < accounts[j].AddedAt
	})

	p.mu.Lock()
	p.accounts = accounts
	p.byUID = make(map[string]*Account, len(accounts))
	for _, a := range accounts {
		p.byUID[a.UID] = a
	}
	p.mu.Unlock()
	return nil
}

// Accounts returns a snapshot of the account list.
func (p *Pool) Accounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Get returns one account by uid.
func (p *Pool) Get(uid string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid]
}

// Add inserts an account and persists it.
func (p *Pool) Add(acc *Account) error {
	if err := acc.Save(p.dir); err != nil {
		return err
	}
	p.mu.Lock()
	if existing, ok := p.byUID[acc.UID]; ok {
		for i, a := range p.accounts {
			if a == existing {
				p.accounts[i] = acc
				break
			}
		}
	} else {
		p.accounts = append(p.accounts, acc)
	}
	p.byUID[acc.UID] = acc
	p.mu.Unlock()
	if p.OnChange != nil {
		p.OnChange(acc)
	}
	return nil
}

// Remove deletes an account and its credential file.
func (p *Pool) Remove(uid string) bool {
	p.mu.Lock()
	acc := p.byUID[uid]
	if acc == nil {
		p.mu.Unlock()
		return false
	}
	delete(p.byUID, uid)
	for i, a := range p.accounts {
		if a.UID == uid {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	if acc.Path != "" {
		_ = os.Remove(acc.Path)
	}
	return true
}

// CountReady counts accounts that can serve now.
func (p *Pool) CountReady(realm, model string, allowedUIDs map[string]bool) int {
	n := 0
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if allowedUIDs != nil && !allowedUIDs[a.UID] {
			continue
		}
		if a.Ready(model) {
			n++
		}
	}
	return n
}

// Pick selects an account for one request.
//
// Order of preference:
//  1. explicit exclude set is honoured (accounts already tried)
//  2. expiring-first (when the expiring window preference is on)
//  3. accounts not in the downgrade window, then downgraded ones as fallback
//  4. round-robin within the eligible set
func (p *Pool) Pick(realm, model string, exclude map[string]bool, allowedUIDs map[string]bool) *Account {
	candidates := make([]*Account, 0, 8)
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if exclude != nil && exclude[a.UID] {
			continue
		}
		if allowedUIDs != nil && !allowedUIDs[a.UID] {
			continue
		}
		if !a.Ready(model) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil
	}

	// Preferences only reorder; they never widen the eligible set.
	sort.SliceStable(candidates, func(i, j int) bool {
		ei, ej := candidates[i].InExpiringWindow(), candidates[j].InExpiringWindow()
		if ei != ej {
			return ei
		}
		di, dj := candidates[i].Degraded(), candidates[j].Degraded()
		if di != dj {
			return !di
		}
		return false
	})

	// Round-robin within the preferred band (avoid always taking index 0).
	preferred := candidates[:0:0]
	for _, a := range candidates {
		if a.Degraded() {
			break
		}
		preferred = append(preferred, a)
	}
	if len(preferred) == 0 {
		preferred = candidates
	}
	p.mu.Lock()
	chosen := preferred[p.cursor%len(preferred)]
	p.cursor++
	p.mu.Unlock()
	return chosen
}

// PickForSession prefers the account a conversation is already bound to.
func (p *Pool) PickForSession(realm, model, sessionKey string, exclude map[string]bool, allowedUIDs map[string]bool) *Account {
	if sessionKey != "" {
		if uid := p.Affinity.Get(sessionKey); uid != "" {
			if acc := p.Get(uid); acc != nil &&
				(realm == "" || acc.Realm == realm) &&
				(exclude == nil || !exclude[uid]) &&
				(allowedUIDs == nil || allowedUIDs[uid]) &&
				acc.Ready(model) {
				return acc
			}
		}
	}
	acc := p.Pick(realm, model, exclude, allowedUIDs)
	if acc != nil && sessionKey != "" {
		p.Affinity.Bind(sessionKey, acc.UID)
	}
	return acc
}

// PickRepresentative returns an account to model the request body after
// (identity fields in the payload must match the account that will send it).
func (p *Pool) PickRepresentative(realm string, allowedUIDs map[string]bool) *Account {
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if allowedUIDs != nil && !allowedUIDs[a.UID] {
			continue
		}
		if a.InferenceEligible() {
			return a
		}
	}
	return nil
}

// ApplySettings pushes guardrail thresholds and the free-model list into every
// account in one pass.
func (p *Pool) ApplySettings(limits LimitsProvider, realm string, freeModels map[string]bool) {
	if limits == nil {
		return
	}
	for _, a := range p.Accounts() {
		a.ApplyLimits(
			limits.LimitValue("reserve_credits", a.Realm),
			limits.LimitValue("daily_token_limit", a.Realm),
			limits.LimitValue("daily_credit_limit", a.Realm),
			limits.LimitValue("model_daily_token_limit", a.Realm),
			limits.LimitValue("expiring_window_days", a.Realm),
			freeModels,
		)
	}
}

// LimitsProvider is the settings surface the pool needs.
type LimitsProvider interface {
	LimitValue(key, realm string) int
}

// SetEnabled flips one account's enabled flag and persists it.
func (p *Pool) SetEnabled(uid string, enabled bool) error {
	acc := p.Get(uid)
	if acc == nil {
		return fmt.Errorf("account not found: %s", uid)
	}
	acc.mu.Lock()
	acc.Enabled = enabled
	acc.mu.Unlock()
	if err := acc.Save(p.dir); err != nil {
		return err
	}
	if p.OnChange != nil {
		p.OnChange(acc)
	}
	return nil
}

// SetAllEnabled flips every account in a realm ("" = all realms).
func (p *Pool) SetAllEnabled(enabled bool, realm string) int {
	n := 0
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		a.mu.Lock()
		if a.Enabled != enabled {
			a.Enabled = enabled
			n++
		}
		a.mu.Unlock()
		_ = a.Save(p.dir)
	}
	return n
}

// SetVPC sets or clears one account's VPC selection.
func (p *Pool) SetVPC(uid string, value any) error {
	acc := p.Get(uid)
	if acc == nil {
		return fmt.Errorf("account not found: %s", uid)
	}
	override, err := NormalizeVPCOverride(value)
	if err != nil {
		return err
	}
	if acc.Realm != RealmCN && override != nil && *override != "" {
		return fmt.Errorf("VPC is only available for CN accounts")
	}
	acc.mu.Lock()
	acc.VPCOverride = override
	acc.mu.Unlock()
	cfg, err := RealmConfigFor(acc.Realm, override)
	if err != nil {
		return err
	}
	acc.mu.Lock()
	acc.Domain = cfg.Domain
	acc.mu.Unlock()
	return acc.Save(p.dir)
}

// RealmReadyTotal counts ready accounts in a realm (helper for single-pool
// cooldown clamping).
func (p *Pool) RealmReadyTotal(realm, model string) int {
	return p.CountReady(realm, model, nil)
}

// ListPublic returns the panel view of every account.
func (p *Pool) ListPublic(realm, group string) []map[string]any {
	out := []map[string]any{}
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if group != "" && a.Group != group {
			continue
		}
		out = append(out, a.Public())
	}
	return out
}

// RepresentativeMatching picks the first ready account for a realm, used for
// model discovery.
func (p *Pool) RepresentativeMatching(realm string) *Account {
	for _, a := range p.Accounts() {
		if realm != "" && a.Realm != realm {
			continue
		}
		if a.Ready("") {
			return a
		}
	}
	return nil
}
