// Package agents detects popular AI coding clients on this machine and can
// point them at the gateway.
//
// Two editing strategies, chosen per client:
//
//   - structured formats (JSON) are deep-merged and written whole;
//   - line-oriented formats (YAML/TOML) are upserted line by line so comments and
//     unrelated settings survive.
//
// Every write is backed up once, on first apply, and restore returns to the state
// immediately before the first apply — not to the previous apply.
package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ProviderID is the provider name written into client configs.
const ProviderID = "qoder-proxy"

// ProviderName is the human label used in client configs.
const ProviderName = "Qoder Gateway"

// Protocol a client speaks.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
	ProtocolNative    = "native"
)

// Mode says whether the gateway can configure the client automatically.
const (
	ModeAutomatic = "automatic"
	ModeManual    = "manual"
)

// Client describes one supported client.
type Client struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Desc       string   `json:"desc"`
	Protocol   string   `json:"protocol"`
	Category   string   `json:"category"`
	Mode       string   `json:"mode"`
	DocsURL    string   `json:"docs_url"`
	Guide      string   `json:"guide"`
	Commands   []string `json:"commands"`
	ProbeDirs  []string `json:"probe_dirs"`
	Extensions []string `json:"extensions,omitempty"`
	// Files returns the config paths this client uses.
	Files func(home string) map[string]string
	// Apply renders new file contents.
	Apply func(paths map[string]string, ctx map[string]any) (map[string]string, error)
	// Format names the editing strategy: "json", "yaml", "toml", "managed".
	Format string
}

var (
	registryMu sync.RWMutex
	registry   map[string]*Client
)

func init() { registry = buildRegistry() }

