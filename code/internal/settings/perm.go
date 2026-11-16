package settings

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// hardenPerm restricts access to a settings file (or its directory).
//
// settings.json holds panel_session_secret, the panel password digest and any
// API keys, so it needs the same protection as an account file. os.WriteFile's
// 0600 argument is advisory on Windows: the created file inherits
// BUILTIN\Users:(RX) from the parent and every other local account can read the
// signing secret, which is enough to mint a panel session.
func hardenPerm(path string, isDir bool) error {
	if path == "" {
		return nil
	}
	if runtime.GOOS != "windows" {
		mode := os.FileMode(0o600)
		if isDir {
			mode = 0o700
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("harden %s: chmod: %w", path, err)
		}
		return nil
	}
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
		grant = user + ":(OI)(CI)F"
	}
	// /inheritance:r keeps explicit ACEs by design, so group grants must be
	// removed by SID too. See accounts/perm_windows.go for the same reasoning.
	args := []string{path, "/inheritance:r",
		"/remove:g", "*S-1-1-0", // Everyone
		"/remove:g", "*S-1-5-11", // Authenticated Users
		"/remove:g", "*S-1-5-32-545", // BUILTIN\Users
		"/grant:r", grant,
		"/grant:r", "SYSTEM:(OI)(CI)F",
		"/grant:r", "Administrators:(OI)(CI)F",
	}
	if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("harden %s: icacls: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
