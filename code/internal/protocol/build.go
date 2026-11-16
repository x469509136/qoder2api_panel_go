package protocol

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed baseprompt.json
var basepromptFS embed.FS

// ConversationIdentity is the per-conversation state carried across turns.
//
// The upstream usage panel groups requests by chat_record_id / business.id.
// Minting a new UUID per request splits one agent turn into dozens of "<1min
// conversations" and burns the daily request cap (code 110) almost at once.
type ConversationIdentity struct {
	ID       string
	WindowID string
	BeginAt  int64
}

// BuildContext carries everything needed to build an upstream body.
type BuildContext struct {
	// Account identity (may be nil for a representative request).
	UID      string
	UserType string
	// ModelKey is the resolved upstream key.
	ModelKey    string
	DisplayName string
	IsVL        bool
	IsReasoning bool
	MaxInput    int64
	// Realm selects catalog metadata.
	Realm string
	// Conversation is the session-level identity.
	Conversation *ConversationIdentity
	// Structured decides native tool_calls vs the textual envelope. It is set
	// by the caller so the decision happens exactly once.
	Structured bool
	// SystemPromptOverride replaces the template system prompt when the client
	// sent none.
	SystemPromptOverride string
	// DefaultSystemPrompt is the built-in default ("You are a helpful
	// assistant."); when the override equals it the template prompt is kept.
	DefaultSystemPrompt string
	// ContextOverride raises max_input_tokens (from X-QD-Context or settings).
	ContextOverride int64
	// MaxTokensClamp caps max_tokens at a learned upstream limit.
	MaxTokensClamp func(int) int
	// Effort is the normalized reasoning effort ("" = do not send).
	Effort string
	// CosyVersion is reported as business.version.
	CosyVersion string
	// KimiToolProtocol appends the Kimi tool protocol constraints.
	KimiToolProtocol bool
	// KeepTopLevelImages restores the legacy redundant image fields.
	KeepTopLevelImages bool
	// SessionID is the upstream session id chosen by the prefix cache.
	SessionID string
	// ToolResult settings.
	ToolResultKeep  int
	ToolResultScope string
	ToolResultTail  int
	// Images preprocessed stats (for logging only).
	OnImagePreflight func(count, optimized, codeBytes, afterBytes, codecMissing int)
}

var (
	basepromptOnce sync.Once
	basepromptData map[string]any
	basepromptErr  error
)

// BasePrompt returns the official request body skeleton.
//
// It is read from assets/baseprompt.json next to the executable when present so
// operators can keep it in step with the official client without rebuilding.
func BasePrompt() (map[string]any, error) {
	basepromptOnce.Do(func() {
		basepromptData, basepromptErr = loadBasePrompt()
	})
	if basepromptErr != nil {
		return nil, basepromptErr
	}
	return deepCopyMap(basepromptData), nil
}

func loadBasePrompt() (map[string]any, error) {
	for _, dir := range []string{"assets", "data", "."} {
		path := dir + string(os.PathSeparator) + "baseprompt.json"
		if raw, err := os.ReadFile(path); err == nil {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
				return m, nil
			}
		}
	}
	raw, err := basepromptFS.ReadFile("baseprompt.json")
	if err != nil {
		return nil, fmt.Errorf("baseprompt: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("baseprompt: %w", err)
	}
	return m, nil
}

