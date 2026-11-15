// Package keepalive re-sends a minimal request before the upstream's prompt
// cache expires.
//
// The upstream expires idle prompt prefixes after about five minutes; a hit
// renews them. A cold start rewrites the whole 124k prefix (~137 credits) while a
// hit costs a cache read (~10.7 credits). This loop re-sends the last payload
// byte-for-byte with stream=false and max_tokens=1, which is enough to renew.
package keepalive

import (
	"container/list"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
)

// Entry is one registered conversation.
type Entry struct {
	Payload    map[string]any
	Account    *accounts.Account
	ModelKey   string
	Realm      string
	SessionID  string
	LastActive time.Time
	Pings      int
	Failures   int
}

// Sender performs one keepalive request; it returns the usage block or an error.
type Sender func(entry *Entry) error

// Manager holds the registered conversations and drives the loop.
type Manager struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	sender  Sender
	stop    chan struct{}
	running bool
	stats   Stats
}

// Stats is the observable state.
type Stats struct {
	PingsSent   int64 `json:"pings_sent"`
	PingsFailed int64 `json:"pings_failed"`
	StaleDrops  int64 `json:"stale_drops"`
	Registered  int   `json:"registered"`
}

// NewManager creates a keepalive manager.
func NewManager() *Manager {
	return &Manager{
		entries: map[string]*list.Element{},
		order:   list.New(),
	}
}

func envInt(name string, def, lo, hi int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	var v int
	if _, err := fmtSscanf(raw, &v); err != nil {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func envTruth(name, def string) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		raw = def
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Enabled reports QD_CACHE_KEEPALIVE (default off).
func Enabled() bool { return envTruth("QD_CACHE_KEEPALIVE", "0") }

// Interval is how long a conversation may sit idle before a ping.
func Interval() time.Duration {
	return time.Duration(envInt("QD_CACHE_KEEPALIVE_INTERVAL", 240, 30, 86400)) * time.Second
}

// MaxAge is how long a conversation stays eligible.
func MaxAge() time.Duration {
	return time.Duration(envInt("QD_CACHE_KEEPALIVE_MAX_AGE", 1800, 60, 86400)) * time.Second
}

// MaxConvs bounds how many payloads are held in memory.
func MaxConvs() int { return envInt("QD_CACHE_KEEPALIVE_MAX_CONVS", 2, 1, 64) }

// Configure registers the sender.
func (m *Manager) Configure(sender Sender) {
	m.mu.Lock()
	m.sender = sender
	m.mu.Unlock()
}

// Observe records a successful real conversation.
//
// Only genuine client traffic is registered: the account-test and diagnostic
// paths must not keep a conversation alive.
func (m *Manager) Observe(payload map[string]any, sessionID string, account *accounts.Account, realm, modelKey string) {
	if !Enabled() || sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := &Entry{Payload: payload, Account: account, ModelKey: modelKey,
		Realm: realm, SessionID: sessionID, LastActive: time.Now()}
	if el, ok := m.entries[sessionID]; ok {
		el.Value = entry
		m.order.MoveToBack(el)
		return
	}
	el := m.order.PushBack(entry)
	m.entries[sessionID] = el
	for m.order.Len() > MaxConvs() {
		oldest := m.order.Front()
		if oldest == nil {
			break
		}
		m.order.Remove(oldest)
		delete(m.entries, oldest.Value.(*Entry).SessionID)
	}
	m.stats.Registered = m.order.Len()
}

// Drop forgets one conversation.
func (m *Manager) Drop(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.entries[sessionID]; ok {
		m.order.Remove(el)
		delete(m.entries, sessionID)
		m.stats.Registered = m.order.Len()
	}
}

// Tick performs one inspection pass (exported for tests).
func (m *Manager) Tick(now time.Time) {
	if !Enabled() {
		return
	}
	m.mu.Lock()
	sender := m.sender
	var due []*Entry
	for el := m.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*Entry)
		idle := now.Sub(e.LastActive)
		if idle > MaxAge() {
			m.stats.StaleDrops++
			m.order.Remove(el)
			delete(m.entries, e.SessionID)
			continue
		}
		if idle >= Interval() {
			due = append(due, e)
		}
	}
	m.stats.Registered = m.order.Len()
	m.mu.Unlock()

	if sender == nil {
		return
	}
	for _, e := range due {
		if err := sender(e); err != nil {
			m.mu.Lock()
			m.stats.PingsFailed++
			e.Failures++
			// Back off after a failure instead of hammering a broken session.
			e.LastActive = now
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		m.stats.PingsSent++
		e.Pings++
		e.LastActive = now
		m.mu.Unlock()
	}
}

// Start launches the background loop.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.stop = make(chan struct{})
	stop := m.stop
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				m.Tick(now)
			}
		}
	}()
}

// Stop halts the background loop.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return
	}
	close(m.stop)
	m.running = false
}

// Status returns the observable state for /health.
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, m.order.Len())
	for el := m.order.Front(); el != nil; el = el.Next() {
		ids = append(ids, el.Value.(*Entry).SessionID)
	}
	return map[string]any{
		"enabled": Enabled(), "interval_s": Interval().Seconds(),
		"max_age_s": MaxAge().Seconds(), "max_convs": MaxConvs(),
		"pings_sent": m.stats.PingsSent, "pings_failed": m.stats.PingsFailed,
		"stale_drops": m.stats.StaleDrops, "sessions": ids,
	}
}
