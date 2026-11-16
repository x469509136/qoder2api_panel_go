package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/catalog"
	"github.com/xzs/bridge_qoder/internal/guards"
	"github.com/xzs/bridge_qoder/internal/protocol"
	"github.com/xzs/bridge_qoder/internal/settings"
	"github.com/xzs/bridge_qoder/internal/sign"
	"github.com/xzs/bridge_qoder/internal/upstream"
	"github.com/xzs/bridge_qoder/internal/usage"
)

// Chat tuning constants.
const (
	transientMaxRetries      = 2
	defaultEffortClampThresh = 240
)

// handleChat serves POST /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	// Local web tools intercept the request before any upstream work.
	if webtoolsEnabled(s) {
		if names := s.builtinWebToolNames(payload); len(names) > 0 {
			s.handleWebTools(w, r, payload, names)
			return
		}
	}

	protocol.NormalizeToolChoice(payload)
	protocol.NormalizeTools(payload)
	protocol.TranslateMaxCompletionTokens(payload)
	model := firstNonEmptyStr(asStringAny(payload["model"]), "auto")
	if _, has := payload["stream"]; !has {
		payload["stream"] = false
	}
	wantStream, _ := payload["stream"].(bool)

	if blocked := s.modelPrecheck(model, s.requestRealm(r), r); blocked != "" {
		s.writeError(w, http.StatusBadRequest, blocked, "invalid_request_error")
		return
	}

	sessionKey := extractSessionKey(r, payload)
	fp := promptFingerprint(payload["messages"], "", 0)
	start := time.Now()
	effortRequested := protocol.RequestedEffort(payload)
	applyContextHeader(r, payload)

	up, acc, err := s.openUpstreamFor(r, payload, sessionKey)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer up.Close()

	holder := &upstream.Holder{}
	allowedNames := guards.ToolNamesFromPayload(payload)
	ctx := r.Context()

	if !wantStream {
		result, err := s.aggregateStream(ctx, up, payload, holder, allowedNames)
		if err != nil {
			wall := int(time.Since(start).Milliseconds())
			s.recordError(model, upstreamStatus(err), err.Error(), wall, acc, s.keyIDForUsage(r))
			s.writeUpstreamFriendly(w, err)
			return
		}
		s.recordUsage(usage.Row{
			Model: model, Stream: false, ElapsedMS: intPtr(int(time.Since(start).Milliseconds())),
			TTFTMS:       intPtr(int(time.Since(start).Milliseconds())),
			PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
			CachedTokens: result.CachedTokens, CachedReported: result.CachedReported,
			TotalTokens: result.TotalTokens, Credit: result.Credit,
			ReasoningTokens: result.ReasoningTokens,
			Account:         acc.UID, Realm: acc.Realm, KeyID: s.keyIDForUsage(r),
			EffortRequested: effortRequested, Effort: s.sentEffort,
			MsgsSHA: fp.MsgsSHA, NMsgs: fp.NMsgs,
		})
		s.writeJSON(w, http.StatusOK, result.Object)
		return
	}

	s.streamChat(w, r, up, acc, payload, holder, allowedNames, model, start, effortRequested, fp, ctx)
}