// BuildQoderBody constructs the agent_chat_generation request body.
//
// The official baseprompt is the skeleton; every field that identifies the
// request, the model, the conversation and the billing attribution is
// overwritten.
func BuildQoderBody(payload map[string]any, ctx BuildContext) (map[string]any, error) {
	body, err := BasePrompt()
	if err != nil {
		return nil, err
	}

	messages := decodeMessages(payload["messages"])
	messages = NormalizeRoles(messages)
	model := asString(payload["model"])

	flat := FlattenMessages(messages, FlattenOptions{
		KeepReasoning:      IsDeepSeek(model, ctx.ModelKey),
		Structured:         ctx.Structured,
		ToolResultKeep:     ctx.ToolResultKeep,
		ToolResultScope:    ctx.ToolResultScope,
		ToolResultKeepTail: ctx.ToolResultTail,
	})

	// Images are normalised after flattening so every protocol goes through the
	// same outbound construction.
	flatMessages, stats := PrepareInlineImages(flat.Messages)
	if ctx.OnImagePreflight != nil {
		ctx.OnImagePreflight(stats.Images, stats.Optimized, stats.BytesBefore,
			stats.BytesAfter, stats.CodecMissing)
	}

	// The template's sample user turn is example content and must be dropped;
	// only its system base survives.
	tmplSystem := []any{}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && asString(mm["role"]) == "system" {
				tmplSystem = append(tmplSystem, mm)
			}
		}
	}
	switch {
	case flat.System != "":
		body["messages"] = []any{map[string]any{"role": "system", "content": flat.System}}
	case ctx.SystemPromptOverride != "" && ctx.SystemPromptOverride != ctx.DefaultSystemPrompt:
		body["messages"] = []any{map[string]any{"role": "system", "content": ctx.SystemPromptOverride}}
	default:
		body["messages"] = tmplSystem
	}
	real := make([]any, 0, len(flatMessages))
	for _, m := range flatMessages {
		real = append(real, m)
	}
	body["messages"] = append(body["messages"].([]any), real...)

	// The latest user prompt: used for chat_context highlighting and the
	// business conversation name.
	prompt := ""
	for i := len(flatMessages) - 1; i >= 0; i-- {
		if asString(flatMessages[i]["role"]) != "user" {
			continue
		}
		switch c := flatMessages[i]["content"].(type) {
		case string:
			if c != "" {
				prompt = c
			}
		case []any:
			var parts []string
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok && asString(pm["type"]) == "text" {
					parts = append(parts, asString(pm["text"]))
				}
			}
			prompt = strings.Join(parts, "\n")
		}
		if prompt != "" {
			break
		}
	}

	nid := randomUUID()
	body["request_id"] = nid
	convID := nid
	var beginAt int64
	if ctx.Conversation != nil && ctx.Conversation.ID != "" {
		convID = ctx.Conversation.ID
	}
	body["chat_record_id"] = convID
	body["request_set_id"] = randomUUID()
	body["session_id"] = firstNonEmpty(ctx.SessionID, randomUUID())
	body["stream"] = true
	body["agent_id"] = "agent_common"
	if ctx.UserType != "" {
		body["aliyun_user_type"] = ctx.UserType
	}

	// model_config comes from the live catalog so display_name always matches
	// what the official client is showing right now.
	mc, _ := body["model_config"].(map[string]any)
	if mc == nil {
		mc = map[string]any{}
	}
	mc["key"] = ctx.ModelKey
	mc["display_name"] = firstNonEmpty(ctx.DisplayName, ctx.ModelKey)
	mc["model"] = ""
	mc["format"] = "openai"
	mc["is_vl"] = ctx.IsVL
	mc["is_reasoning"] = ctx.IsReasoning
	mc["api_key"] = ""
	mc["url"] = ""
	mc["source"] = "system"
	maxInput := ctx.MaxInput
	if maxInput == 0 {
		maxInput = 180000
	}
	if ctx.ContextOverride > 0 {
		maxInput = ctx.ContextOverride
	}
	mc["max_input_tokens"] = maxInput
	body["model_config"] = mc

	cc, _ := body["chat_context"].(map[string]any)
	if cc == nil {
		cc = map[string]any{}
	}
	txt, _ := cc["text"].(map[string]any)
	if txt == nil {
		txt = map[string]any{"type": "text"}
	}
	txt["text"] = prompt
	cc["text"] = txt
	extra, _ := cc["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
	}
	oc, _ := extra["originalContent"].(map[string]any)
	if oc == nil {
		oc = map[string]any{"type": "text"}
	}
	oc["text"] = prompt
	extra["originalContent"] = oc
	extra["modelConfig"] = copyMap(mc)
	cc["extra"] = extra
	if ctx.KeepTopLevelImages && len(flat.Images) > 0 {
		// Legacy behaviour for diagnostics only: the upstream ignores these,
		// and they triple the request size.
		cc["imageUrls"] = flat.Images
		body["image_urls"] = flat.Images
	}
	body["chat_context"] = cc

	params, _ := body["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	if v, ok := payload["max_tokens"]; ok && v != nil {
		n := int(asInt(v))
		if ctx.MaxTokensClamp != nil {
			n = ctx.MaxTokensClamp(n)
		}
		if n > 0 {
			params["max_tokens"] = n
		}
	} else if v, ok := payload["max_completion_tokens"]; ok && v != nil {
		n := int(asInt(v))
		if ctx.MaxTokensClamp != nil {
			n = ctx.MaxTokensClamp(n)
		}
		if n > 0 {
			params["max_tokens"] = n
		}
	}
	if ctx.Effort != "" {
		params["reasoning_effort"] = ctx.Effort
	}
	body["parameters"] = params

	// Tools: the client's list wins. Never leak the desktop client's built-in
	// agent tools (Bash/Edit/...) to a plain API client.
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		body["tools"] = tools
	} else {
		body["tools"] = []any{}
	}
	normalizeToolsInPlace(body)
	if ctx.KimiToolProtocol {
		applyKimiToolProtocol(body, payload)
	}

	biz, _ := body["business"].(map[string]any)
	if biz == nil {
		biz = map[string]any{}
	}
	if ctx.Conversation != nil && ctx.Conversation.ID != "" {
		biz["id"] = ctx.Conversation.ID
		beginAt = ctx.Conversation.BeginAt
		if beginAt == 0 {
			beginAt = time.Now().UnixMilli()
		}
	} else {
		biz["id"] = randomUUID()
		beginAt = time.Now().UnixMilli()
	}
	biz["begin_at"] = beginAt
	if prompt != "" {
		biz["name"] = truncateRunes(prompt, 30)
	} else {
		biz["name"] = "chat"
	}
	if _, ok := biz["product"]; !ok {
		biz["product"] = "cli"
	}
	if _, ok := biz["type"]; !ok {
		biz["type"] = "agent"
	}
	if _, ok := biz["stage"]; !ok {
		biz["stage"] = "start"
	}
	// The upstream bills by business.model_key; omitting it silently attributes
	// newer models (kmodel_latest) to "auto".
	biz["model_key"] = ctx.ModelKey
	// Version travels with the identity story: keep it in step with cosy-version.
	if ctx.CosyVersion != "" {
		biz["version"] = ctx.CosyVersion
	}
	body["business"] = biz

	return body, nil
}

