//go:build !windows

package accounts

import (
	"os"
	"testing"
)

// grantUsersReadable is a no-op off Windows: the POSIX control is the file
// mode, which writeJSONAtomic already narrows.
func grantUsersReadable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
}

// windowsACL is unreachable off Windows; callers use file modes instead.
func windowsACL(t *testing.T, path string) []string {
	t.Helper()
	return nil
}
