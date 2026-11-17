package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogRedactsBeforePersisting pins that the scheduler's own log is a
// scrubbed sink.
//
// These lines quote upstream error bodies verbatim ("… 失败: HTTP 401
// {\"code\":\"TOKEN_EXPIRE\"…}") and take a path no other diagnostic takes:
// they are stored in accounts/scheduler/state.json on disk, then replayed by
// /scheduler and /health. If a credential ever appears in an upstream body,
// this is the one sink that logx.Redact would not otherwise cover — the
// persisted copy would outlive the log rotation and be readable from the panel.
func TestLogRedactsBeforePersisting(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir, nil)

	s.Log("刷新失败: HTTP 401 {\"message\":\"token is not active\"," +
		"\"accessToken\":\"dt-LEAKEDACCESS0000000000\"}")

	raw, err := os.ReadFile(filepath.Join(dir, StateSubdir, StateFileName))
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "dt-LEAKEDACCESS0000000000") {
		t.Errorf("scheduler log persisted a raw token:\n%s", text)
	}
	// The diagnostic value must survive the scrub: the operator still needs to
	// see which account failed and why.
	if !strings.Contains(text, "token is not active") {
		t.Errorf("redaction removed the diagnostic message:\n%s", text)
	}
	if !strings.Contains(text, "accessToken") {
		t.Errorf("redaction removed the field name:\n%s", text)
	}
}
