package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func TestPanelPasswordDefault(t *testing.T) {
	s := newStore(t)
	if !s.PanelPasswordIsDefault() {
		t.Fatal("a fresh install must report the default password")
	}
	if !s.VerifyPanelPassword("admin") {
		t.Fatal("admin must open a fresh panel")
	}
	if s.VerifyPanelPassword("nope") {
		t.Fatal("a wrong password must not pass")
	}
}

func TestPanelPasswordRoundTrip(t *testing.T) {
	s := newStore(t)
	if err := s.SetPanelPassword("s3cret"); err != nil {
		t.Fatal(err)
	}
	if s.PanelPasswordIsDefault() {
		t.Fatal("still reporting the default password after a change")
	}
	if !s.VerifyPanelPassword("s3cret") {
		t.Fatal("the new password must verify")
	}
	if s.VerifyPanelPassword("admin") {
		t.Fatal("the default password must stop working")
	}
	// No plaintext password may reach the file.
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if containsBytes(raw, []byte("s3cret")) {
		t.Fatal("the panel password was stored in clear text")
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestPanelSessionsSurviveReload(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStore(dir)
	p1 := NewPanelSessions(s1)
	token := p1.Create()
	if !p1.Valid(token) {
		t.Fatal("fresh token must verify")
	}
	// A second process (simulated by a new store + manager) must accept the same
	// token: the signing secret is persisted.
	s2 := NewStore(dir)
	p2 := NewPanelSessions(s2)
	if !p2.Valid(token) {
		t.Fatal("token did not survive a restart")
	}
	// Rotating the secret kills every old token.
	p2.RevokeAll()
	if p2.Valid(token) {
		t.Fatal("token still valid after RevokeAll")
	}
}

func TestPanelSessionRejectsMalformed(t *testing.T) {
	s := newStore(t)
	p := NewPanelSessions(s)
	for _, bad := range []string{
		"", "a.b", "abc.def.ghi", "123.nonce", "123.nonce." + string(make([]byte, 64)),
		"１２３.nonce.sig",
	} {
		if p.Valid(bad) {
			t.Fatalf("accepted malformed token %q", bad)
		}
	}
}

func TestAPIKeyMatching(t *testing.T) {
	s := newStore(t)
	keys, err := s.SetAPIKeys([]*KeyEntry{
		{ID: "k1", Name: "one", Key: "key-one", Enabled: true, Realm: "cn"},
		{ID: "k2", Name: "two", Key: "key-two", Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("stored %d keys", len(keys))
	}
	entry := s.MatchAPIKey("key-one", nil)
	if entry == nil || entry.ID != "k1" {
		t.Fatalf("key-one did not match: %+v", entry)
	}
	if entry.Realm != "cn" {
		t.Fatalf("realm lost: %q", entry.Realm)
	}
	// A disabled key must not authenticate.
	if s.MatchAPIKey("key-two", nil) != nil {
		t.Fatal("a disabled key matched")
	}
	// A launcher key still works as a fallback.
	entry = s.MatchAPIKey("boot-key", []string{"boot-key"})
	if entry == nil || entry.ID != "launcher" {
		t.Fatalf("launcher key did not match: %+v", entry)
	}
}

func TestKeyAllowsModel(t *testing.T) {
	entry := &KeyEntry{Models: []string{"qwen*", "glm-5.3"}}
	if !KeyAllowsModel(entry, "Qwen3.8-Max") {
		t.Fatal("wildcard whitelist should match")
	}
	if !KeyAllowsModel(entry, "GLM-5.3") {
		t.Fatal("matching must be case-insensitive")
	}
	if KeyAllowsModel(entry, "Kimi-K3") {
		t.Fatal("an unlisted model must be rejected")
	}
	if KeyAllowsModel(entry, "") {
		t.Fatal("an empty model must be rejected on a restricted key")
	}
	if !KeyAllowsModel(&KeyEntry{}, "anything") {
		t.Fatal("an unrestricted key must allow everything")
	}
}

func TestBannedModelsFailOpen(t *testing.T) {
	s := newStore(t)
	if _, err := s.SetBannedModels("qwen*, kimi-k3"); err != nil {
		t.Fatal(err)
	}
	if !s.IsModelBanned("Qwen3.8-Max") {
		t.Fatal("qwen* should ban Qwen3.8-Max")
	}
	if s.IsModelBanned("GLM-5.3") {
		t.Fatal("GLM-5.3 must not be banned")
	}
}

func TestLimitsInheritance(t *testing.T) {
	s := newStore(t)
	if _, err := s.SetLimit("reserve_credits", "global", 100); err != nil {
		t.Fatal(err)
	}
	if got := s.LimitValue("reserve_credits", "cn"); got != 100 {
		t.Fatalf("cn should inherit 100, got %d", got)
	}
	if _, err := s.SetLimit("reserve_credits", "cn", 0); err != nil {
		t.Fatal(err)
	}
	if got := s.LimitValue("reserve_credits", "cn"); got != 0 {
		t.Fatalf("explicit 0 should disable cn, got %d", got)
	}
	if got := s.LimitValue("reserve_credits", "intl"); got != 100 {
		t.Fatalf("intl should still inherit 100, got %d", got)
	}
	if _, err := s.SetLimit("nope", "global", 1); err == nil {
		t.Fatal("an unknown limit key must be rejected")
	}
}

func TestSettingsRefusesToOverwriteCorruptFile(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PanelSessionSecret(); err == nil {
		t.Fatal("a corrupt settings file must refuse to be rewritten")
	}
	// The corrupt file is left as-is.
	raw, _ := os.ReadFile(s.Path())
	if string(raw) != "{not json" {
		t.Fatal("the corrupt settings file was modified")
	}
}

func TestPanelPasswordDoesNotWipeKeys(t *testing.T) {
	s := newStore(t)
	if _, err := s.SetAPIKeys([]*KeyEntry{{ID: "k", Key: "abc", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPanelPassword("newpass"); err != nil {
		t.Fatal(err)
	}
	if len(s.APIKeys()) != 1 {
		t.Fatal("changing the password dropped the API keys")
	}
}

func TestSettingsPathInsideDir(t *testing.T) {
	s := newStore(t)
	if filepath.Dir(s.Path()) != s.Dir() {
		t.Fatalf("settings path escaped the accounts dir: %s", s.Path())
	}
}
