package logx

import (
	"os"
	"strings"
	"testing"
)

// TestLogRedactsBeforeEverySink pins that a token handed to the logger cannot
// reach the ring, the file, or stderr.
//
// This is the enforcement point: redact.go has unit tests for the patterns,
// but only this test proves the logger actually calls them, and that all three
// sinks see the same scrubbed text.
func TestLogRedactsBeforeEverySink(t *testing.T) {
	var echo strings.Builder
	dir := t.TempDir()
	l := New(Options{Dir: dir, FileName: "test.log", RingSize: 16, Echo: &echo})
	defer l.Close()

	const secret = "dt-9f8e7d6c5b4a3210"
	l.Warn("upstream", "request failed with Authorization: Bearer %s", secret)

	// Ring (what /logs returns).
	entries := l.Query(Query{})
	if len(entries) != 1 {
		t.Fatalf("ring has %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].Msg, secret) {
		t.Errorf("ring leaked the token: %q", entries[0].Msg)
	}
	// The tag and level must survive so the panel can still filter.
	if entries[0].Tag != "upstream" || entries[0].Level != LevelWarn {
		t.Errorf("metadata lost: tag=%q level=%q", entries[0].Tag, entries[0].Level)
	}

	// Echo (stderr in production).
	if strings.Contains(echo.String(), secret) {
		t.Errorf("echo leaked the token: %q", echo.String())
	}

	// File.
	raw, err := os.ReadFile(l.FilePath())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("file leaked the token: %q", string(raw))
	}
}
