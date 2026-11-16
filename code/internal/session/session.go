// Package session implements the opt-in prefix-affinity map from a conversation
// prefix to an upstream session id.
//
// What is stored: only HMAC(prefix) -> random UUID. Never conversation text,
// never an upstream token, never an API key. The actual inference cache is
// controlled by the upstream; a stable session id does not guarantee a hit.
package session

import (
	"bufio"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Minimal RESP2 Redis client
//
// Only GET/SET with EX and a Lua script are used; a full client would drag in a
// dependency for four commands.
// ---------------------------------------------------------------------------

// RedisClient is a tiny blocking RESP2 client.
type RedisClient struct {
	Host     string
	Port     int
	Database int
	Username string
	Password string
	TLS      bool
	Timeout  time.Duration
}

// ParseRedisURL builds a client from redis:// or rediss://.
func ParseRedisURL(raw string, timeout time.Duration) (*RedisClient, error) {
	if !strings.HasPrefix(raw, "redis://") && !strings.HasPrefix(raw, "rediss://") {
		return nil, errors.New("QD_SESSION_REDIS_URL must be redis:// or rediss://")
	}
	tlsOn := strings.HasPrefix(raw, "rediss://")
	rest := strings.TrimPrefix(strings.TrimPrefix(raw, "redis://"), "rediss://")
	userinfo := ""
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo = rest[:at]
		rest = rest[at+1:]
	}
	hostport := rest
	db := 0
	if slash := strings.Index(rest, "/"); slash >= 0 {
		hostport = rest[:slash]
		dbPart := strings.Trim(rest[slash+1:], "/")
		if dbPart != "" {
			if _, err := fmt.Sscanf(dbPart, "%d", &db); err != nil {
				return nil, errors.New("Redis database must be a number")
			}
		}
	}
	if db < 0 || db > 15 {
		return nil, errors.New("Redis database must be between 0 and 15")
	}
	host, port := hostport, 6379
	if colon := strings.LastIndex(hostport, ":"); colon >= 0 {
		host = hostport[:colon]
		if _, err := fmt.Sscanf(hostport[colon+1:], "%d", &port); err != nil {
			return nil, errors.New("invalid Redis port")
		}
	}
	c := &RedisClient{Host: host, Port: port, Database: db, TLS: tlsOn, Timeout: timeout}
	if userinfo != "" {
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			c.Username = userinfo[:colon]
			c.Password = userinfo[colon+1:]
		} else {
			c.Password = userinfo
		}
	}
	return c, nil
}

func encodeCommand(parts ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return []byte(b.String())
}

func readReply(r *bufio.Reader) (any, error) {
	kind, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	switch kind {
	case '-':
		return nil, errors.New(line)
	case '+':
		return line, nil
	case ':':
		var n int
		_, _ = fmt.Sscanf(line, "%d", &n)
		return n, nil
	case '$':
		var size int
		_, _ = fmt.Sscanf(line, "%d", &size)
		if size < 0 {
			return nil, nil
		}
		if size > 1024*1024 {
			return nil, errors.New("invalid Redis bulk length")
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:size]), nil
	case '*':
		var count int
		_, _ = fmt.Sscanf(line, "%d", &count)
		if count < 0 {
			return nil, nil
		}
		out := make([]any, 0, count)
		for i := 0; i < count; i++ {
			v, err := readReply(r)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, errors.New("unsupported Redis reply type")
}

// Execute runs one command.
func (c *RedisClient) Execute(args ...string) (any, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", c.Host, c.Port), c.Timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.Timeout))
	var rw io.ReadWriter = conn
	if c.TLS {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: c.Host})
		if err := tlsConn.Handshake(); err != nil {
			return nil, err
		}
		_ = tlsConn.SetDeadline(time.Now().Add(c.Timeout))
		rw = tlsConn
	}
	reader := bufio.NewReader(rw)
	call := func(parts ...string) (any, error) {
		if _, err := rw.Write(encodeCommand(parts...)); err != nil {
			return nil, err
		}
		return readReply(reader)
	}
	if c.Password != "" {
		var authArgs []string
		if c.Username != "" {
			authArgs = []string{"AUTH", c.Username, c.Password}
		} else {
			authArgs = []string{"AUTH", c.Password}
		}
		if v, err := call(authArgs...); err != nil || v != "OK" {
			return nil, errors.New("Redis authentication failed")
		}
	}
	if c.Database != 0 {
		if v, err := call("SELECT", fmt.Sprintf("%d", c.Database)); err != nil || v != "OK" {
			return nil, errors.New("Redis database selection failed")
		}
	}
	return call(args...)
}