// streamChat forwards the stream, applying the guard chain.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, up *upstream.Response,
	acc *accounts.Account, payload map[string]any, holder *upstream.Holder,
	allowed map[string]bool, model string, start time.Time, effortRequested string,
	fp promptFP, ctx context.Context) {
	sw := s.newSSEWriter(w)
	firstByte := -1

	var frames []guards.Frame
	pumpErr := upstream.Stream(ctx, up.Body, holder, func(inner map[string]any) error {
		if firstByte < 0 {
			firstByte = int(time.Since(start).Milliseconds())
		}
		frames = append(frames, guards.ParseFrame(encodeDataLine(inner)))
		return nil
	})
	if pumpErr != nil {
		if he, ok := pumpErr.(*upstream.EnvelopeStatusError); ok {
			s.noteRisk("rejected", he.Detail)
			s.handleAccountCooldown(acc, he.Status, he.Detail, model)
		} else {
			s.noteRisk("timeout", pumpErr.Error())
			s.handlePostFailureForStream(acc, pumpErr, s.currentRealm(), model)
		}
	}

	guarded, guardErr := guards.Process(frames, allowed)
	for _, f := range guarded {
		sw.WriteBytes(f.Raw)
	}

	if guardErr != nil {
		s.handleGuardFailure(sw, r, guardErr, model, acc, start, fp, effortRequested)
		return
	}
	if pumpErr != nil {
		msg, etype := friendlyUpstreamError(upstreamStatus(pumpErr), errText(pumpErr))
		errBody, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": msg, "type": etype, "code": clientErrorCode(upstreamStatus(pumpErr), etype),
		}})
		sw.Write("data: " + string(errBody) + "\n\n")
		sw.Done()
		s.recordError(model, upstreamStatus(pumpErr), errText(pumpErr),
			int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
		return
	}
	sw.Done()

	wall := int(time.Since(start).Milliseconds())
	s.recordUsage(usage.Row{
		Model: model, Stream: true, ElapsedMS: &wall, TTFTMS: optInt(firstByte),
		GenMS:        optInt(wall - firstByte),
		PromptTokens: holderPrompt(holder), CompletionTokens: holderCompletion(holder),
		CachedTokens: holderCached(holder), CachedReported: holderCachedReported(holder),
		TotalTokens: holderTotal(holder), Credit: holderCredit(holder),
		ReasoningTokens: holderReasoning(holder),
		Account:         acc.UID, Realm: acc.Realm, KeyID: s.keyIDForUsage(r),
		EffortRequested: effortRequested, Effort: s.sentEffort, MsgsSHA: fp.MsgsSHA, NMsgs: fp.NMsgs,
	})
}

// handlePostFailureForStream marks the account after a transport-level failure
// that surfaced mid-stream.
func (s *Server) handlePostFailureForStream(acc *accounts.Account, err error, realm, model string) {
	if acc == nil {
		return
	}
	if upstream.IsTransientTransport(err) {
		acc.NoteError("stream transport: "+truncateStr(errText(err), 100), 15, false, "", nil)
		return
	}
	acc.NoteError(truncateStr(errText(err), 120), 60, false, "", nil)
}

// handleGuardFailure applies the strict/lenient policy to a guard failure.
func (s *Server) handleGuardFailure(sw *sseWriter, r *http.Request, guardErr *guards.ToolCallError,
	model string, acc *accounts.Account, start time.Time, fp promptFP,
	effortRequested string) {
	if s.toolEchoLenient() {
		// Lenient finishes normally and keeps whatever prose was produced: the
		// client sees a completed turn with no tool call instead of a hard stop.
		guards.NoteLenientTurn()
		s.log.Warn("chat", "lenient tool-echo finish (%s) for model %s", guardErr.Kind, model)
		sw.Done()

		s.recordUsage(usage.Row{
			Model: model, Stream: true, ElapsedMS: intPtr(int(time.Since(start).Milliseconds())),
			Account: acc.UID, Realm: acc.Realm, KeyID: s.keyIDForUsage(r),
			EffortRequested: effortRequested, MsgsSHA: fp.MsgsSHA, NMsgs: fp.NMsgs,
		})
		return
	}
	s.log.Warn("chat", "tool-echo guard failed the turn closed (%s, %d chars) for model %s",
		guardErr.Kind, guardErr.Chars, model)
	msg, etype := friendlyUpstreamError(502, guardErr.Detail)
	errBody, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": etype, "code": clientErrorCode(502, etype),
	}})
	sw.Write("data: " + string(errBody) + "\n\n")
	sw.Done()

	s.recordError(model, 502, guardErr.Detail, int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
}

// promptFP is the privacy-safe prompt fingerprint.
type promptFP struct {
	MsgsSHA string
	NMsgs   int
}

func promptFingerprint(messages any, sysSrc string, nMsgSystem int) promptFP {
	blob, err := protocol.MarshalNoEscape(messages)
	if err != nil {
		return promptFP{}
	}
	sum := sha256.Sum256(blob)
	list, _ := messages.([]any)
	return promptFP{MsgsSHA: hex.EncodeToString(sum[:])[:12], NMsgs: len(list)}
}