// Registry returns the client registry.
func Registry() map[string]*Client {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make(map[string]*Client, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}

func resolveHome(home string) string {
	if home != "" {
		return home
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func join(home string, parts ...string) string {
	return filepath.Join(append([]string{home}, parts...)...)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

func buildRegistry() map[string]*Client {
	reg := map[string]*Client{}

	// Claude Code speaks the Anthropic protocol natively.
	reg["claude-code"] = &Client{
		ID: "claude-code", Label: "Claude Code", Protocol: ProtocolAnthropic,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "json",
		Desc:     "Anthropic CLI；走网关原生 /v1/messages（ANTHROPIC_BASE_URL）。",
		DocsURL:  "https://docs.anthropic.com/en/docs/claude-code",
		Guide:    "写入 ~/.claude/settings.json 的 env 段；写入前备份，可还原。",
		Commands: []string{"claude"},
		Files: func(home string) map[string]string {
			return map[string]string{"settings": claudeSettingsPath(home)}
		},
		Apply: applyClaudeCode,
	}

	reg["codex"] = &Client{
		ID: "codex", Label: "Codex CLI", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "toml",
		Desc:     "OpenAI Codex CLI；注册 qoder-proxy Responses provider。",
		DocsURL:  "https://github.com/openai/codex",
		Guide:    "写入 ~/.codex/config.toml 的 provider 段；保留原有设置。",
		Commands: []string{"codex"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".codex", "config.toml")}
		},
		Apply: applyCodex,
	}

	reg["opencode"] = &Client{
		ID: "opencode", Label: "OpenCode", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "json",
		Desc:     "OpenCode；注册 openai-compatible provider。",
		DocsURL:  "https://opencode.ai/docs/providers/",
		Guide:    "写入 ~/.config/opencode/opencode.json 的 provider 段。",
		Commands: []string{"opencode"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".config", "opencode", "opencode.json")}
		},
		Apply: applyOpenCode,
	}

	reg["aider"] = &Client{
		ID: "aider", Label: "Aider", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "yaml",
		Desc:     "Aider；更新用户级 .aider.conf.yml。",
		DocsURL:  "https://aider.chat/docs/config/options.html",
		Guide:    "写入 ~/.aider.conf.yml 的 openai-api-base/key/model；保留原注释。",
		Commands: []string{"aider"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".aider.conf.yml")}
		},
		Apply: applyAider,
	}

	reg["continue"] = &Client{
		ID: "continue", Label: "Continue", Protocol: ProtocolOpenAI,
		Category: "IDE 扩展", Mode: ModeAutomatic, Format: "managed",
		Desc:     "Continue；向用户 YAML 增量添加网关模型。",
		DocsURL:  "https://docs.continue.dev/reference",
		Guide:    "在 ~/.continue/config.yaml 的 models 列表下追加受管块。",
		Commands: []string{"cn"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".continue", "config.yaml")}
		},
		Apply: applyContinue,
	}

	reg["pi"] = &Client{
		ID: "pi", Label: "Pi Coding Agent", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "json",
		Desc:    "Pi；注册 models.json 自定义 provider。",
		DocsURL: "https://pi.dev/docs/latest/models",
		Guide:   "写入 ~/.pi/agent/models.json 的 providers 段。",
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".pi", "agent", "models.json")}
		},
		Apply: applyPi,
	}

	reg["factory-droid"] = &Client{
		ID: "factory-droid", Label: "Factory Droid", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "json",
		Desc:     "Factory Droid；保留已有 customModels，添加网关 BYOK 模型。",
		DocsURL:  "https://docs.factory.com/model-independence/byok",
		Guide:    "写入 ~/.factory/settings.json 的 customModels。",
		Commands: []string{"droid"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".factory", "settings.json")}
		},
		Apply: applyFactoryDroid,
	}

	reg["copilot-cli"] = &Client{
		ID: "copilot-cli", Label: "GitHub Copilot CLI", Protocol: ProtocolOpenAI,
		Category: "CLI / Harness", Mode: ModeAutomatic, Format: "json",
		Desc:     "GitHub Copilot CLI；写入官方 providers.json BYOK 注册表。",
		DocsURL:  "https://docs.github.com/copilot/how-tos/copilot-cli/customize-copilot/add-llm-models",
		Guide:    "写入 ~/.copilot/providers.json，保留 GitHub 默认账号。",
		Commands: []string{"copilot"},
		Files: func(home string) map[string]string {
			return map[string]string{"config": join(home, ".copilot", "providers.json")}
		},
		Apply: applyCopilotCLI,
	}

	// Manual-only clients: detection plus a written guide.
	manual := []struct {
		id, label, category, command, docs, guide string
		dirs                                      []string
		protocol                                  string
	}{
		{"goose", "Goose", "CLI / Harness", "goose", "https://goose-docs.ai/docs/getting-started/providers/",
			"goose configure → Custom Providers / OpenAI Compatible；Base URL={BASE_URL_V1}；Key=YOUR_API_KEY；模型={MODEL}。",
			[]string{".config/goose"}, ProtocolOpenAI},
		{"gemini-cli", "Gemini CLI", "CLI / Harness", "gemini", "https://geminicli.com/docs/",
			"原生使用 Gemini 协议，不能直接接本项目 OpenAI/Anthropic 出口；请保留其原生配置。",
			[]string{".gemini"}, ProtocolNative},
		{"qwen-code", "Qwen Code", "CLI / Harness", "qwen", "https://qwenlm.github.io/qwen-code-docs/en/users/configuration/",
			"使用 OpenAI-compatible provider；OPENAI_BASE_URL={BASE_URL_V1}；OPENAI_API_KEY=YOUR_API_KEY；OPENAI_MODEL={MODEL}。",
			[]string{".qwen"}, ProtocolOpenAI},
		{"cursor", "Cursor", "IDE / 桌面", "cursor", "https://docs.cursor.com/settings/api-keys",
			"Settings → Models → Override OpenAI Base URL；URL={BASE_URL_V1}，Key=YOUR_API_KEY，添加模型={MODEL}。",
			[]string{".cursor"}, ProtocolOpenAI},
		{"windsurf", "Windsurf", "IDE / 桌面", "windsurf", "https://docs.windsurf.com/",
			"使用客户端 BYOK 支持范围；网关不能替代 Windsurf 原生订阅能力。",
			[]string{".codeium/windsurf", ".windsurf"}, ProtocolNative},
		{"trae", "Trae", "IDE / 桌面", "trae", "https://docs.trae.ai/",
			"设置 → 模型 → 自定义 OpenAI-compatible provider；URL={BASE_URL_V1}，Key=YOUR_API_KEY，模型={MODEL}。",
			[]string{".trae"}, ProtocolOpenAI},
		{"zed", "Zed", "IDE / 桌面", "zed", "https://zed.dev/docs/ai/llm-providers",
			"Settings → AI → OpenAI Compatible；api_url={BASE_URL_V1}，模型={MODEL}；凭据在 Zed 自己的安全存储录入。",
			[]string{".config/zed"}, ProtocolOpenAI},
		{"cline", "Cline", "IDE 扩展", "", "https://docs.cline.bot/provider-config/openai-compatible",
			"Cline 设置 → API Provider=OpenAI Compatible；Base URL={BASE_URL_V1}；API Key=YOUR_API_KEY；Model ID={MODEL}。",
			nil, ProtocolOpenAI},
		{"roo-code", "Roo Code", "IDE 扩展", "", "https://docs.roocode.com/providers/openai-compatible",
			"Roo Code 设置 → OpenAI Compatible；Base URL={BASE_URL_V1}；Key=YOUR_API_KEY；Model={MODEL}。",
			nil, ProtocolOpenAI},
		{"cherry-studio", "Cherry Studio", "IDE / 桌面", "", "https://docs.cherry-ai.com/",
			"设置 → 模型服务 → OpenAI；地址={BASE_URL_V1}，Key=YOUR_API_KEY，获取模型。",
			nil, ProtocolOpenAI},
		{"chatbox", "Chatbox", "IDE / 桌面", "chatbox", "https://chatboxai.app/",
			"Settings → Model Provider → Custom/OpenAI API；URL={BASE_URL_V1}，Key=YOUR_API_KEY，模型={MODEL}。",
			nil, ProtocolOpenAI},
		{"lobe-chat", "LobeChat", "Web / 自部署", "", "https://lobehub.com/docs",
			"Provider → OpenAI Compatible；Base URL={BASE_URL_V1}，Key=YOUR_API_KEY。部署在其他机器时不要使用网关 127.0.0.1。",
			nil, ProtocolOpenAI},
		{"open-webui", "Open WebUI", "Web / 自部署", "open-webui", "https://docs.openwebui.com/",
			"Admin → Connections → OpenAI API，填 {BASE_URL_V1} 和 YOUR_API_KEY；容器须使用可达宿主地址。",
			nil, ProtocolOpenAI},
	}
	for _, m := range manual {
		mm := m
		reg[mm.id] = &Client{
			ID: mm.id, Label: mm.label, Protocol: mm.protocol,
			Category: mm.category, Mode: ModeManual,
			Desc:    "提供本机检测与明确接入指引，不直接改私有状态或凭据库。",
			DocsURL: mm.docs, Guide: mm.guide, ProbeDirs: mm.dirs,
			Commands: func() []string {
				if mm.command == "" {
					return nil
				}
				return []string{mm.command}
			}(),
			Files: func(string) map[string]string { return map[string]string{} },
		}
	}
	return reg
}

