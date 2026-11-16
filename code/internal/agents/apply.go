package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// writeFileAtomic writes via a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".qoder-proxy.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nowISO() string { return time.Now().Format("2006-01-02 15:04:05") }

func readText(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func loadJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func dumpJSON(v any) (string, error) {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(blob) + "\n", nil
}

// deepMerge merges patch into base, recursing into nested maps.
func deepMerge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if patchMap, ok := v.(map[string]any); ok {
			if baseMap, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(baseMap, patchMap)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// managedBlock wraps a snippet in markers so it can be replaced or removed.
func managedBlock(text, name, body string) string {
	begin := "# qoder-proxy:" + name + ":begin"
	end := "# qoder-proxy:" + name + ":end"
	block := begin + "\n" + body + "\n" + end
	re := regexp.MustCompile(regexp.QuoteMeta(begin) + `(?s).*?` + regexp.QuoteMeta(end))
	if re.MatchString(text) {
		return re.ReplaceAllString(text, block)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + block + "\n"
}

// yamlUpsertInline sets a scalar top-level key, preserving comments elsewhere.
func yamlUpsertInline(text, key, value string) string {
	quoted, _ := json.Marshal(value)
	line := key + ": " + string(quoted)
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:.*$`)
	if re.MatchString(text) {
		return re.ReplaceAllString(text, line)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + line + "\n"
}

// tomlUpsertTable sets a table's keys, replacing an existing table body.
func tomlUpsertTable(text, table string, mapping map[string]string) string {
	lines := strings.Split(text, "\n")
	header := "[" + table + "]"
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sortStrings(keys)
	body := []string{header}
	for _, k := range keys {
		v, _ := json.Marshal(mapping[k])
		body = append(body, k+" = "+string(v))
	}
	if start < 0 {
		out := strings.TrimRight(text, "\n")
		if out != "" {
			out += "\n\n"
		}
		out += strings.Join(body, "\n") + "\n"
		return out
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "#") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, body...)
	if end < len(lines) {
		out = append(out, "")
		out = append(out, lines[end:]...)
	}
	return strings.Join(out, "\n")
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Per-client appliers
// ---------------------------------------------------------------------------

func claudeSettingsPath(home string) string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	return join(home, ".claude", "settings.json")
}

// applyClaudeCode points Claude Code at the gateway's Anthropic endpoint.
func applyClaudeCode(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["settings"]
	data, err := loadJSON(path)
	if err != nil {
		return nil, err
	}
	env, _ := data["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = ctx["base_url"]
	env["ANTHROPIC_AUTH_TOKEN"] = ctx["api_key"]
	if model, ok := ctx["model"].(string); ok && model != "" {
		env["ANTHROPIC_MODEL"] = model
	}
	data["env"] = env
	out, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{"settings": out}, nil
}

// applyCodex registers the gateway as a Responses provider.
func applyCodex(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	text := readText(path)
	text = tomlUpsertTable(text, "model_providers."+ProviderID, map[string]string{
		"name":     ProviderName,
		"base_url": ctx["base_url_v1"].(string),
		"wire_api": "responses",
		"env_key":  "QODER_PROXY_API_KEY",
	})
	model, _ := ctx["model"].(string)
	if model == "" {
		model = "auto"
	}
	out := tomlUpsertTopKey(text, "model_provider", ProviderID)
	out = tomlUpsertTopKey(out, "model", model)
	return map[string]string{"config": out}, nil
}

// tomlUpsertTopKey sets a top-level scalar, inserting before the first table.
func tomlUpsertTopKey(text, key, value string) string {
	quoted, _ := json.Marshal(value)
	lines := strings.Split(text, "\n")
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	for i, l := range lines {
		if re.MatchString(l) {
			lines[i] = key + " = " + string(quoted)
			return strings.Join(lines, "\n")
		}
	}
	insertAt := len(lines)
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "[") {
			insertAt = i
			break
		}
	}
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, key+" = "+string(quoted))
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\n")
}

// applyOpenCode registers an openai-compatible provider.
func applyOpenCode(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path)
	if err != nil {
		return nil, err
	}
	merged := deepMerge(data, map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			ProviderID: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": ProviderName,
				"options": map[string]any{
					"baseURL": ctx["base_url_v1"],
					"apiKey":  ctx["api_key"],
				},
				"models": map[string]any{
					defaultModel(ctx): map[string]any{"name": defaultModel(ctx)},
				},
			},
		},
	})
	out, err := dumpJSON(merged)
	if err != nil {
		return nil, err
	}
	return map[string]string{"config": out}, nil
}

func defaultModel(ctx map[string]any) string {
	if m, ok := ctx["model"].(string); ok && m != "" {
		return m
	}
	return "auto"
}

// applyAider sets the three OpenAI keys in its YAML config.
func applyAider(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	text := readText(path)
	text = yamlUpsertInline(text, "openai-api-base", ctx["base_url_v1"].(string))
	text = yamlUpsertInline(text, "openai-api-key", ctx["api_key"].(string))
	text = yamlUpsertInline(text, "model", "openai/"+defaultModel(ctx))
	return map[string]string{"config": text}, nil
}

// applyContinue appends a managed model entry under the models list.
func applyContinue(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	text := readText(path)
	entry, _ := json.Marshal(map[string]any{
		"name": ProviderName, "provider": "openai", "model": defaultModel(ctx),
		"apiBase": ctx["base_url_v1"], "apiKey": ctx["api_key"],
		"roles": []string{"chat", "edit", "apply"}, "useResponsesApi": false,
	})
	block := managedBlock(text, "continue-model", "  - "+string(entry))
	return map[string]string{"config": block}, nil
}

// applyPi registers a custom provider in models.json.
func applyPi(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path)
	if err != nil {
		return nil, err
	}
	merged := deepMerge(data, map[string]any{
		"providers": map[string]any{
			ProviderID: map[string]any{
				"baseUrl": ctx["base_url_v1"],
				"api":     "openai-completions",
				"apiKey":  ctx["api_key"],
				"models": []any{
					map[string]any{"id": defaultModel(ctx)},
				},
			},
		},
	})
	out, err := dumpJSON(merged)
	if err != nil {
		return nil, err
	}
	return map[string]string{"config": out}, nil
}

// applyFactoryDroid adds BYOK models without touching existing ones.
func applyFactoryDroid(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path)
	if err != nil {
		return nil, err
	}
	var kept []any
	if prev, ok := data["customModels"].([]any); ok {
		for _, item := range prev {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["displayName"].(string)
			if strings.HasPrefix(name, ProviderName+" · ") {
				continue
			}
			kept = append(kept, item)
		}
	}
	kept = append(kept, map[string]any{
		"model":       defaultModel(ctx),
		"displayName": ProviderName + " · " + defaultModel(ctx),
		"baseUrl":     ctx["base_url_v1"],
		"apiKey":      ctx["api_key"],
		"provider":    "generic-chat-completion-api",
	})
	data["customModels"] = kept
	out, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{"config": out}, nil
}

// applyCopilotCLI writes the official BYOK provider registry.
func applyCopilotCLI(paths map[string]string, ctx map[string]any) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path)
	if err != nil {
		return nil, err
	}
	var providers []any
	if prev, ok := data["providers"].([]any); ok {
		for _, item := range prev {
			m, ok := item.(map[string]any)
			if ok {
				if name, _ := m["name"].(string); name == ProviderID {
					continue
				}
			}
			providers = append(providers, item)
		}
	}
	providers = append(providers, map[string]any{
		"name": ProviderID, "type": "openai",
		"baseUrl": ctx["base_url_v1"], "apiKey": ctx["api_key"],
	})
	data["providers"] = providers
	var models []any
	if prev, ok := data["models"].([]any); ok {
		for _, item := range prev {
			if m, ok := item.(map[string]any); ok {
				if p, _ := m["provider"].(string); p == ProviderID {
					continue
				}
			}
			models = append(models, item)
		}
	}
	models = append(models, map[string]any{
		"id": defaultModel(ctx), "provider": ProviderID,
	})
	data["models"] = models
	out, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{"config": out}, nil
}
