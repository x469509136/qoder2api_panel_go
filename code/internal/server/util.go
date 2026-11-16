package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/xzs/bridge_qoder/internal/sign"
)

func signQoderEncode(plain []byte) string { return sign.QoderEncode(plain) }

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// MaxOutLimit is one learned output-token cap.
type MaxOutLimit struct {
	Limit int     `json:"limit"`
	At    float64 `json:"at"`
}

var maxOutMu sync.Mutex

func (s *Server) maxOutLimitsPath() string {
	return filepath.Join(s.cfg.UsageDir, "max-output-limits.json")
}

func (s *Server) loadMaxOutLimits() map[string]MaxOutLimit {
	maxOutMu.Lock()
	defer maxOutMu.Unlock()
	out := map[string]MaxOutLimit{}
	raw, err := os.ReadFile(s.maxOutLimitsPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// learnMaxOutputLimit records a cap parsed from a 400 response.
func (s *Server) learnMaxOutputLimit(limit int, modelKey, realm string) {
	if limit <= 0 || modelKey == "" {
		return
	}
	maxOutMu.Lock()
	defer maxOutMu.Unlock()
	limits := map[string]MaxOutLimit{}
	if raw, err := os.ReadFile(s.maxOutLimitsPath()); err == nil {
		_ = json.Unmarshal(raw, &limits)
	}
	limits[modelKey+"|"+realm] = MaxOutLimit{Limit: limit, At: nowFloat()}
	blob, err := json.MarshalIndent(limits, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(s.maxOutLimitsPath(), blob)
}

// requestRealm resolves the exit for one request.
//
// Priority: an explicit ?realm= argument, then the realm bound to the API key,
// then the X-Realm header or ?realm= query, and finally "" (let the model-based
// detection decide).
func (s *Server) requestRealm(r *http.Request) string {
	if entry := s.keyEntry(r); entry != nil && entry.Realm != "" {
		return entry.Realm
	}
	if v := normalizeRealm(r.Header.Get("X-Realm")); v != "" {
		return v
	}
	if v := normalizeRealm(r.URL.Query().Get("realm")); v != "" {
		return v
	}
	return ""
}

func normalizeRealm(v string) string {
	switch lowerTrim(v) {
	case "cn":
		return "cn"
	case "intl":
		return "intl"
	}
	return ""
}

func lowerTrim(v string) string { return trimSpace(strings.ToLower(v)) }

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
