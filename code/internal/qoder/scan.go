package qoder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// ScannedCredential is one credential found on this machine.
type ScannedCredential struct {
	Path      string `json:"path"`
	Realm     string `json:"realm"`
	RealmName string `json:"realmName"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Valid     bool   `json:"valid"`
	Reason    string `json:"reason,omitempty"`
	// Credential is set when Valid.
	Credential *Credential `json:"-"`
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
	cred.UID = uid
	cred.Nickname = firstNonEmpty(asString(data["nickname"]), asString(data["name"]))
	cred.Valid = true
	cred.Credential = &Credential{
		UID:          uid,
		Realm:        realm,
		AccessToken:  token,
		RefreshToken: firstNonEmpty(asString(data["refreshToken"]), asString(data["refresh_token"])),
		ExpiresAt: normalizeEpoch(firstNonEmpty(
			asString(data["expiresAt"]), asString(data["expires_at"]))),
	}
	return cred, true
}

// CLIAuthDirs returns the official CLI credential directories to scan.
func CLIAuthDirs(realm string, vpc *string) []string {
	cfg, err := RealmConfigFor(realm, vpc)
	if err != nil {
		return nil
	}
	root, err := os.UserHomeDir()
	if err != nil || root == "" {
		return nil
	}
	base := filepath.Join(root, cfg.HomeDir)
	var out []string
	if cfg.VPCInstance != "" {
		out = append(out, filepath.Join(base, ".vpc", cfg.VPCInstance, ".auth"))
	}
	return append(out, filepath.Join(base, ".auth"))
}