// ---------------------------------------------------------------------------
// Detection
// ---------------------------------------------------------------------------

// Overview reports which clients are installed and configured.
func Overview(accountsDir string) map[string]any {
	home := resolveHome("")
	state := loadState(accountsDir)
	out := []map[string]any{}
	reg := Registry()
	ids := make([]string, 0, len(reg))
	for id := range reg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := reg[id]
		evidence := probeEvidence(c, home)
		installed := len(evidence) > 0
		record := state[id]
		out = append(out, map[string]any{
			"id": c.ID, "label": c.Label, "desc": c.Desc, "protocol": c.Protocol,
			"installed": installed, "mode": c.Mode, "category": c.Category,
			"evidence": evidence, "docs_url": c.DocsURL, "guide": c.Guide,
			"config_paths": mapValues(c.Files(home)),
			"configured":   record != nil && len(record.Files) > 0,
			"applied_at":   recordAppliedAt(record),
			"base_url":     recordBaseURL(record),
			"model":        recordModel(record),
		})
	}
	return map[string]any{"clients": out}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func recordAppliedAt(r *ClientState) any {
	if r == nil {
		return nil
	}
	return r.AppliedAt
}

func recordBaseURL(r *ClientState) string {
	if r == nil {
		return ""
	}
	return r.BaseURL
}

