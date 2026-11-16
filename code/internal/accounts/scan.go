package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ScannedCredential is one credential found on this machine.
type ScannedCredential struct {
	Path      string   `json:"path"`
	Realm     string   `json:"realm"`
	RealmName string   `json:"realmName"`
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname"`
	Valid     bool     `json:"valid"`
	Reason    string   `json:"reason,omitempty"`
	Account   *Account `json:"-"`
}

// ScanDesktopCredentials looks for credentials the official client already
// stored on this machine.
//
// It never imports anything by itself: the caller reports the findings and a
// human decides. Silently adopting a login found on disk is exactly the wrong
// default on a shared machine.
func ScanDesktopCredentials() []ScannedCredential {
	var out []ScannedCredential
	seen := map[string]bool{}
	for _, realm := range []string{RealmCN, RealmINTL} {
		for _, dir := range CLIAuthDirs(realm, nil) {
			for _, name := range []string{"user.json", "user_info.json", "auth.json"} {
				path := filepath.Join(dir, name)
				cred, ok := readCredentialFile(path, realm)
				if !ok || seen[cred.UID] {
					continue
				}
				seen[cred.UID] = true
				out = append(out, cred)
			}
		}
	}
	// Desktop application data directories.
	for _, realm := range []string{RealmCN, RealmINTL} {
		cfg := GetRealmConfig(realm)
		appDir := os.Getenv("APPDATA")
		if runtime.GOOS == "darwin" {
			if home, err := os.UserHomeDir(); err == nil {
				appDir = filepath.Join(home, "Library", "Application Support")
			}
		} else if runtime.GOOS != "windows" {
			if home, err := os.UserHomeDir(); err == nil {
				appDir = filepath.Join(home, ".config")
			}
		}
		if appDir == "" {
			continue
		}
		root := filepath.Join(appDir, cfg.AppDir)
		for _, name := range []string{"user.json", "auth.json"} {
			path := filepath.Join(root, name)
			cred, ok := readCredentialFile(path, realm)
			if !ok || seen[cred.UID] {
				continue
			}
			seen[cred.UID] = true
			out = append(out, cred)
		}
	}
	return out
}

func readCredentialFile(path, realm string) (ScannedCredential, bool) {
	cred := ScannedCredential{Path: path, Realm: realm, RealmName: GetRealmConfig(realm).Name}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cred, false
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		cred.Reason = "unparseable credential file"
		return cred, true
	}
	token := firstNonEmpty(asString(data["accessToken"]), asString(data["access_token"]),
		asString(data["token"]))
	if token == "" {
		cred.Reason = "no access token in file"
		return cred, true
	}
	uid := firstNonEmpty(asString(data["uid"]), asString(data["id"]), asString(data["userId"]))
	acc, err := NewAccount(map[string]any{
		"uid":          uid,
		"nickname":     firstNonEmpty(asString(data["nickname"]), asString(data["name"])),
		"domain":       GetRealmConfig(realm).Domain,
		"realm":        realm,
		"accessToken":  token,
		"refreshToken": firstNonEmpty(asString(data["refreshToken"]), asString(data["refresh_token"])),
		"expiresAt":    normalizeEpoch(firstNonEmpty(asString(data["expiresAt"]), asString(data["expires_at"]))),
		"source":       "desktop-scan",
		"enabled":      true,
	}, "")
	if err != nil {
		cred.Reason = err.Error()
		return cred, true
	}
	cred.UID = acc.UID
	cred.Nickname = acc.Nickname
	cred.Valid = true
	cred.Account = acc
	return cred, true
}

// DesktopInstallDir returns the official client's install directory.
func DesktopInstallDir(realm string) string {
	cfg := GetRealmConfig(realm)
	if runtime.GOOS == "windows" {
		for _, env := range []string{"LOCALAPPDATA", "APPDATA"} {
			if base := os.Getenv(env); base != "" {
				candidate := filepath.Join(base, "Programs", cfg.AppDir)
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}
	return ""
}

// LooksLikeVPC reports whether a domain is an enterprise VPC subdomain.
func LooksLikeVPC(domain string) bool {
	return strings.HasSuffix(strings.ToLower(domain), "."+VPCDomainSuffix)
}
