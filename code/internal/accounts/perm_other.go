//go:build !windows

package accounts

import (
	"fmt"
	"os"
)

// hardenPlatform enforces owner-only permissions on POSIX systems.
//
// writeJSONAtomic already passes 0600, but that only applies at creation: an
// existing file keeps whatever mode it had, and a directory keeps its 0755.
// Re-applying the mode here makes the guarantee hold for both, and for files
// created by an older build.
func hardenPlatform(path string, isDir bool) error {
	if path == "" {
		return nil
	}
	mode := os.FileMode(0o600)
	if isDir {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("harden %s: chmod: %w", path, err)
	}
	return nil
}
