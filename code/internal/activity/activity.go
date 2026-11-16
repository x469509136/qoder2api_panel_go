// Package activity records check-in and claim events.
//
// The account JSON only carries lastCheckin and the redemption codes, so there is
// no way to see which day, which account, what action and whether it worked. One
// line per event makes that visible.
package activity

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxBytes triggers a single .1 rotation.
	MaxBytes = 5 * 1024 * 1024
	// KeepDays bounds what the read API returns.
	KeepDays = 30
)

// Event is one recorded action.
type Event struct {
	Ts       float64 `json:"ts"`
	Kind     string  `json:"kind"`
	OK       bool    `json:"ok"`
	UID      string  `json:"uid"`
	Nickname string  `json:"nickname"`
	Realm    string  `json:"realm"`
	Detail   string  `json:"detail"`
	Amount   int     `json:"amount"`
}

var mu sync.Mutex

// Path returns usage/activity.jsonl.
func Path(usageDir string) string { return filepath.Join(usageDir, "activity.jsonl") }

// Record appends one event. Every error is swallowed: this log must never affect
// the action it describes.
func Record(usageDir, kind string, ok bool, uid, nickname, realm, detail string, amount int) {
	mu.Lock()
	defer mu.Unlock()
	path := Path(usageDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > MaxBytes {
		_ = os.Rename(path, path+".1")
	}
	if len(uid) > 8 {
		uid = uid[:8]
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	row := Event{
		Ts: float64(time.Now().Unix()), Kind: kind, OK: ok, UID: uid,
		Nickname: nickname, Realm: realm, Detail: detail, Amount: amount,
	}
	blob, err := json.Marshal(row)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(blob, '\n'))
	_ = f.Close()
}

// Recent returns the newest events first.
//
// Events written in the same millisecond are ordered by write order (later wins),
// otherwise a success/failure pair with an identical timestamp would flap.
func Recent(usageDir string, limit, days int) []Event {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if days <= 0 {
		days = KeepDays
	}
	cutoff := float64(time.Now().Unix() - int64(days*86400))
	f, err := os.Open(Path(usageDir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil || e.Ts < cutoff {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