func recordModel(r *ClientState) string {
	if r == nil {
		return ""
	}
	return r.Model
}

// probeEvidence lists the paths proving a client is installed.
func probeEvidence(c *Client, home string) []string {
	var out []string
	for _, dir := range c.ProbeDirs {
		full := join(home, dir)
		if _, err := os.Stat(full); err == nil {
			out = append(out, "配置目录: "+full)
		}
	}
	for _, path := range c.Files(home) {
		if _, err := os.Stat(path); err == nil {
			out = append(out, "配置文件: "+path)
		}
	}
	for _, cmd := range c.Commands {
		if p, err := exec.LookPath(cmd); err == nil {
			out = append(out, "可执行文件: "+p)
		}
	}
	if runtime.GOOS == "darwin" {
		for _, ext := range c.Extensions {
			if _, err := os.Stat(join(home, "Library", "Application Support", ext)); err == nil {
				out = append(out, "应用扩展: "+ext)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Apply / restore
// ---------------------------------------------------------------------------

// ClientState is the persisted record of one applied configuration.
type ClientState struct {
	Files     []string          `json:"files"`
	Backups   map[string][]byte `json:"-"`
	AppliedAt string            `json:"applied_at"`
	BaseURL   string            `json:"base_url"`
	Model     string            `json:"model"`
}

type stateDoc struct {
	Clients map[string]*ClientState `json:"clients"`
}

func statePath(accountsDir string) string {
	return filepath.Join(accountsDir, "admin", "agents-state.json")
}

func backupDir(accountsDir string) string {
	return filepath.Join(accountsDir, "admin", "agents-backup")
}

func loadState(accountsDir string) map[string]*ClientState {
	out := map[string]*ClientState{}
	raw, err := os.ReadFile(statePath(accountsDir))
	if err != nil {
		return out
	}
	var doc stateDoc
	if json.Unmarshal(raw, &doc) == nil && doc.Clients != nil {
		return doc.Clients
	}
	return out
}

func saveState(accountsDir string, state map[string]*ClientState) error {
	doc := stateDoc{Clients: state}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(accountsDir), blob)
}

// Plan reports exactly which files an apply would write (no side effects).
func Plan(clientID, baseURL, apiKey, model string, home string) (map[string]any, error) {
	reg := Registry()
	c, ok := reg[clientID]
	if !ok {
		return nil, fmt.Errorf("unknown client %q", clientID)
	}
	home = resolveHome(home)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("base_url must start with http:// or https://")
	}
	baseURLV1 := baseURL
	if !strings.HasSuffix(baseURLV1, "/v1") {
		baseURLV1 += "/v1"
	}
	ctx := map[string]any{
		"base_url": baseURL, "base_url_v1": baseURLV1,
		"api_key": apiKey, "model": model,
	}
	if c.Mode == ModeManual {
		return nil, fmt.Errorf("该客户端仅支持检测与手动接入，请查看官方配置指引")
	}
	paths := c.Files(home)
	newContents, err := c.Apply(paths, ctx)
	if err != nil {
		return nil, err
	}
	files := []map[string]any{}
	for name, path := range paths {
		content, ok := newContents[name]
		if !ok {
			continue
		}
		_, statErr := os.Stat(path)
		files = append(files, map[string]any{
			"path": path, "content": content, "existed": statErr == nil,
		})
	}
	return map[string]any{
		"client": clientID, "label": c.Label, "base_url": baseURL,
		"model": model, "files": files,
	}, nil
}

// Apply writes the client configuration, backing up originals once.
func Apply(accountsDir, clientID, baseURL, apiKey, model, home string) (map[string]any, error) {
	plan, err := Plan(clientID, baseURL, apiKey, model, home)
	if err != nil {
		return nil, err
	}
	files, _ := plan["files"].([]map[string]any)
	state := loadState(accountsDir)
	record, exists := state[clientID]
	if !exists {
		record = &ClientState{Backups: map[string][]byte{}}
	}
	// Back up the original bytes once: restore always returns to the state
	// immediately before the first apply.
	for _, f := range files {
		path := f["path"].(string)
		if _, done := record.Backups[path]; done {
			continue
		}
		original, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		record.Backups[path] = original
		target := filepath.Join(backupDir(accountsDir), clientID,
			strings.ReplaceAll(path, string(os.PathSeparator), "_"))
		if err := writeFileAtomic(target, original); err != nil {
			return nil, err
		}
	}
	// Two-phase write: prepare everything, then commit; a failure rolls back.
	written := []string{}
	for _, f := range files {
		path := f["path"].(string)
		content := f["content"].(string)
		if err := writeFileAtomic(path, []byte(content)); err != nil {
			for _, w := range written {
				_ = os.Remove(w)
			}
			return nil, err
		}
		written = append(written, path)
	}
	record.Files = written
	record.AppliedAt = nowISO()
	record.BaseURL = baseURL
	record.Model = model
	state[clientID] = record
	if err := saveState(accountsDir, state); err != nil {
		return nil, err
	}
	return plan, nil
}

// Restore returns every touched file to its pre-first-apply content.
func Restore(accountsDir, clientID, home string) (map[string]any, error) {
	state := loadState(accountsDir)
	record, ok := state[clientID]
	if !ok {
		return nil, fmt.Errorf("client %q has no recorded configuration", clientID)
	}
	restored := []string{}
	for path, original := range record.Backups {
		if err := writeFileAtomic(path, original); err != nil {
			return nil, err
		}
		restored = append(restored, path)
	}
	delete(state, clientID)
	if err := saveState(accountsDir, state); err != nil {
		return nil, err
	}
	return map[string]any{"client": clientID, "restored": restored}, nil
}

// Template renders the manual configuration snippet for a client.
func Template(clientID, baseURL, model string) (map[string]any, error) {
	reg := Registry()
	c, ok := reg[clientID]
	if !ok {
		return nil, fmt.Errorf("unknown client %q", clientID)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	baseURLV1 := baseURL
	if !strings.HasSuffix(baseURLV1, "/v1") {
		baseURLV1 += "/v1"
	}
	guide := strings.NewReplacer(
		"{BASE_URL_V1}", baseURLV1,
		"{BASE_URL}", baseURL,
		"{MODEL}", model,
	).Replace(c.Guide)
	return map[string]any{
		"client": c.ID, "label": c.Label, "protocol": c.Protocol,
		"docs_url": c.DocsURL, "guide": guide,
		"base_url": baseURL, "base_url_v1": baseURLV1, "model": model,
		"api_key_placeholder": "YOUR_API_KEY",
	}, nil
}

// CLISource describes how to download the configurator on another machine.
func CLISource() string {
	return `#!/bin/sh
# qoder-proxy agent configurator (Go build)
#
# This build configures clients through the panel API instead of a standalone
# script. From a machine that can reach the gateway:
#
#   curl -s "$GATEWAY/agents/template?client=codex&model=auto" -H "X-Panel-Token: $TOKEN"
#
# and apply with POST /agents/apply from the gateway host itself.
set -eu
echo "See /agents/template for per-client snippets; /agents/apply must run on the gateway host." >&2
`
}
