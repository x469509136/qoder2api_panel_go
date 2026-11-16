package server

import (
	"strings"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/guards"
	"github.com/xzs/bridge_qoder/internal/protocol"
	"github.com/xzs/bridge_qoder/internal/settings"
)

func protocolImageStatus() map[string]any { return protocol.ImagePreflightStatus() }

func guardsSnapshot() map[string]any {
	out := map[string]any{
		"mode":     guards.ToolEchoMode(),
		"hold_max": guards.HoldMaxChars,
	}
	for k, v := range guards.RecoverySnapshot() {
		out[k] = v
	}
	return out
}

// measureClockSkew reads the upstream Date header (no auth, HEAD only).
func measureClockSkew() (*int64, error) {
	return accounts.UpstreamClockSkew(4 * time.Second)
}

// exportSettingsView renders the settings payload the panel consumes.
func (s *Server) exportSettingsView(reveal bool) map[string]any {
	data := s.settings.Load()
	keys := s.settings.APIKeys()
	outKeys := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		key := k.Key
		if !reveal {
			key = maskKey(key)
		}
		outKeys = append(outKeys, map[string]any{
			"id": k.ID, "name": k.Name, "key": key, "realm": k.Realm,
			"enabled": k.Enabled, "models": k.Models, "note": k.Note,
			"created_at": k.CreatedAt, "anthropic_thinking": k.AnthropicThinking,
			"account_ids": k.AccountIDs, "account_groups": k.AccountGroups,
		})
	}
	view := map[string]any{
		"api_keys":               outKeys,
		"banned_models":          s.settings.BannedModels(),
		"limits":                 s.settings.LimitsData(),
		"model_context":          s.settings.ModelContext(),
		"tool_echo_mode":         s.settings.ToolEchoMode(),
		"anthropic_thinking":     s.settings.AnthropicThinking(),
		"web_tools":              s.settings.WebToolsEnabled(),
		"auth_disabled":          s.settings.AuthDisabled(),
		"panel_password_default": s.settings.PanelPasswordIsDefault(),
		"realm":                  s.currentRealm(),
		"version":                s.cfg.Version,
		"api_key_required":       s.authRequired(),
	}
	if v, ok := data["model_context"]; ok {
		view["model_context_raw"] = v
	}
	return view
}

func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

// keyEntryView is the panel's key row.
type keyEntryView = settings.KeyEntry
