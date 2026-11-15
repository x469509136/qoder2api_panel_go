package route

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xzs/bridge_qoder/internal/settings"
)

type fakeAccount struct {
	uid, group, realm string
	ready             bool
}

func (f fakeAccount) UID() string         { return f.uid }
func (f fakeAccount) Group() string       { return f.group }
func (f fakeAccount) RealmOf() string     { return f.realm }
func (f fakeAccount) IsReady(string) bool { return f.ready }
func (f fakeAccount) IsDegraded() bool    { return false }

func newManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(t.TempDir())
}

func TestResolveModelPattern(t *testing.T) {
	m := newManager(t)
	if err := os.MkdirAll(filepath.Dir(m.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"rules": []any{
			map[string]any{"id": "r1", "model": "qwen*", "realm": "cn",
				"accounts": []any{"a1"}, "priority": 0},
			map[string]any{"id": "r2", "model": "*", "realm": "", "priority": 10},
		},
	}
	blob, _ := json.Marshal(doc)
	if err := os.WriteFile(m.Path(), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := []AccountLike{
		fakeAccount{uid: "a1", realm: "cn"},
		fakeAccount{uid: "a2", realm: "cn"},
		fakeAccount{uid: "b1", realm: "intl"},
	}
	decision, err := m.Resolve(accounts, "Qwen3.8-Max", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Matched == nil || decision.Matched.ID != "r1" {
		t.Fatalf("matched = %+v", decision.Matched)
	}
	if decision.Realm != "cn" {
		t.Fatalf("realm = %q", decision.Realm)
	}
	if len(decision.AllowedUIDs) != 1 || !decision.AllowedUIDs["a1"] {
		t.Fatalf("allowed = %v", decision.AllowedUIDs)
	}
}

func TestRulesCannotWidenKeyScope(t *testing.T) {
	m := newManager(t)
	if err := os.MkdirAll(filepath.Dir(m.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"rules": []any{
		map[string]any{"id": "r1", "model": "*", "accounts": []any{"a2"}},
	}}
	blob, _ := json.Marshal(doc)
	_ = os.WriteFile(m.Path(), blob, 0o600)

	accounts := []AccountLike{
		fakeAccount{uid: "a1"}, fakeAccount{uid: "a2"}, fakeAccount{uid: "a3"},
	}
	// The key is scoped to a1; the rule wants a2. The intersection is empty, and
	// it must NOT be widened to a2.
	key := &settings.KeyEntry{ID: "k1", Enabled: true, AccountIDs: []string{"a1"}}
	decision, err := m.Resolve(accounts, "anything", key, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.AllowedUIDs) != 0 {
		t.Fatalf("a rule widened the key scope: %v", decision.AllowedUIDs)
	}
}

func TestKeyRealmConflict(t *testing.T) {
	m := newManager(t)
	key := &settings.KeyEntry{ID: "k", Realm: "cn"}
	if _, err := m.Resolve(nil, "m", key, "intl"); err == nil {
		t.Fatal("a key/request realm conflict must be rejected")
	}
}

func TestTierOrderAndStrategies(t *testing.T) {
	m := newManager(t)
	decision := &Decision{
		Strategy: "fill-first",
		Tiers: []Tier{
			{ID: "t1", Accounts: []string{"a1"}},
			{ID: "t2", Accounts: []string{"a2"}},
		},
	}
	pool := []Candidate{
		fakeAccount{uid: "a1", realm: "cn", ready: false},
		fakeAccount{uid: "a2", realm: "cn", ready: true},
	}
	// The preferred tier has no ready account, so selection falls to tier two.
	picked := m.Pick(decision, "cn", "", pool, "", nil)
	if picked == nil || picked.UID() != "a2" {
		t.Fatalf("picked = %v", picked)
	}
}

func TestRoundRobinAndWeighted(t *testing.T) {
	m := newManager(t)
	pool := []Candidate{
		fakeAccount{uid: "a", realm: "cn", ready: true},
		fakeAccount{uid: "b", realm: "cn", ready: true},
	}
	rr := &Decision{Strategy: "round-robin"}
	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		picked := m.Pick(rr, "cn", "m", pool, "", nil)
		seen[picked.UID()]++
	}
	if seen["a"] != 5 || seen["b"] != 5 {
		t.Fatalf("round-robin distribution = %v", seen)
	}

	wrr := &Decision{Strategy: "weighted-round-robin",
		Tiers: []Tier{{ID: "t", Accounts: []string{"a", "b"},
			Weights: map[string]int{"a": 3, "b": 1}}}}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		picked := m.Pick(wrr, "cn", "m", pool, "", nil)
		counts[picked.UID()]++
	}
	if counts["a"] <= counts["b"] {
		t.Fatalf("weighted distribution ignores weights: %v", counts)
	}
}

func TestExcludeHonoured(t *testing.T) {
	m := newManager(t)
	pool := []Candidate{fakeAccount{uid: "a", realm: "cn", ready: true}}
	decision := &Decision{Strategy: "round-robin"}
	if picked := m.Pick(decision, "cn", "", pool, "", map[string]bool{"a": true}); picked != nil {
		t.Fatalf("an excluded account was selected: %v", picked.UID())
	}
}

func TestSaveRevisionConflict(t *testing.T) {
	m := newManager(t)
	rules := []Rule{{ID: "r1", Model: "*", Enabled: true, Strategy: "round-robin"}}
	cfg, err := m.Save(rules, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Revision == "" || cfg.Revision == "empty" {
		t.Fatalf("revision = %q", cfg.Revision)
	}
	if _, err := m.Save(rules, nil, nil, "bogus-revision"); err == nil {
		t.Fatal("a stale revision must be rejected")
	}
	if _, err := m.Save(rules, nil, nil, cfg.Revision); err != nil {
		t.Fatalf("the current revision must be accepted: %v", err)
	}
}

func TestPreviewHasNoSideEffects(t *testing.T) {
	m := newManager(t)
	accounts := []AccountLike{fakeAccount{uid: "a", realm: "cn"}}
	before, _ := m.Load()
	if _, err := m.Preview(accounts, "Qwen", nil, "", "cn"); err != nil {
		t.Fatal(err)
	}
	after, _ := m.Load()
	if before.Revision != after.Revision {
		t.Fatal("a preview changed the configuration")
	}
}