var conversationHeaders = []string{
	"X-Conversation-Id", "X-Session-Id", "X-Chat-Id", "X-Request-Id",
}

// extractSessionKey finds the client's conversation identity.
func extractSessionKey(r *http.Request, payload map[string]any) string {
	for _, h := range conversationHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return "hdr-" + v
		}
	}
	for _, field := range []string{"conversation_id", "session_id", "chat_id"} {
		if v := strings.TrimSpace(asStringAny(payload[field])); v != "" {
			return "body-" + v
		}
	}
	if meta, ok := payload["metadata"].(map[string]any); ok {
		if v := strings.TrimSpace(asStringAny(meta["user_id"])); v != "" {
			return "meta-" + v
		}
	}
	// Fall back to the first user message: only that, never the system prompt.
	// The system prompt often carries per-turn context; including it would rotate
	// the affinity key every turn and destroy the upstream cache.
	if list, ok := payload["messages"].([]any); ok {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok || asStringAny(m["role"]) != "user" {
				continue
			}
			blob, err := protocol.MarshalNoEscape([]any{m})
			if err != nil {
				break
			}
			sum := sha256.Sum256(blob)
			return "pfx-" + hex.EncodeToString(sum[:])[:16]
		}
	}
	return ""
}

// applyContextHeader moves X-QD-Context into the payload.
//
// Any client-supplied _qd_context key is removed first: the window may only be
// raised through the HTTP header, never forged in the body.
func applyContextHeader(r *http.Request, payload map[string]any) {
	delete(payload, "_qd_context")
	if hdr := strings.TrimSpace(r.Header.Get("X-QD-Context")); hdr != "" {
		if len(hdr) > 12 {
			hdr = hdr[:12]
		}
		payload["_qd_context"] = hdr
	}
}

// ---------------------------------------------------------------------------
// Upstream orchestration
// ---------------------------------------------------------------------------

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// shortCooldownWait returns how long to wait when every account is briefly
// cooling down.
func (s *Server) shortCooldownWait(realm, model string, tried, allowed map[string]bool) float64 {
	best := 0.0
	for _, acc := range s.pool.Accounts() {
		if acc.Realm != realm {
			continue
		}
		if allowed != nil && !allowed[acc.UID] {
			continue
		}
		if tried[acc.UID] {
			continue
		}
		if !acc.InferenceEligible() {
			continue
		}
		wait := acc.ThrottleWait(model)
		if wait > 0 && wait <= 10 && (best == 0 || wait < best) {
			best = wait
		}
	}
	return best
}

// buildBody constructs the upstream request body for one account.
func (s *Server) buildBody(payload map[string]any, acc *accounts.Account, modelKey, realm, sessionKey string) (map[string]any, error) {
	model := asStringAny(payload["model"])
	meta := s.catalogMetaFor(modelKey, realm)
	structured := s.structuredToolHistoryEnabled(model, modelKey, realm, payload)
	if v, ok := payload["_qd_structured"].(bool); ok {
		structured = v
	}
	keep, scope, tail := protocol.ToolResultSettings()
	conv := s.conversationIdentity(payload, sessionKey)
	effort := s.normalizeEffort(payload, meta)
	convID := ""
	var beginAt int64
	if conv != nil {
		convID = conv.ID
		beginAt = conv.BeginAt
	}
	ctx := protocol.BuildContext{
		UID: acc.UID, UserType: acc.UserTypeValue(),
		ModelKey: modelKey, DisplayName: meta.DisplayName,
		IsVL: meta.IsVL, IsReasoning: meta.IsReasoning,
		MaxInput: meta.MaxInputTokens, Realm: realm,
		Conversation:         &protocol.ConversationIdentity{ID: convID, BeginAt: beginAt},
		Structured:           structured,
		SystemPromptOverride: s.systemPrompt,
		DefaultSystemPrompt:  DefaultSystemPrompt,
		ContextOverride:      s.modelContextOverride(payload, modelKey),
		Effort:               effort,
		CosyVersion:          sign.CosyVersion(),
		KimiToolProtocol:     protocol.IsKimi(model, modelKey),
		ToolResultKeep:       keep, ToolResultScope: scope, ToolResultTail: tail,
		MaxTokensClamp: func(n int) int {
			if limit := s.maxOutputLimit(modelKey, realm); limit > 0 && n > limit {
				s.log.Warn("chat", "max_tokens %d clamped to the learned upstream limit %d", n, limit)
				return limit
			}
			return n
		},
		OnImagePreflight: func(count, optimized, before, after, missing int) {
			if optimized > 0 || missing > 0 {
				s.log.Info("image", "image preflight: count=%d optimized=%d chars=%d->%d codec_missing=%d",
					count, optimized, before, after, missing)
			}
		},
	}
	body, err := protocol.BuildQoderBody(payload, ctx)
	if err != nil {
		return nil, err
	}
	// The prefix cache chooses the upstream session id within the selected account.
	sessionID := ""
	if s.sessionCache != nil {
		if id, err := s.sessionCache.Resolve(messageList(body), acc.UID, realm, modelKey,
			sessionKey, s.keyIDFromPayload(payload), map[string]any{
				"tools":      body["tools"],
				"parameters": body["parameters"],
				"agent_id":   body["agent_id"],
			}); err == nil {
			sessionID = id
		}
	}
	if sessionID == "" && convID != "" {
		sessionID = convID
	}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	return body, nil
}

