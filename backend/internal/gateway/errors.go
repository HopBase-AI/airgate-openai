package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// 统一错误分类工具（跨 OpenAI / Anthropic 协议共用）。
//
// 新契约：插件用 OutcomeKind 向 Core 表达判决。classifyHTTPFailure / classifyMessage
// 是所有错误路径的唯一分类入口。

// classifyHTTPFailure 把 HTTP 状态码 + 错误文本归一化为 OutcomeKind。
// 返回的 Kind 决定 Core 如何处置账号：
//
//	429 → AccountRateLimited，除非结构化错误明确表示上游 overload
//	401 / 403 → AccountDead（附加消息关键词检查"usage limit" / "rate limit" 等会降级为 RateLimited）
//	400 + 消息含限流关键词 → AccountRateLimited（部分上游用 400 返回 usage_limit_reached）
//	400 + 消息含 disabled/deactivated → AccountDead
//	非 429 的 4xx/5xx + 计费失效文案（account is not active / check your billing）→ AccountDead
//	  （中转常把欠费包在 5xx 里，判 Transient 会反复降级-恢复来回抖）
//	504 → ClientError（SDK 暂无账号中性的不可重放 5xx，借此保留原始 504）
//	529 / 明确 overload 的 5xx → UpstreamTransient（短暂上游容量故障，不处罚账号）
//	其它 5xx → UpstreamTransient
//	其它 4xx → ClientError（客户端请求自己的问题，账号无辜）
func classifyHTTPFailure(statusCode int, message string) sdk.OutcomeKind {
	// A gateway timeout has an unknown execution result and must not be replayed
	// against another account. OutcomeClientError is the SDK's current
	// account-neutral, non-failover passthrough verdict.
	if statusCode == http.StatusGatewayTimeout {
		return sdk.OutcomeClientError
	}
	// Specific rate-limit wording wins over overload prose. Structured error
	// codes/types are handled separately by classifyHTTPFailureBody, where the
	// machine-readable signal has priority over this message fallback.
	if isDefinitiveRateLimitText(message) && (statusCode == 400 || statusCode == 403 || statusCode == 429) {
		return sdk.OutcomeAccountRateLimited
	}
	if isOverloadedText(message) && (statusCode == http.StatusTooManyRequests || statusCode >= 500) {
		return sdk.OutcomeUpstreamTransient
	}
	// 计费失效（欠费/账号未激活）必须先于"try again later"类安抚文案判定：它不会在短
	// 冷却内自愈，且中转常包在 5xx 里返回——按 5xx 判 Transient 会让池账号反复
	// 降级-恢复来回抖一整天（2026-08-08 事故）。判 Dead：普通账号禁用待人工，
	// 池账号走连击软降级。429 除外，保留限流语义。
	if isBillingInactiveText(message) && statusCode >= 400 && statusCode != http.StatusTooManyRequests {
		return sdk.OutcomeAccountDead
	}
	if isTemporaryRateLimitText(message) && (statusCode == 400 || statusCode == 403 || statusCode == 429) {
		return sdk.OutcomeAccountRateLimited
	}
	if statusCode == 529 {
		return sdk.OutcomeUpstreamTransient
	}
	if isDisabledAccountText(message) && (statusCode == 400 || statusCode == 403) {
		return sdk.OutcomeAccountDead
	}
	switch statusCode {
	case 429:
		return sdk.OutcomeAccountRateLimited
	case 401, 403:
		return sdk.OutcomeAccountDead
	}
	if statusCode >= 500 {
		return sdk.OutcomeUpstreamTransient
	}
	if statusCode >= 400 {
		return sdk.OutcomeClientError
	}
	return sdk.OutcomeSuccess
}

// classifyAnthropicBody 从 Anthropic 错误响应体（400 可能是账号级）归一化 OutcomeKind。
func classifyAnthropicBody(statusCode int, body []byte) sdk.OutcomeKind {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = string(body)
	}
	return classifyHTTPFailureBody(statusCode, body, msg)
}

