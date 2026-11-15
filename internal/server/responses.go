package server

import (
	"net/http"
	"time"

	"github.com/xzs/bridge_qoder/internal/accounts"
	"github.com/xzs/bridge_qoder/internal/guards"
	"github.com/xzs/bridge_qoder/internal/protocol"
	"github.com/xzs/bridge_qoder/internal/upstream"
	"github.com/xzs/bridge_qoder/internal/usage"
)

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	if webtoolsEnabled(s) {
		if names := s.builtinWebToolNames(payload); len(names) > 0 {
			s.handleWebTools(w, r, payload, names)
			return
		}
	}

	chatReq := protocol.ResponsesToChat(payload)
	rawTools, _ := payload["tools"].([]any)
	customNames := protocol.CustomToolNames(rawTools)
	nsMap, _ := chatReq["_qd_tool_ns"].(map[string]string)
	if nsMap == nil {
		if m, ok := chatReq["_qd_tool_ns"].(map[string]any); ok {
			nsMap = map[string]string{}
			for k, v := range m {
				if s, ok := v.(string); ok {
					nsMap[k] = s
				}
			}
		}
	}
	model := firstNonEmptyStr(asStringAny(chatReq["model"]), "auto")
	wantStream, _ := payload["stream"].(bool)

	if blocked := s.modelPrecheck(model, s.requestRealm(r), r); blocked != "" {
		s.writeError(w, http.StatusBadRequest, blocked, "invalid_request_error")
		return
	}

	sessionKey := extractSessionKey(r, chatReq)
	applyContextHeader(r, chatReq)
	allowed := guards.ToolNamesFromPayload(chatReq)
	start := time.Now()

	up, acc, err := s.openUpstreamFor(r, chatReq, sessionKey)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer up.Close()

	holder := &upstream.Holder{}
	if !wantStream {
		result, err := s.aggregateStream(r.Context(), up, chatReq, holder, allowed)
		if err != nil {
			s.recordError(model, upstreamStatus(err), err.Error(),
				int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
			s.writeUpstreamFriendly(w, err)
			return
		}
		obj := protocol.ChatToResponse(result.Object, model, customNames, nsMap)
		s.recordUsage(simpleUsageRowWithStats(chatReq, result, acc, start, s.keyIDForUsage(r)))
		s.writeJSON(w, http.StatusOK, obj)
		return
	}

	sw := s.newSSEWriter(w)
	var frames []guards.Frame
	firstByte := -1
	pumpErr := upstream.Stream(r.Context(), up.Body, holder, func(inner map[string]any) error {
		if firstByte < 0 {
			firstByte = int(time.Since(start).Milliseconds())
		}
		frames = append(frames, guards.ParseFrame(encodeDataLine(inner)))
		return nil
	})
	guarded, guardErr := guards.Process(frames, allowed)

	state := protocol.NewResponsesStreamState(model, customNames, nsMap)
	for _, f := range state.Start() {
		sw.WriteBytes(f)
	}
	for _, f := range guarded {
		if f.Payload == nil {
			continue
		}
		for _, ev := range state.Feed(f.Payload) {
			sw.WriteBytes(ev)
		}
	}

	// Failures take precedence over a normal close: a client must not see a
	// completed response for a turn that produced unusable tool arguments.
	if guardErr != nil {
		if s.toolEchoLenient() {
			guards.NoteLenientTurn()
			s.log.Warn("responses", "lenient tool-echo finish (%s)", guardErr.Kind)
		} else {
			for _, ev := range state.FinishEvents(true, guardErr.Detail) {
				sw.WriteBytes(ev)
			}
			sw.Done()
			s.recordError(model, 502, guardErr.Detail, int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
			return
		}
	}
	if pumpErr != nil {
		msg, _ := friendlyUpstreamError(upstreamStatus(pumpErr), errText(pumpErr))
		for _, ev := range state.FinishEvents(true, msg) {
			sw.WriteBytes(ev)
		}
		sw.Done()
		s.recordError(model, upstreamStatus(pumpErr), errText(pumpErr),
			int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
		return
	}
	for _, ev := range state.FinishEvents(false, "") {
		sw.WriteBytes(ev)
	}
	sw.Done()

	wall := int(time.Since(start).Milliseconds())
	s.recordUsage(usage.Row{
		Model: model, Stream: true, ElapsedMS: &wall, TTFTMS: optInt(firstByte),
		GenMS:        optInt(wall - firstByte),
		PromptTokens: holderPrompt(holder), CompletionTokens: holderCompletion(holder),
		CachedTokens: holderCached(holder), CachedReported: holderCachedReported(holder),
		TotalTokens: holderTotal(holder), Credit: holderCredit(holder),
		Account: acc.UID, Realm: acc.Realm, KeyID: s.keyIDForUsage(r),
		Effort: s.sentEffort,
	})
}

// simpleUsageRowWithStats builds a usage row from an aggregated result.
func simpleUsageRowWithStats(payload map[string]any, result *aggResult,
	acc *accounts.Account, start time.Time, keyID string) usage.Row {
	wall := int(time.Since(start).Milliseconds())
	row := usage.Row{
		Model:  firstNonEmptyStr(asStringAny(payload["model"]), "auto"),
		Stream: false, ElapsedMS: &wall, TTFTMS: &wall,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		CachedTokens: result.CachedTokens, CachedReported: result.CachedReported,
		TotalTokens: result.TotalTokens, Credit: result.Credit,
		ReasoningTokens: result.ReasoningTokens, KeyID: keyID,
	}
	if acc != nil {
		row.Account = acc.UID
		row.Realm = acc.Realm
	}
	return row
}

// handleAnthropicCountTokens estimates tokens locally (no upstream call).
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, protocol.EstimateAnthropicTokens(payload))
}