func messageList(body map[string]any) []any {
	list, _ := body["messages"].([]any)
	return list
}

// conversationIdentity reuses one upstream conversation identity per dialogue.
//
// The upstream usage panel groups by chat_record_id; minting a new id per request
// splits one agent turn into dozens of "<1min conversations" and burns the daily
// cap.
func (s *Server) conversationIdentity(payload map[string]any, sessionKey string) *protocol.ConversationIdentity {
	key := sessionKey
	if key == "" {
		key = "anon"
	}
	s.convMu.Lock()
	defer s.convMu.Unlock()
	if s.convMap == nil {
		s.convMap = map[string]*protocol.ConversationIdentity{}
	}
	if hit, ok := s.convMap[key]; ok {
		return hit
	}
	if len(s.convMap) >= 4096 {
		s.convMap = map[string]*protocol.ConversationIdentity{}
	}
	conv := &protocol.ConversationIdentity{
		ID: newUUID(), BeginAt: time.Now().UnixMilli(),
	}
	s.convMap[key] = conv
	return conv
}

// normalizeEffort clamps the requested effort to what the model supports.
func (s *Server) normalizeEffort(payload map[string]any, meta catalog.Model) string {
	requested := protocol.RequestedEffort(payload)
	if requested == "" {
		return ""
	}
	norm, note := catalog.NormalizeEffort(requested, meta)
	if note != "" {
		s.log.Info("chat", "reasoning_effort %s on %q", note, meta.Key)
	}
	return norm
}

func sentEffort(body map[string]any) string {
	params, _ := body["parameters"].(map[string]any)
	if params == nil {
		return ""
	}
	v, _ := params["reasoning_effort"].(string)
	return v
}

// detectModelRealm resolves the exit from the model's own availability.
func (s *Server) detectModelRealm(model string) string {
	if owner := s.catalog.ExclusiveRealm(model); owner != "" {
		return owner
	}
	if model == "" || !envBool("QD_AUTO_REALM", true) {
		return s.currentRealm()
	}
	cur := s.currentRealm()
	if realm, _ := s.catalog.AutoRealmForModel(model, cur); realm != "" {
		return realm
	}
	return cur
}

// autoRealmReason names the deterministic reason for a realm switch.
func (s *Server) autoRealmReason(model, realm string) string {
	cur := s.currentRealm()
	if hint, _ := s.catalog.AutoRealmForModel(model, cur); hint == realm {
		return "model_missing"
	}
	return "no_account"
}

// structuredToolHistoryEnabled is the single decision point for tool history
// shape.
func (s *Server) structuredToolHistoryEnabled(model, modelKey, realm string, payload map[string]any) bool {
	if v, ok := payload["_qd_structured"].(bool); ok {
		return v
	}
	msgs := decodeMessagesForIDs(payload["messages"])
	return protocol.StructuredToolHistoryEnabled(model, modelKey, realm, msgs)
}

