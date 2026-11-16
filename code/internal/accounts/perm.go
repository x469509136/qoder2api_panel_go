package accounts

// hardenCredentialPath restricts who may read a credential path.
//
// On POSIX systems the file mode (0600) set by writeJSONAtomic is the control.
// Windows ignores the mode bits entirely: os.WriteFile(path, data, 0o600)
// creates a file whose ACL says BUILTIN\Users:(RX) and
// NT AUTHORITY\Authenticated Users:(M) — every other local account can read
// the access and refresh tokens, and any authenticated process can rewrite them.
//
// This file is the portable entry point; the platform-specific work lives in
// perm_windows.go and perm_other.go so the POSIX build stays dependency-free.
//
// Errors are returned so the caller can surface them instead of silently
// believing the directory is protected.
func hardenCredentialPath(path string, isDir bool) error {
	return hardenPlatform(path, isDir)
}