// classifyHTTPFailureBody gives machine-readable error.code/error.type
// precedence over prose and transport status. Providers sometimes return an
// overload inside HTTP 429, while real credential limits may mention an
// overloaded model in their human-readable message.
func classifyHTTPFailureBody(statusCode int, body []byte, fallback string) sdk.OutcomeKind {
	if statusCode != http.StatusGatewayTimeout {
		for _, path := range []string{"error.code", "response.error.code", "code", "error.type", "response.error.type", "type"} {
			if kind, ok := classifyStructuredHTTPFailure(statusCode, gjson.GetBytes(body, path).String()); ok {
				return kind
			}
		}
	}
	return classifyHTTPFailure(statusCode, failureClassificationText(body, fallback))
}

func classifyStructuredHTTPFailure(statusCode int, signal string) (sdk.OutcomeKind, bool) {
	signal = strings.TrimSpace(signal)
	if signal == "" {
		return sdk.OutcomeUnknown, false
	}
	if isOverloadMachineSignal(signal) && (statusCode == http.StatusTooManyRequests || statusCode >= 500) {
		return sdk.OutcomeUpstreamTransient, true
	}
	if isBillingDeadMachineSignal(signal) && statusCode >= 400 && statusCode != http.StatusTooManyRequests {
		return sdk.OutcomeAccountDead, true
	}
	if isRateLimitMachineSignal(signal) && (statusCode == 400 || statusCode == 403 || statusCode == 429) {
		return sdk.OutcomeAccountRateLimited, true
	}
	if isClientRequestMachineSignal(signal) && statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError {
		return sdk.OutcomeClientError, true
	}
	return sdk.OutcomeUnknown, false
}

// failureClassificationText preserves machine-readable provider semantics for
// HTTP failure paths. Relays are inconsistent about transport status, so the
// structured type/code must be considered before falling back to prose.
func failureClassificationText(body []byte, fallback string) string {
	parts := make([]string, 0, 10)
	seen := make(map[string]struct{}, 10)
	appendPart := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		parts = append(parts, value)
	}

	for _, path := range []string{
		"error.type", "error.code", "error.message",
		"response.error.type", "response.error.code", "response.error.message",
		"type", "code", "message",
	} {
		appendPart(gjson.GetBytes(body, path).String())
	}
	if len(parts) == 0 && len(body) > 0 {
		appendPart(truncate(string(body), 500))
	}
	appendPart(fallback)
	return strings.Join(parts, " ")
}

// isBillingInactiveText 上游把"账号未激活/欠费"类计费失效包在任意状态码（常见 5xx 包装）
// 里返回的文案。真实样本：ndplaygames/bigsnake 中转 502 "Your account is not active,
// please check your billing details on our website."（2026-08-08）。
func isBillingInactiveText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "account is not active") ||
		strings.Contains(combined, "check your billing") ||
		strings.Contains(combined, "billing_not_active") ||
		strings.Contains(combined, "account_not_active")
}

// isBillingDeadMachineSignal 结构化 error.code/type 明确表示账号计费失效。
// 与限流信号分开：限流会自愈，计费失效需要人工充值/激活。
func isBillingDeadMachineSignal(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "billing_not_active", "account_not_active", "billing_inactive":
		return true
	}
	return false
}

func isDefinitiveRateLimitText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "usage limit") ||
		strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "too many requests") ||
		strings.Contains(combined, "quota exceeded") ||
		strings.Contains(combined, "insufficient quota") ||
		strings.Contains(combined, "billing hard limit") ||
		strings.Contains(combined, "slow down") ||
		strings.Contains(combined, "request limit exceeded")
}

func isOverloadMachineSignal(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "server_is_overloaded", "server_overloaded", "service_overloaded", "model_overloaded", "engine_overloaded", "overloaded_error":
		return true
	}
	return false
}

func isRateLimitMachineSignal(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "rate_limit_error", "rate_limit_exceeded", "usage_limit_reached", "too_many_requests", "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached", "slow_down":
		return true
	}
	return false
}