func decodeMessagesForIDs(raw any) []protocol.Message {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]protocol.Message, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := protocol.Message{Role: asStringAny(m["role"]),
			ToolCallID: asStringAny(m["tool_call_id"])}
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := tm["function"].(map[string]any)
				msg.ToolCalls = append(msg.ToolCalls, protocol.ToolCall{
					ID: asStringAny(tm["id"]),
					Function: protocol.FunctionCall{
						Name: asStringAny(fn["name"]),
					},
				})
			}
		}
		out = append(out, msg)
	}
	return out
}

// modelContextOverride reads the context-tier override for a model.
func (s *Server) modelContextOverride(payload map[string]any, modelKey string) int64 {
	label := asStringAny(payload["_qd_context"])
	if label == "" {
		if v, ok := s.settings.ModelContext()[modelKey]; ok {
			label = asStringAny(v)
		}
	}
	if label == "" {
		return 0
	}
	return s.resolveContextTier(label, modelKey)
}

// resolveContextTier maps a tier label or token count onto a token count.
func (s *Server) resolveContextTier(label, modelKey string) int64 {
	upper := strings.ToUpper(strings.TrimSpace(label))
	multiplier := int64(0)
	switch {
	case strings.HasSuffix(upper, "M"):
		if n, err := parseLeadingInt(upper[:len(upper)-1]); err == nil {
			multiplier = int64(n) * 1000000
		}
	case strings.HasSuffix(upper, "K"):
		if n, err := parseLeadingInt(upper[:len(upper)-1]); err == nil {
			multiplier = int64(n) * 1000
		}
	default:
		if n, err := parseLeadingInt(upper); err == nil {
			multiplier = int64(n)
		}
	}
	if multiplier <= 0 {
		return 0
	}
	// Verify against the model's official selectable windows when it publishes any.
	meta := s.catalogMetaFor(modelKey, s.currentRealm())
	if len(meta.ContextConfig) > 0 {
		for _, conf := range meta.ContextConfig {
			if conf.TokenCount == multiplier {
				return multiplier
			}
		}
		// A tier the model does not advertise is still allowed (the catalog
		// windows are not exhaustive), but never beyond the largest window.
		var maxWindow int64
		for _, conf := range meta.ContextConfig {
			if conf.TokenCount > maxWindow {
				maxWindow = conf.TokenCount
			}
		}
		if maxWindow > 0 && multiplier > maxWindow {
			return maxWindow
		}
	}
	return multiplier
}

func parseLeadingInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// modelPrecheck runs the three model gates shared by every entry point.
func (s *Server) modelPrecheck(model, realm string, r *http.Request) string {
	if cross := s.crossRealmError(model, realm, r); cross != "" {
		return cross
	}
	if s.settings.IsModelBanned(model) {
		return fmt.Sprintf("模型 %s 已被本机网关封禁（settings 的 banned_models 配置）。"+
			"如需使用，请在设置页或 settings.json 里调整 banned_models。", model)
	}
	if entry := s.keyEntry(r); entry != nil && !settings.KeyAllowsModel(entry, model) {
		return fmt.Sprintf("API Key「%s」的模型限制不允许 %s。该 Key 目前允许：%s。"+
			"请在设置页修改该 Key 的模型限制，或改用允许该模型的 Key。",
			entry.Name, model, joinOrDash(entry.Models))
	}
	return ""
}

func (s *Server) crossRealmError(model, realm string, r *http.Request) string {
	if realm == "" || model == "" {
		return ""
	}
	owner := s.catalog.ExclusiveRealm(model)
	if owner == "" || owner == realm {
		return ""
	}
	name := "当前 Key"
	if entry := s.keyEntry(r); entry != nil && entry.Name != "" {
		name = entry.Name
	}
	served := map[string]string{"cn": "国内版", "intl": "国际版"}[owner]
	used := map[string]string{"cn": "国内版", "intl": "国际版"}[realm]
	return fmt.Sprintf("模型 %s 只在%s提供，但「%s」绑定的是%s出口。"+
		"请改用对应出口的 Key，或把该 Key 的出口改为「跟随面板切换」。",
		model, served, name, used)
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, "、")
}

// ---------------------------------------------------------------------------
// Aggregation (non-streaming)
// ---------------------------------------------------------------------------

