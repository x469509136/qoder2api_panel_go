package session

import (
	"testing"
	"time"
)

func msgs(texts ...string) []any {
	out := make([]any, 0, len(texts))
	for _, t := range texts {
		out = append(out, map[string]any{"role": "user", "content": t})
	}
	return out
}

func TestMemoryPrefixAffinity(t *testing.T) {
	c := NewMemoryCache(time.Hour, 128, HashSecret([]byte("secret")))
	first, err := c.Resolve(msgs("hello"), "acct-1", "cn", "qmodel", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("empty session id")
	}
	// The same prefix must resolve to the same session.
	again, _ := c.Resolve(msgs("hello"), "acct-1", "cn", "qmodel", "", "", nil)
	if again != first {
		t.Fatalf("prefix affinity broke: %q != %q", again, first)
	}
	// A growing conversation keeps matching its earlier prefix.
	grown, _ := c.Resolve(msgs("hello", "and then"), "acct-1", "cn", "qmodel", "", "", nil)
	if grown != first {
		t.Fatalf("a grown prefix lost affinity: %q != %q", grown, first)
	}
	// A different account must not share the mapping.
	other, _ := c.Resolve(msgs("hello"), "acct-2", "cn", "qmodel", "", "", nil)
	if other == first {
		t.Fatal("two accounts shared one upstream session")
	}
	// A different model is isolated too.
	modelB, _ := c.Resolve(msgs("hello"), "acct-1", "cn", "gmodel", "", "", nil)
	if modelB == first {
		t.Fatal("two models shared one upstream session")
	}
}

func TestMemoryCacheRequiresUserPrefix(t *testing.T) {
	c := NewMemoryCache(time.Hour, 16, HashSecret([]byte("s")))
	// Only a system message: there is no user prefix to match on, so the cache
	// must mint a random id rather than merge unrelated conversations.
	onlySystem := []any{map[string]any{"role": "system", "content": "shared preamble"}}
	a, _ := c.Resolve(onlySystem, "acct", "cn", "m", "", "", nil)
	b, _ := c.Resolve(onlySystem, "acct", "cn", "m", "", "", nil)
	if a == b {
		t.Fatal("a system-only prefix merged two conversations")
	}
}

func TestMemoryCacheLRUBound(t *testing.T) {
	c := NewMemoryCache(time.Hour, 4, HashSecret([]byte("s")))
	for i := 0; i < 20; i++ {
		if _, err := c.Resolve(msgs(string(rune('a'+i))), "acct", "cn", "m", "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// The cache must stay bounded.
	if len(c.entries) > 4*2 {
		t.Fatalf("cache grew past its bound: %d entries", len(c.entries))
	}
}

func TestBuildPrefixesMinimumIsFirstUser(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "preamble"},
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": "second"},
	}
	prefixes := BuildPrefixes(messages, "scope")
	if len(prefixes) != 3 {
		t.Fatalf("got %d prefixes, want 3 (one per message from the first user on)", len(prefixes))
	}
}

func TestParseRedisURL(t *testing.T) {
	c, err := ParseRedisURL("redis://user:pw@host.example:6380/3", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "host.example" || c.Port != 6380 || c.Database != 3 {
		t.Fatalf("parsed = %+v", c)
	}
	if c.Username != "user" || c.Password != "pw" {
		t.Fatalf("credentials = %q/%q", c.Username, c.Password)
	}
	if _, err := ParseRedisURL("http://x", time.Second); err == nil {
		t.Fatal("a non-redis scheme must be rejected")
	}
	if _, err := ParseRedisURL("redis://host/20", time.Second); err == nil {
		t.Fatal("an out-of-range database must be rejected")
	}
	if c2, err := ParseRedisURL("rediss://host", time.Second); err != nil || !c2.TLS {
		t.Fatalf("rediss must enable TLS: %+v %v", c2, err)
	}
}

func TestHashKeyHidesPrefix(t *testing.T) {
	secret := HashSecret([]byte("key"))
	h := secret.HashKey("the actual prompt text")
	if h == "" {
		t.Fatal("empty hash")
	}
	if len(h) != 64 {
		t.Fatalf("hash length %d, want 64 hex chars", len(h))
	}
	if h == secret.HashKey("different text") {
		t.Fatal("different prefixes hashed identically")
	}
}

func TestConfigFromEnvOff(t *testing.T) {
	t.Setenv("QD_SESSION_CACHE", "off")
	t.Setenv("QD_SESSION_REDIS_URL", "redis://host")
	cfg := FromEnv()
	if cfg.Mode != "off" {
		t.Fatalf("an explicit off must win, got %q", cfg.Mode)
	}
	if Build(cfg, HashSecret([]byte("s"))) != nil {
		t.Fatal("off must produce no cache")
	}
}

func TestConfigFromEnvMemory(t *testing.T) {
	t.Setenv("QD_SESSION_CACHE", "memory")
	t.Setenv("QD_SESSION_CACHE_TTL", "120")
	t.Setenv("QD_SESSION_CACHE_MAX_ENTRIES", "64")
	cfg := FromEnv()
	if cfg.Mode != "memory" || cfg.TTL != 120*time.Second || cfg.MaxEntries != 64 {
		t.Fatalf("config = %+v", cfg)
	}
	cache := Build(cfg, HashSecret([]byte("s")))
	if cache == nil || cache.Backend() != "memory" {
		t.Fatalf("cache = %v", cache)
	}
}