// ---------------------------------------------------------------------------
// Prefix affinity
// ---------------------------------------------------------------------------

// AssignScript selects the longest matching prefix and claims it atomically.
const AssignScript = `
local selected = false
local matched = 0
for i = #KEYS, 1, -1 do
    local value = redis.call('GET', KEYS[i])
    if value then
        selected = value
        matched = i
        break
    end
end
if not selected then selected = ARGV[1] end
redis.call('SET', KEYS[1], selected, 'EX', ARGV[2])
if #KEYS > 1 then
    redis.call('SET', KEYS[#KEYS], selected, 'EX', ARGV[2])
end
return selected
`

// Cache is the prefix-affinity interface.
type Cache interface {
	// Resolve returns an upstream session id for the message prefix, minting one
	// when nothing matches.
	Resolve(messages []any, accountUID, realm, modelKey, clientSessionKey, tenantScope string, context map[string]any) (string, error)
	// Backend names the storage ("memory" or "redis").
	Backend() string
}

// HashSecret is the keyed hash used for prefix keys.
type HashSecret []byte

// HashKey derives the HMAC of a prefix.
func (h HashSecret) HashKey(prefix string) string {
	mac := hmac.New(sha256.New, h)
	mac.Write([]byte(prefix))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildPrefixes renders the increasing message prefixes that are eligible for
// matching. The first non-empty user message is the minimum: matching on a
// shared system prompt alone would merge unrelated conversations.
func BuildPrefixes(messages []any, scopeParts ...string) []string {
	base := strings.Join(scopeParts, "\x1f")
	minIdx := -1
	for i, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		if content, _ := mm["content"].(string); strings.TrimSpace(content) != "" {
			minIdx = i
			break
		}
	}
	if minIdx < 0 {
		return nil
	}
	var out []string
	for end := minIdx + 1; end <= len(messages); end++ {
		blob, err := json.Marshal(messages[:end])
		if err != nil {
			continue
		}
		out = append(out, base+"\x1e"+string(blob))
	}
	return out
}

// ---------------------------------------------------------------------------
// Memory backend
// ---------------------------------------------------------------------------

type memEntry struct {
	value   string
	expires time.Time
}

// MemoryCache is an LRU-bounded, TTL-based prefix cache.
type MemoryCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	secret     HashSecret
	entries    map[string]*memEntry
	order      []string
}

// NewMemoryCache builds an in-memory backend.
func NewMemoryCache(ttl time.Duration, maxEntries int, secret HashSecret) *MemoryCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &MemoryCache{ttl: ttl, maxEntries: maxEntries, secret: secret,
		entries: map[string]*memEntry{}}
}

// Backend names the storage.
func (m *MemoryCache) Backend() string { return "memory" }

// Resolve looks up the longest matching prefix or mints a new session.
func (m *MemoryCache) Resolve(messages []any, accountUID, realm, modelKey, clientSessionKey, tenantScope string, context map[string]any) (string, error) {
	prefixes := BuildPrefixes(messages, tenantScope, accountUID, realm, modelKey,
		clientSessionKey, contextDigest(context))
	if len(prefixes) == 0 {
		return randomUUID(), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	selected := ""
	for i := len(prefixes) - 1; i >= 0; i-- {
		key := m.secret.HashKey(prefixes[i])
		if e, ok := m.entries[key]; ok && e.expires.After(now) {
			selected = e.value
			break
		}
	}
	if selected == "" {
		selected = randomUUID()
	}
	// Claim both the first (stable anchor) and the last (full prefix) keys so a
	// growing conversation keeps matching at any depth.
	for _, idx := range []int{0, len(prefixes) - 1} {
		key := m.secret.HashKey(prefixes[idx])
		if _, exists := m.entries[key]; !exists {
			m.order = append(m.order, key)
		}
		m.entries[key] = &memEntry{value: selected, expires: now.Add(m.ttl)}
	}
	// Bound the map: drop the oldest keys whether or not they have expired. A
	// long-lived process with many short conversations would otherwise grow
	// without limit while nothing is stale yet.
	for len(m.order) > m.maxEntries {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, oldest)
	}
	return selected, nil
}

