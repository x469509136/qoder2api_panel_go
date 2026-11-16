package server

import (
	"errors"
	"strings"

	"github.com/xzs/bridge_qoder/internal/upstream"
)

// Error classification markers.
var (
	clientFaultMarkers = []string{
		"invalid_parameter_error", "invalid_request_error", "authentication_error",
		"permission_error", `"Range of `,
		"DataInspectionFailed", "inappropriate content",
		"input text data may contain", "ContentFilter", "SensitiveContent",
	}
	contentPolicyMarkers = []string{
		"DataInspectionFailed", "inappropriate content",
		"input text data may contain", "ContentFilter", "SensitiveContent",
	}
)

// upstreamDetail extracts the raw upstream body from a typed error so the
// message layer can match on it directly instead of on the wrapper's text.
func upstreamDetail(err error) string {
	var se *upstream.StatusError
	if errors.As(err, &se) {
		return se.Detail
	}
	var ee *upstream.EnvelopeStatusError
	if errors.As(err, &ee) {
		return ee.Detail
	}
	var qe *upstream.QueuedError
	if errors.As(err, &qe) {
		return qe.Detail
	}
	return errText(err)
}

// friendlyUpstreamError turns an upstream error into an actionable message.
//
// Every branch exists because a raw upstream body made users chase the wrong
// thing: queueing looked like a bad key, an exhausted plan looked like a bad key,
// and a daily cap surfaced as a bare 403.
func friendlyUpstreamError(code int, detail string) (string, string) {
	detail = strings.TrimSpace(detail)
	safe := neutralizeAuthCodes(detail)

	// A local "no usable account" is an operator problem, not an upstream fault;
	// reporting it as a transient upstream error sends people to the wrong place.
	if strings.HasPrefix(detail, "no usable account") {
		return "网关没有可用账号（全部被禁用、冷却中或凭证已过期）。" +
			"请在看板 / 账号页启用或添加一个账号。详情：" + detail, "no_usable_account"
	}
	if strings.HasPrefix(detail, "API Key / routing account scope") {
		return "当前 API Key / 路由规则的账号范围为空，没有可派发的账号。" +
			"请检查该 Key 的账号限制或路由规则。详情：" + detail, "empty_account_scope"
	}
	if strings.HasPrefix(detail, "routing configuration error") {
		return "本地路由配置无效：" + strings.TrimPrefix(detail, "routing configuration error: ") +
			"。请在看板 /admin/routing 修正后重试。", "routing_config_error"
	}
	if strings.HasPrefix(detail, "model is only available in realm") {
		return "该模型只在另一个区域提供，网关拒绝跨区回退。请改用对应区域的出口。" +
			"详情：" + detail, "cross_realm_refused"
	}

	if upstreamIsImageConversionTimeout(detail) {
		return "上游图片转换超时 (HTTP " + itoa(code) + ")：图片转换服务在时限内未完成。" +
			"这不是 Key 失效；网关保留图片，限制自动重试且不冷却/轮换账号。" +
			"可稍后重试；若重复出现，请分批发送图片或检查原图格式。上游详情：" +
			safeOr(safe, "(无详情)"), "upstream_image_conversion_timeout"
	}
	if isQueueSignal(detail) && (code == 403 || code == 503) {
		return "上游模型排队中（10605 / RESOURCE_LIMIT）：请稍后重试，或换一个模型/区域。" +
				"这不是你的 Key、账号或网络问题。上游详情：" + safeOr(safe, "(无详情)"),
			"upstream_queued"
	}
	quotaKind := quotaCooldownKind(detail)
	if quotaKind == "exhausted" {
		return "上游拒绝本次模型额度/套餐权限（112）：这不等于账号总余额为零。" +
			"请核对实际出口账号、当前模型可用额度与套餐权限；不要只看两区合计余额。" +
			"上游详情：" + safeOr(safe, "(无详情)"), "quota_exhausted"
	}
	if quotaKind == "daily" {
		return "上游账号今日请求次数已达上限（110）：本地 0 点后恢复，或启用其它账号。" +
			"这不是你的 Key 问题。上游详情：" + safeOr(safe, "(无详情)"), "quota_daily_limit"
	}
	if strings.Contains(itoa(code), "430") {
		return "客户端/网关版本过低（430）：上游要求升级后才能继续。" +
			"请升级到最新版；这不是账号或网络问题。", "version_too_low"
	}
	if strings.Contains(detail, "payload too large") {
		return "请求体过大（超过上游 30MB 限制）：网关已在本地拦下、未发送。" +
			"请减少图片数量/尺寸（或改用更小的图）后重试。详情：" +
			truncateStr(detail, 160), "payload_too_large"
	}
	if isQueueSignal(detail) {
		return "上游排队中（10605 / RESOURCE_LIMIT）：已按 retryAfterSeconds 等待并重试，" +
			"仍拿不到可用资源。请稍后重试；若频繁出现，说明该模型当前排队严重。" +
			"上游详情：" + safeOr(safe, "(无详情)"), "upstream_queued"
	}
	if strings.Contains(detail, "pricingUrl") {
		return "上游返回套餐购买/权限入口，但未明确报告余额耗尽。" +
			"请核对本次模型和出口账号的套餐权限。上游详情：" +
			truncateStr(safe, 200), "upstream_plan_rejected"
	}
	if strings.Contains(detail, "missing_tool_name") || strings.Contains(detail, "missing_tool_call") {
		return "上游工具调用缺少名称或实际调用，已阻止伪装为正常结束；请重试该轮。",
			"upstream_incomplete_tool_call"
	}
	if strings.Contains(detail, "reasoning_only") || strings.Contains(detail, "empty_tool_turn") {
		return "上游只返回思考或空响应，没有正文及可执行工具调用；本轮未完成，" +
			"未把思考改成正文。", "upstream_empty_response"
	}
	if strings.Contains(detail, "truncated_arguments") {
		return "上游工具参数被截断或不是合法 JSON；本轮未完整结束，请重试，" +
			"不会重复已发送的输出。", "upstream_incomplete_tool_call"
	}
	if strings.Contains(detail, "invalid JSON arguments") {
		return "上游生成的工具调用参数不是合法 JSON，本轮**未执行任何工具**；" +
			"网关已按协议返回失败，请重试该轮。上游详情：" +
			safeOr(truncateStr(detail, 200), "(无详情)"), "invalid_tool_arguments"
	}
	for _, m := range contentPolicyMarkers {
		if strings.Contains(detail, m) {
			return "上游内容安全审核未通过 (DataInspectionFailed)：输入可能含不当内容，" +
				"属确定性拒绝、重试无效。请检查/缩短输入（系统提示词、超长历史、" +
				"工具定义或粘贴的代码/文本）后重试。上游详情：" +
				truncateStr(detail, 300), "content_policy_rejected"
		}
	}
	if upstreamIsFirstTokenTimeout(detail) {
		return "上游首字/推理超时 (HTTP " + itoa(code) + ")：本次未拿到可用响应；" +
			"不代表 Key 或额度失效。网关不再针对该超时重复请求同一账号。上游详情：" +
			truncateStr(safe, 200), "upstream_transient_error"
	}
	if isTransientUpstreamCode(code, detail) ||
		(strings.Contains(detail, "provider_error") && !strings.Contains(detail, "invalid_")) {
		return "上游瞬时故障 (HTTP " + itoa(code) + ")：网关已对同账号自动重试仍失败，" +
				"请稍后重试。上游详情：" + safeOr(truncateStr(detail, 300), "(无详情)"),
			"upstream_transient_error"
	}
	return "upstream " + itoa(code) + ": " + safeOr(detail, "(无详情)"), "upstream_error"
}