func isClientRequestMachineSignal(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "invalid_request_error", "invalid_request", "unknown_parameter", "invalid_parameter", "unsupported_parameter", "context_length_exceeded", "input_too_long", "content_policy_violation", "model_not_found", "model_not_supported":
		return true
	}
	return false
}

func isTemporaryRateLimitText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return isDefinitiveRateLimitText(combined) ||
		strings.Contains(combined, "try again later") ||
		strings.Contains(combined, "try again in") ||
		strings.Contains(combined, "retry after")
}

func isDisabledAccountText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "disabled") ||
		strings.Contains(combined, "deactivated") ||
		strings.Contains(combined, "suspended")
}

// isDefinitiveCredentialFailureText only matches errors that establish the
// credential or account itself is unusable. Generic 403/forbidden text is
// intentionally excluded because upstream risk controls are recoverable.
func isDefinitiveCredentialFailureText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	for _, signal := range []string{
		"authentication_error",
		"auth_error",
		"invalid_access_token",
		"invalid_token",
		"token_expired",
		"account_deactivated",
		"account_disabled",
		"account_suspended",
		"invalid_grant",
	} {
		if strings.Contains(combined, signal) {
			return true
		}
	}
	if isDisabledAccountText(combined) &&
		(strings.Contains(combined, "account") ||
			strings.Contains(combined, "organization") ||
			strings.Contains(combined, "workspace") ||
			strings.Contains(combined, "credential")) {
		return true
	}
	for _, phrase := range []string{
		"authentication failed",
		"not authenticated",
		"invalid access token",
		"access token is invalid",
		"access token expired",
		"access token has expired",
		"expired access token",
		"token is invalid",
		"token has expired",
	} {
		if strings.Contains(combined, phrase) {
			return true
		}
	}
	return false
}

func isModelUnsupportedText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	directSignals := []string{
		"model_not_found",
		"model_not_supported",
		"invalid_model",
		"unsupported_model",
		"no such model",
	}
	for _, signal := range directSignals {
		if strings.Contains(combined, signal) {
			return true
		}
	}
	if !strings.Contains(combined, "model") {
		return false
	}
	return strings.Contains(combined, "not supported") ||
		strings.Contains(combined, "unsupported") ||
		strings.Contains(combined, "does not support") ||
		strings.Contains(combined, "does not exist") ||
		strings.Contains(combined, "not found") ||
		strings.Contains(combined, "not available") ||
		strings.Contains(combined, "invalid model")
}

// anthropicErrorType 根据 HTTP 状态码返回 Anthropic 错误类型。
func anthropicErrorType(statusCode int) string {
	switch statusCode {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 422:
		return "invalid_model_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func anthropicErrorJSON(errType, message string) []byte {
	return anthropicErrorJSONWithCode(errType, "", message)
}

// anthropicErrorJSONWithCode 透传可机器读的 error.code（例如 safety_rejected）。
// 仅当 code 非空时才输出该字段，避免影响既有不带 code 的回包。
func anthropicErrorJSONWithCode(errType, code, message string) []byte {
	out := `{"type":"error","error":{"type":"","message":""}}`
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	if code != "" {
		out, _ = sjson.Set(out, "error.code", code)
	}
	return []byte(out)
}

func openAIErrorJSON(errType, code, message string) []byte {
	out := `{"error":{"message":"","type":"","code":""}}`
	out, _ = sjson.Set(out, "error.message", message)
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.code", code)
	return []byte(out)
}

func openAIErrorTypeForStatus(statusCode int) string {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "rate_limit_error"
	case statusCode >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// extractRetryAfterHeader 从响应头提取 Retry-After。
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := strings.TrimSpace(headers.Get("Retry-After"))
	if val == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(val, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(val); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
		return 0
	}
	if delay := parseRetryDelay(val); delay > 0 {
		return delay
	}
	// Preserve the historical decimal-seconds fallback used by providers that
	// send non-standard values such as "1.5".
	return parseRetryDelay("try again in " + val + "s")
}

// truncate 截断字符串到指定长度。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