// normalizeToolsInPlace nests every tool schema under `function` and drops
// entries without a name.
func normalizeToolsInPlace(body map[string]any) {
	tools, ok := body["tools"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := m["function"].(map[string]any); hasFn {
			out = append(out, m)
			continue
		}
		typ, _ := m["type"].(string)
		if typ != "" && typ != "function" {
			out = append(out, m)
			continue
		}
		name := asString(m["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d, ok := m["description"]; ok {
			fn["description"] = d
		}
		if p, ok := m["parameters"]; ok {
			fn["parameters"] = p
		} else if p, ok := m["input_schema"]; ok {
			fn["parameters"] = p
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	body["tools"] = out
}

// kimiToolProtocolPrompt is appended to the system prompt for Kimi models with
// declared tools. It is a fixed string so the prompt prefix stays stable
// between turns.
const kimiToolProtocolPrompt = "\n\nWhen you call a tool, use the native tool-call channel. " +
	"Arguments must be a single JSON object with correct escaping, must follow the tool's " +
	"schema exactly, and must be complete and closed. For long file writes, split the content " +
	"according to the tool's capability. Do not print tool-call markers, XML/DSML tags, JSON " +
	"call envelopes, or other scaffolding in the reply text."

// applyKimiToolProtocol prevents Kimi from echoing the double-JSON text
// envelope it sees in its own history (issue #153).
func applyKimiToolProtocol(body map[string]any, payload map[string]any) {
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		return
	}
	if tc, ok := payload["tool_choice"]; ok && asString(tc) == "none" {
		return
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok || asString(mm["role"]) != "system" {
			continue
		}
		content := asString(mm["content"])
		if strings.Contains(content, "native tool-call channel") {
			return
		}
		msgs[i] = map[string]any{"role": "system", "content": content + kimiToolProtocolPrompt}
		body["messages"] = msgs
		return
	}
}

// decodeMessages converts a decoded JSON message array into Message structs.
func decodeMessages(raw any) []Message {
	list, ok := raw.([]any)
	if !ok {
		// Some clients send a single message object instead of an array.
		if m, ok := raw.(map[string]any); ok {
			list = []any{m}
		} else {
			return nil
		}
	}
	out := make([]Message, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := Message{
			Role:    asString(m["role"]),
			Content: m["content"],
			Name:    asString(m["name"]),
		}
		msg.ToolCallID = asString(m["tool_call_id"])
		if rc, ok := m["reasoning_content"].(string); ok {
			msg.ReasoningContent = &rc
		}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tm["function"].(map[string]any)
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{
					ID:   asString(tm["id"]),
					Type: asString(tm["type"]),
					Function: FunctionCall{
						Name:      asString(fn["name"]),
						Arguments: argumentsToString(fn["arguments"]),
					},
				})
			}
		}
		out = append(out, msg)
	}
	return out
}

// argumentsToString keeps arguments as a JSON string; a decoded object is
// re-serialised because both the client and upstream converters assume string.
func argumentsToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case map[string]any, []any:
		blob, err := MarshalNoEscape(t)
		if err != nil {
			return ""
		}
		return string(blob)
	default:
		return asString(v)
	}
}

// ---------------------------------------------------------------------------
// JSON copy helpers
// ---------------------------------------------------------------------------

func deepCopyMap(src map[string]any) map[string]any {
	blob, err := json.Marshal(src)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func copyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewRequestID returns a 32-hex request id.
func NewRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// BodySizeUpperBound returns the encoded wire size for a plain body.
//
// Qoder's custom encoding preserves padded Base64 length, so the outbound size
// can be checked without allocating the multi-megabyte encoded string.
func BodySizeUpperBound(plainLen int) int {
	return 4 * ((plainLen + 2) / 3)
}