// neutralizeAuthCodes rewrites a bare 401/403 in prose so clients do not classify
// an unrelated failure as a credential problem.
var authLikeCodeRe = strings.NewReplacer(
	" 401 ", " HTTP_401 ", " 403 ", " HTTP_403 ",
	"(401)", "(HTTP_401)", "(403)", "(HTTP_403)",
)

func neutralizeAuthCodes(text string) string {
	if text == "" {
		return text
	}
	out := authLikeCodeRe.Replace(" " + text + " ")
	return strings.TrimSpace(out)
}

func isQueueSignal(detail string) bool {
	return strings.Contains(detail, "10605") || strings.Contains(detail, "isQueued") ||
		strings.Contains(detail, "RESOURCE_LIMIT")
}

// quotaCooldownKind classifies a quota rejection.
func quotaCooldownKind(detail string) string {
	low := strings.ToLower(detail)
	if strings.Contains(detail, "Billing daily count exceeded") ||
		strings.Contains(detail, `"code":110`) || strings.Contains(detail, `"code": 110`) {
		return "daily"
	}
	if strings.Contains(detail, "112") &&
		(strings.Contains(detail, "pricingUrl") || strings.Contains(detail, "code")) {
		return "exhausted"
	}
	if strings.Contains(low, "quota") && strings.Contains(low, "exceed") {
		return "exhausted"
	}
	return ""
}

// isTransientUpstreamCode mirrors upstream.IsTransientUpstream for the message
// layer (the gateway's own classifier lives in the upstream package).
func isTransientUpstreamCode(code int, detail string) bool {
	if code == 401 || code == 403 || code == 429 {
		return false
	}
	for _, m := range clientFaultMarkers {
		if strings.Contains(detail, m) {
			return false
		}
	}
	if code == 418 || code >= 500 {
		return true
	}
	return false
}

func upstreamIsImageConversionTimeout(detail string) bool {
	low := strings.ToLower(detail)
	return strings.Contains(low, "image conversion timeout") ||
		strings.Contains(low, "image_conversion_timeout") ||
		strings.Contains(low, "image conversion timed out")
}

func upstreamIsFirstTokenTimeout(detail string) bool {
	low := strings.ToLower(detail)
	return strings.Contains(low, "first token timeout") ||
		strings.Contains(low, "first_token_timeout") ||
		strings.Contains(low, "upstream timeout")
}

func safeOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// parseMaxTokensRange extracts N from the upstream's
// "Range of max_tokens should be [1, N]" validation message.
func parseMaxTokensRange(detail string) int {
	m := rangeRe.FindStringSubmatch(detail)
	if m == nil {
		return 0
	}
	var n int
	if _, err := fmtSscanf(m[1], &n); err != nil {
		return 0
	}
	return n
}
