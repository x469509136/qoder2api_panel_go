package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginThrottle slows down password guessing against /panel/login.
//
// The panel password defaults to "admin" and there was no rate limit, so a
// script on the LAN could brute-force it in seconds and then export every
// account (see docs/踩坑笔记.md S1). The throttle is deliberately simple and
// in-process: a restart clears it, which is acceptable because the alternative
// (a shared counter) would need storage the gateway does not have.
//
// Two windows, both keyed by client IP:
//
//	failures  — consecutive wrong passwords; the N+1th attempt is refused
//	cooldown  — how long a locked-out client must wait before trying again
type loginThrottle struct {
	mu       sync.Mutex
	entries  map[string]*loginAttempt
	failures int
	cooldown time.Duration
	now      func() time.Time
}

type loginAttempt struct {
	fails      int
	lockedTill time.Time
	lastSeen   time.Time
}

const (
	// loginFailureLimit is how many wrong passwords are tolerated per IP before
	// a cooldown starts. Five is high enough for a typo, low enough that
	// guessing "admin" from a list of 20 candidates is not viable.
	loginFailureLimit = 5
	// loginLockout is the first cooldown. It doubles per extra failure.
	loginLockout = 30 * time.Second
	// loginLockoutMax caps the exponential growth so a stuck client recovers.
	loginLockoutMax = 15 * time.Minute
	// loginEntryTTL drops idle IPs so the map cannot grow without bound.
	loginEntryTTL = 24 * time.Hour
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		entries:  map[string]*loginAttempt{},
		failures: loginFailureLimit,
		cooldown: loginLockout,
		now:      time.Now,
	}
}

// allowed reports whether this client may attempt a login right now, and how
// long it must wait if not.
func (t *loginThrottle) allowed(ip string) (bool, time.Duration) {
	if t == nil {
		return true, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	e := t.entries[ip]
	if e == nil {
		return true, 0
	}
	if now := t.now(); e.lockedTill.After(now) {
		return false, e.lockedTill.Sub(now)
	}
	return true, 0
}

// fail records a wrong password and returns the cooldown that now applies.
func (t *loginThrottle) fail(ip string) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	e := t.entries[ip]
	if e == nil {
		e = &loginAttempt{}
		t.entries[ip] = e
	}
	now := t.now()
	e.lastSeen = now
	e.fails++
	if e.fails < t.failures {
		return 0
	}
	// 5th failure = 30s, 6th = 60s, 7th = 120s ... capped.
	extra := e.fails - t.failures
	wait := t.cooldown
	for i := 0; i < extra; i++ {
		wait *= 2
		if wait >= loginLockoutMax {
			wait = loginLockoutMax
			break
		}
	}
	e.lockedTill = now.Add(wait)
	return wait
}

// succeed clears the counter for this client.
func (t *loginThrottle) succeed(ip string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, ip)
}

// pruneLocked drops entries that have been idle past the TTL.
func (t *loginThrottle) pruneLocked() {
	if len(t.entries) < 128 {
		return
	}
	cutoff := t.now().Add(-loginEntryTTL)
	for ip, e := range t.entries {
		if e.lastSeen.Before(cutoff) && !e.lockedTill.After(t.now()) {
			delete(t.entries, ip)
		}
	}
}

// loginThrottleFor derives the throttle key for a request.
//
// RemoteAddr is used, not X-Forwarded-For: the gateway is the TLS/edge in the
// deployments this ships for, and trusting a client-supplied forwarding header
// would let an attacker bypass the limit by rotating it. A reverse-proxy
// deployment must enforce its own limit at the edge.
func loginThrottleKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return strings.Trim(host, "[]")
}