func contextDigest(context map[string]any) string {
	if len(context) == 0 {
		return ""
	}
	keys := make([]string, 0, len(context))
	for k := range context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		blob, _ := json.Marshal(context[k])
		b.WriteString(k)
		b.WriteByte('=')
		b.Write(blob)
		b.WriteByte(';')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Redis backend
// ---------------------------------------------------------------------------

// RedisCache stores prefix mappings in Redis.
type RedisCache struct {
	client *RedisClient
	ttl    time.Duration
	secret HashSecret
}

// NewRedisCache builds a Redis-backed cache.
func NewRedisCache(client *RedisClient, ttl time.Duration, secret HashSecret) *RedisCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &RedisCache{client: client, ttl: ttl, secret: secret}
}

// Backend names the storage.
func (r *RedisCache) Backend() string { return "redis" }

// Resolve looks up the longest matching prefix or mints a new session.
func (r *RedisCache) Resolve(messages []any, accountUID, realm, modelKey, clientSessionKey, tenantScope string, context map[string]any) (string, error) {
	prefixes := BuildPrefixes(messages, tenantScope, accountUID, realm, modelKey,
		clientSessionKey, contextDigest(context))
	if len(prefixes) == 0 {
		return randomUUID(), nil
	}
	args := []string{"EVAL", AssignScript, fmt.Sprintf("%d", len(prefixes))}
	for _, p := range prefixes {
		args = append(args, "qod:pfx:{"+r.secret.HashKey(p)+"}")
	}
	args = append(args, randomUUID(), fmt.Sprintf("%d", int(r.ttl.Seconds())))
	v, err := r.client.Execute(args...)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return randomUUID(), nil
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

// Config holds the resolved session-cache configuration.
type Config struct {
	Mode       string
	RedisURL   string
	TTL        time.Duration
	Timeout    time.Duration
	MaxEntries int
}

// FromEnv reads QD_SESSION_CACHE_* (an explicit "off" always wins).
func FromEnv() Config {
	url := strings.TrimSpace(os.Getenv("QD_SESSION_REDIS_URL"))
	mode := strings.TrimSpace(os.Getenv("QD_SESSION_CACHE"))
	if mode == "" {
		if url != "" {
			mode = "redis"
		} else {
			mode = "off"
		}
	}
	cfg := Config{Mode: strings.ToLower(mode), RedisURL: url,
		TTL: time.Hour, Timeout: 400 * time.Millisecond, MaxEntries: 4096}
	if v := intEnv("QD_SESSION_CACHE_TTL", 3600); v > 0 {
		cfg.TTL = time.Duration(v) * time.Second
	}
	if v := intEnv("QD_SESSION_REDIS_TTL", 0); v > 0 && cfg.TTL == time.Hour {
		cfg.TTL = time.Duration(v) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("QD_SESSION_REDIS_TIMEOUT")); raw != "" {
		var f float64
		if _, err := fmt.Sscanf(raw, "%g", &f); err == nil && f > 0 {
			cfg.Timeout = time.Duration(f * float64(time.Second))
		}
	}
	if v := intEnv("QD_SESSION_CACHE_MAX_ENTRIES", 0); v > 0 {
		cfg.MaxEntries = v
	}
	return cfg
}

func intEnv(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return def
	}
	return v
}

// Build constructs the configured cache, or nil when disabled.
//
// A misconfigured or unreachable Redis must not block inference: the caller
// falls back to a random session and logs only the error type (never the URL or
// credentials).
func Build(cfg Config, secret HashSecret) Cache {
	switch cfg.Mode {
	case "memory":
		return NewMemoryCache(cfg.TTL, cfg.MaxEntries, secret)
	case "redis":
		client, err := ParseRedisURL(cfg.RedisURL, cfg.Timeout)
		if err != nil {
			return nil
		}
		return NewRedisCache(client, cfg.TTL, secret)
	}
	return nil
}

func randomUUID() string {
	b := make([]byte, 16)
	// crypto/rand, not the clock: the Windows clock ticks at ~15 ms, so a
	// time-derived id repeats for calls in the same tick and two different
	// conversations would be handed the same upstream session.
	if _, err := cryptorand.Read(b); err != nil {
		// Unreachable on supported platforms; fall back to a hash of the clock
		// plus a counter so the value is still unique within the process.
		h := sha256.Sum256([]byte(fmt.Sprintf("%d|%d", time.Now().UnixNano(), atomic.AddUint64(&uuidSeq, 1))))
		copy(b, h[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var uuidSeq uint64
