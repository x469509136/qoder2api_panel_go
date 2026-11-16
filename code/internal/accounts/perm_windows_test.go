//go:build windows

package accounts

import (
	"os/exec"
	"strings"
	"testing"
)

// grantUsersReadable adds an inheritable read grant for BUILTIN\Users on dir.
//
// The SID is used rather than the name so the command works on non-English
// Windows installs.
func grantUsersReadable(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)R").CombinedOutput()
	if err != nil {
		t.Fatalf("grant Users read on %s: %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
}

// windowsACL returns the icacls entries for a path.
func windowsACL(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("icacls", path).Output()
	if err != nil {
		t.Fatalf("icacls %s: %v", path, err)
	}
	var entries []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Successfully processed") {
			continue
		}
		// icacls pads the first line with the path; keep only ACE lines.
		if strings.Contains(line, ":(") {
			entries = append(entries, line)
		}
	}
	if len(entries) == 0 {
		t.Fatalf("icacls produced no ACEs for %s:\n%s", path, out)
	}
	return entries
}
