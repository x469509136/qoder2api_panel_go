//go:build windows

package accounts

import (
	"fmt"
	"os/exec"
	"strings"
)

// hardenPlatform rewrites the Windows ACL of a credential path so only the
// current user, SYSTEM and Administrators can read it.
//
// icacls is used instead of golang.org/x/sys/windows because this project has
// a hard zero-dependency rule (go.mod has no require lines). icacls ships with
// every supported Windows version.
//
// The ACL is set with /inheritance:r to drop inherited ACEs first: the parent
// directory's "Users:(RX)" entry would otherwise re-grant read access no matter
// what we add afterwards.
func hardenPlatform(path string, isDir bool) error {
	if path == "" {
		return nil
	}
	// Current user name as DOMAIN\user, which is what icacls expects.
	whoami, err := exec.Command("whoami").Output()
	if err != nil {
		return fmt.Errorf("harden %s: whoami: %w", path, err)
	}
	user := strings.TrimSpace(string(whoami))
	if user == "" {
		return fmt.Errorf("harden %s: empty whoami result", path)
	}

	grant := user + ":F"
	if isDir {
		// (OI)(CI) makes the grant inherit to files created later, so a new
		// account file is protected the moment it is written.
		grant = user + ":(OI)(CI)F"
	}

	// /inheritance:r drops inherited ACEs but, deliberately, keeps explicit
	// ones — that is how icacls is specified. An attacker (or an earlier
	// installer) can leave an explicit "BUILTIN\Users:(R)" on the directory,
	// so the group grants have to be removed by name as well. Without this the
	// "shared directory" case stays readable even after hardening.
	args := []string{path, "/inheritance:r",
		"/remove:g", "*S-1-1-0", // Everyone
		"/remove:g", "*S-1-5-11", // Authenticated Users
		"/remove:g", "*S-1-5-32-545", // BUILTIN\Users
		"/grant:r", grant,
		"/grant:r", "SYSTEM:(OI)(CI)F",
		"/grant:r", "Administrators:(OI)(CI)F",
	}
	cmd := exec.Command("icacls", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harden %s: icacls: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
