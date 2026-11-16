package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenACLMarkers are the entries that must never appear on a credential
// path: they mean another local account can read (or rewrite) the tokens.
var forbiddenACLMarkers = []string{
	"BUILTIN\\Users:",
	"Everyone:",
	"Authenticated Users:",
}

// TestAccountFileIsNotWorldReadable pins the credential-file permission fix.
//
// The bug: os.WriteFile(path, data, 0o600) is advisory on Windows. The created
// file inherited BUILTIN\Users:(RX) and NT AUTHORITY\Authenticated Users:(M)
// from its parent, so every other local account could read the access and
// refresh tokens and any authenticated process could rewrite them. Verified on
// the repository checkout itself:
//
//	> icacls accounts\01a1df2a-....json
//	... BUILTIN\Users:(I)(RX)   NT AUTHORITY\Authenticated Users:(I)(M)
func TestAccountFileIsNotWorldReadable(t *testing.T) {
	dir := sharedDir(t)

	// Control: a plain 0600 write must inherit the permissive parent. Without
	// this, the assertion below could pass because the volume happens to be
	// locked down (Go's t.TempDir does exactly that on Windows), which would
	// make the test measure nothing.
	control := filepath.Join(dir, "control.json")
	if err := os.WriteFile(control, []byte("{\"accessToken\":\"dt-x\"}"), 0o600); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	if !hasForbiddenACL(t, control) {
		t.Fatal("control file did not inherit a permissive ACL; " +
			"the test cannot distinguish a fix from the volume default")
	}
	_ = os.Remove(control)

	acc, err := NewAccount(map[string]any{
		"uid": "perm-uid-0001", "realm": "cn", "domain": "qoder.com.cn",
		"accessToken": "dt-perm-secret", "refreshToken": "drt-perm-secret",
		"enabled": true,
	}, "")
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if err := acc.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(dir, "perm-uid-0001.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	// Sanity: the secret really is in the file, so a passing permission check
	// means something.
	if !strings.Contains(string(raw), "dt-perm-secret") {
		t.Fatal("saved file does not contain the token; test is not testing anything")
	}
	if hasForbiddenACL(t, path) {
		t.Errorf("credential file is still readable by other local accounts:\n%s",
			strings.Join(windowsACL(t, path), "\n"))
	}
}

// TestAccountsDirIsHardenedByPool pins that loading the pool tightens the
// directory itself. A permissive parent re-grants read access to every file
// written afterwards, so hardening files alone is not enough.
func TestAccountsDirIsHardenedByPool(t *testing.T) {
	dir := sharedDir(t)
	if !hasForbiddenACL(t, dir) {
		t.Fatal("shared dir is not permissive; test cannot prove the fix")
	}

	if _, err := NewPool(dir, nil); err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if hasForbiddenACL(t, dir) {
		t.Errorf("accounts dir is still shared:\n%s", strings.Join(windowsACL(t, dir), "\n"))
	}
}

// TestAccountFilePermissionSurvivesUpdate pins that an overwrite (token
// rotation) does not widen the ACL again.
func TestAccountFilePermissionSurvivesUpdate(t *testing.T) {
	dir := sharedDir(t)
	acc, err := NewAccount(map[string]any{
		"uid": "perm-uid-0002", "realm": "cn", "accessToken": "dt-first",
		"enabled": true,
	}, "")
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if err := acc.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	acc.SetTokens("dt-second", "drt-second", 0)
	if err := acc.Save(dir); err != nil {
		t.Fatalf("Save after rotate: %v", err)
	}

	path := filepath.Join(dir, "perm-uid-0002.json")
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "dt-second") {
		t.Fatal("rotated token was not persisted")
	}
	if hasForbiddenACL(t, path) {
		t.Errorf("rotation widened the ACL:\n%s", strings.Join(windowsACL(t, path), "\n"))
	}
}

// TestWriteJSONAtomicHardensTempFile pins that no temp file survives and the
// written document is intact.
func TestWriteJSONAtomicHardensTempFile(t *testing.T) {
	dir := sharedDir(t)
	path := filepath.Join(dir, "probe.json")
	if err := writeJSONAtomic(path, map[string]any{"accessToken": "dt-x"}); err != nil {
		t.Fatalf("writeJSONAtomic: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	var got map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("written file is not JSON: %v", err)
	}
	if got["accessToken"] != "dt-x" {
		t.Errorf("accessToken = %v", got["accessToken"])
	}
}

// sharedDir returns a temp directory that reproduces the inherited-ACL
// situation described in the bug, on any machine.
//
// t.TempDir() alone is not enough: Go creates it with an owner-only ACL on
// Windows, so a 0600 file inside it looks protected even without the fix. The
// extra subdirectory is explicitly granted inheritable read access for
// BUILTIN\Users, which is what a normal checkout or a domain machine has.
func sharedDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "accounts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	grantUsersReadable(t, dir)
	return dir
}

// hasForbiddenACL reports whether any ACL entry grants access to a group that
// should not see credentials.
func hasForbiddenACL(t *testing.T, path string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		// Off Windows the mode bits are the control: group/other must be zero.
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		return fi.Mode().Perm()&0o077 != 0
	}
	for _, entry := range windowsACL(t, path) {
		for _, bad := range forbiddenACLMarkers {
			if strings.Contains(entry, bad) {
				return true
			}
		}
	}
	return false
}