// aggResult is the folded non-streaming response.
type aggResult struct {
	Object           map[string]any
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CachedReported   bool
	TotalTokens      int
	ReasoningTokens  int
	Credit           float64
}

// aggregateStream folds the envelope stream into one chat.completion object.
func (s *Server) aggregateStream(ctx context.Context, up *upstream.Response,
	payload map[string]any, holder *upstream.Holder, allowed map[string]bool) (*aggResult, error) {
	var frames []guards.Frame
	err := upstream.Stream(ctx, up.Body, holder, func(inner map[string]any) error {
		frames = append(frames, guards.ParseFrame(encodeDataLine(inner)))
		return nil
	})
	if err != nil {
		if he, ok := err.(*upstream.EnvelopeStatusError); ok {
			return nil, he
		}
		return nil, err
	}

	// A non-streaming request still runs the same guard chain: the model may echo
	// its tool calls as text regardless of the transport.
	guarded, guardErr := guards.Process(frames, allowed)
	if guardErr != nil {
		if s.toolEchoLenient() {
			guards.NoteLenientTurn()
			s.log.Warn("chat", "lenient tool-echo finish (%s) for non-streaming turn", guardErr.Kind)
		} else {
			return nil, &upstream.StatusError{Code: 502, Detail: guardErr.Detail}
		}
	}

	var text, reasoning strings.Builder
	finish := "stop"
	toolCalls := map[int]map[string]any{}
	seq := 0
	for _, f := range guarded {
		if f.Payload == nil {
			continue
		}
		choices, _ := f.Payload["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			text.WriteString(asStringAny(delta["content"]))
			reasoning.WriteString(asStringAny(delta["reasoning_content"]))
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, t := range tcs {
					tc, ok := t.(map[string]any)
					if !ok {
						continue
					}
					idx := seq
					if v, ok := tc["index"]; ok && v != nil {
						idx = intAny(v)
					}
					slot, ok := toolCalls[idx]
					if !ok {
						slot = map[string]any{
							"id": asStringAny(tc["id"]), "type": "function",
							"function": map[string]any{"name": "", "arguments": ""},
						}
						toolCalls[idx] = slot
					}
					fn, _ := tc["function"].(map[string]any)
					if name := asStringAny(fn["name"]); name != "" {
						slot["function"].(map[string]any)["name"] = name
					}
					if args := asStringAny(fn["arguments"]); args != "" {
						cur := slot["function"].(map[string]any)["arguments"].(string)
						slot["function"].(map[string]any)["arguments"] = cur + args
					}
					seq++
				}
			}
			if fr := asStringAny(choice["finish_reason"]); fr != "" {
				finish = fr
			}
		}
	}

	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		indexes := make([]int, 0, len(toolCalls))
		for i := range toolCalls {
			indexes = append(indexes, i)
		}
		sortInts(indexes)
		calls := make([]any, 0, len(indexes))
		for _, i := range indexes {
			calls = append(calls, toolCalls[i])
		}
		message["tool_calls"] = calls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}

	u := protocol.DecodeUsage(holder.Usage)
	result := &aggResult{Object: map[string]any{
		"id":      "chatcmpl-" + newHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   firstNonEmptyStr(asStringAny(payload["model"]), up.Account.UID),
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": usageMapForClient(u),
	}}
	if u != nil {
		result.PromptTokens = u.PromptTokens
		result.CompletionTokens = u.CompletionTokens
		result.CachedTokens = u.EffectiveCached()
		result.CachedReported = u.CachedReported()
		result.TotalTokens = u.TotalTokens
		result.ReasoningTokens = u.ReasoningTokens()
		result.Credit = u.Credits
	}
	return result, nil
}