// handleAnthropic serves POST /v1/messages.
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.payloadObject(w, r)
	if !ok {
		return
	}
	if ok, problem := protocol.ValidateAnthropicRequest(payload); !ok {
		s.writeError(w, http.StatusBadRequest, problem, "invalid_request_error")
		return
	}
	reqRealm := s.requestRealm(r)
	modelHint := firstNonEmptyStr(asStringAny(payload["model"]), "auto")
	// Resolve through the same resolver as chat so a renamed display name still
	// finds its catalog metadata (otherwise the effort tier is silently dropped).
	modelKey := s.resolveModelKey(modelHint, reqRealm)
	meta := s.catalogMetaFor(modelKey, firstNonEmptyStr(reqRealm, s.currentRealm()))
	chatReq := protocol.MessagesToChat(payload, catalogSupportedEfforts(meta), catalogDefaultEffort(meta))
	applyContextHeader(r, chatReq)

	model := firstNonEmptyStr(asStringAny(chatReq["model"]), modelHint)
	wantStream, _ := payload["stream"].(bool)
	sessionKey := extractSessionKey(r, chatReq)
	fp := promptFingerprintAnthropic(chatReq, payload)

	if blocked := s.modelPrecheck(model, reqRealm, r); blocked != "" {
		s.writeError(w, http.StatusBadRequest, blocked, "invalid_request_error")
		return
	}

	// The shape decision is made once and carried down: deciding it twice is what
	// makes the client receive the envelope as text.
	shapeRealm := reqRealm
	if shapeRealm == "" {
		shapeRealm = s.detectModelRealm(model)
	}
	chatReq["_qd_structured"] = protocol.StructuredToolHistoryEnabled(model, modelKey, shapeRealm,
		decodeMessagesForIDs(chatReq["messages"]))

	allowed := guards.ToolNamesFromPayload(chatReq)
	start := time.Now()
	thinkingMode := s.anthropicThinkingMode(r)

	up, acc, err := s.openUpstreamFor(r, chatReq, sessionKey)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer up.Close()
	holder := &upstream.Holder{}

	if !wantStream {
		result, err := s.aggregateStream(r.Context(), up, chatReq, holder, allowed)
		if err != nil {
			s.recordError(model, upstreamStatus(err), err.Error(),
				int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
			s.writeUpstreamFriendly(w, err)
			return
		}
		s.recordUsage(simpleUsageRowWithStats(chatReq, result, acc, start, s.keyIDForUsage(r)))
		s.writeJSON(w, http.StatusOK,
			protocol.ChatToMessages(result.Object, model, thinkingMode))
		return
	}

	sw := s.newSSEWriter(w)
	var frames []guards.Frame
	pumpErr := upstream.Stream(r.Context(), up.Body, holder, func(inner map[string]any) error {
		frames = append(frames, guards.ParseFrame(encodeDataLine(inner)))
		return nil
	})
	guarded, guardErr := guards.Process(frames, allowed)

	state := protocol.NewAnthropicStreamState(model, thinkingMode)
	sw.WriteBytes(state.Start())
	for _, f := range guarded {
		if f.Payload == nil {
			continue
		}
		for _, ev := range state.Feed(f.Payload) {
			sw.WriteBytes(ev)
		}
	}
	if guardErr != nil && !s.toolEchoLenient() {
		msg, _ := friendlyUpstreamError(502, guardErr.Detail)
		sw.WriteBytes(protocol.AnthropicErrorFrame(msg, "api_error"))
		s.recordError(model, 502, guardErr.Detail, int(time.Since(start).Milliseconds()), acc, s.keyIDForUsage(r))
		return
	}
	if guardErr != nil {
		guards.NoteLenientTurn()
		s.log.Warn("anthropic", "lenient tool-echo finish (%s)", guardErr.Kind)
	}
	if pumpErr != nil {
		// This entry deliberately does not reopen the stream: Anthropic clients
		// tolerate a repeated message_start poorly, so an explicit error frame is
		// the honest ending.
		msg, _ := friendlyUpstreamError(upstreamStatus(pumpErr), errText(pumpErr))
		s.log.Error("anthropic", "upstream status %d: %s", upstreamStatus(pumpErr), truncateStr(msg, 200))
		sw.WriteBytes(protocol.AnthropicErrorFrame(msg, "api_error"))
		return
	}
	for _, ev := range state.FinishEvents() {
		sw.WriteBytes(ev)
	}
	s.recordUsage(usage.Row{
		Model: model, Stream: true,
		ElapsedMS:    intPtr(int(time.Since(start).Milliseconds())),
		PromptTokens: holderPrompt(holder), CompletionTokens: holderCompletion(holder),
		CachedTokens: holderCached(holder), CachedReported: holderCachedReported(holder),
		TotalTokens: holderTotal(holder), Credit: holderCredit(holder),
		Account: acc.UID, Realm: acc.Realm, KeyID: s.keyIDForUsage(r),
		Effort: s.sentEffort, MsgsSHA: fp.MsgsSHA, NMsgs: fp.NMsgs,
		SysSrc: fp.SysSrc,
	})
}

// promptFingerprintAnthropic includes the system-source label.
func promptFingerprintAnthropic(chatReq, payload map[string]any) fingerprintExt {
	fp := promptFingerprint(chatReq["messages"],
		protocol.SystemSource(payload), protocol.CountMsgSystem(payload))
	return fingerprintExt{MsgsSHA: fp.MsgsSHA, NMsgs: fp.NMsgs,
		SysSrc: protocol.SystemSource(payload)}
}

// fingerprintExt extends the fingerprint with the system source.
type fingerprintExt struct {
	MsgsSHA string
	NMsgs   int
	SysSrc  string
	NMsgSys int
}

// anthropicThinkingMode resolves the per-key and global thinking policy.
func (s *Server) anthropicThinkingMode(r *http.Request) string {
	if entry := s.keyEntry(r); entry != nil && entry.AnthropicThinking != "" {
		return protocol.ThinkingMode(entry.AnthropicThinking)
	}
	if mode := s.settings.AnthropicThinking(); mode != "" {
		return protocol.ThinkingMode(mode)
	}
	return protocol.ThinkingMode("")
}