// usageMapForClient normalises the usage block for the client.
func usageMapForClient(u *protocol.Usage) map[string]any {
	if u == nil {
		return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	cached := u.EffectiveCached()
	out := map[string]any{
		"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
		"total_tokens": u.TotalTokens,
	}
	if cached > 0 || u.CachedReported() {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if u.CompletionTokensDetails != nil {
		out["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	if u.Credits > 0 {
		out["credits"] = u.Credits
	}
	return out
}

// ---------------------------------------------------------------------------
// Usage recording
// ---------------------------------------------------------------------------

func (s *Server) recordUsage(row usage.Row) {
	if row.Realm == "" {
		row.Realm = s.currentRealm()
	}
	if err := s.usage.Record(row); err != nil {
		s.log.Warn("usage", "usage persist failed: %v", err)
		return
	}
	s.log.Info("chat", "chat done: model=%s acct=%s tokens=%d (in=%d out=%d)",
		row.Model, shortUID(row.Account), row.TotalTokens, row.PromptTokens, row.CompletionTokens)
}

func (s *Server) recordError(model string, status int, message string, elapsedMS int,
	acc *accounts.Account, keyID string) {
	account := ""
	realm := s.currentRealm()
	if acc != nil {
		account = acc.UID
		realm = acc.Realm
	}
	s.log.Warn("chat", "error: model=%s status=%d account=%s: %s",
		model, status, shortUID(account), truncateStr(message, 200))
	_ = s.usage.Record(usage.Row{
		Model: model, ErrType: firstNonEmptyStr(httpStatusText(status), "error"),
		At:      nowFloat(),
		Account: account, Realm: realm, KeyID: keyID,
	})
}

func (s *Server) keyIDForUsage(r *http.Request) string {
	if entry := s.keyEntry(r); entry != nil {
		return entry.ID
	}
	return ""
}

func (s *Server) keyIDFromPayload(payload map[string]any) string {
	if v, ok := payload["_qd_key_id"].(string); ok {
		return v
	}
	return ""
}

func (s *Server) handleAccountCooldown(acc *accounts.Account, status int, detail, model string) {
	if acc == nil {
		return
	}
	if upstream.QueueWaitSeconds(detail) > 0 {
		return
	}
	if status == 401 || status == 403 {
		acc.NoteError(fmt.Sprintf("in-stream HTTP %d", status), 60, false, "", nil)
	}
}

// ---------------------------------------------------------------------------
// SSE helpers
// ---------------------------------------------------------------------------

// sseWriter wraps a streaming response.
//
// Chunked encoding keeps the connection reusable: a "Connection: close" stream is
// mistaken for a reusable one by pooled clients, and the next request lands on a
// half-closed socket (observed as endless reconnects).
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *Server) newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	if realm := s.currentRealm(); realm != "" {
		h.Set("X-Qoder-Realm", realm)
	}
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w}
	sw.flusher, _ = w.(http.Flusher)
	return sw
}

// Write sends one SSE frame.
func (w *sseWriter) Write(payload string) {
	if _, err := w.w.Write([]byte(payload)); err != nil {
		return
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// WriteBytes sends raw frame bytes.
func (w *sseWriter) WriteBytes(payload []byte) {
	if _, err := w.w.Write(payload); err != nil {
		return
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// Done emits the terminating sentinel.
func (w *sseWriter) Done() { w.Write("data: [DONE]\n\n") }

func encodeDataLine(inner map[string]any) []byte {
	blob, _ := protocol.MarshalNoEscape(inner)
	out := append([]byte("data: "), blob...)
	return append(out, '\n', '\n')
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func asStringAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func intAny(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}

func intPtr(v int) *int { return &v }

func optInt(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

func sortInts(list []int) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func shortUID(uid string) string {
	if len(uid) >= 8 {
		return uid[:8]
	}
	return uid
}

func httpStatusText(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("http_%d", code)
}

func upstreamStatus(err error) int {
	var se *upstream.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	var ee *upstream.EnvelopeStatusError
	if errors.As(err, &ee) {
		return ee.Status
	}
	var qe *upstream.QueuedError
	if errors.As(err, &qe) {
		return 429
	}
	var pe *upstream.PayloadTooLargeError
	if errors.As(err, &pe) {
		return 413
	}
	return 502
}

var rangeRe = regexp.MustCompile(`Range of max_tokens should be \[1,\s*(\d+)\]`)

// learnFromUpstreamError opportunistically learns an output cap from a 400.
func (s *Server) learnFromUpstreamError(detail, modelKey string) {
	if m := rangeRe.FindStringSubmatch(detail); m != nil {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil && n > 0 {
			s.learnMaxOutputLimit(n, modelKey, s.currentRealm())
		}
	}
}
