package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

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
//	400 / 403 + 消息明确说"账号/密钥被停用"→ AccountDead（只认完整语义，见 isDisabledAccountText）
//	4xx（401 / 429 除外）+ 参数校验语义（未知字段 / 非法枚举值 / 这把 key 没开通某个内置工具）
//	  → ClientError，且不参与账号存活判定（见 isRequestScopedFailureText）
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
	// 参数校验类 4xx 绝不参与账号存活判定。上游明确指向请求本身（未知字段、非法枚举值、
	// 这把 key 没开通某个内置工具）的错误，换任何账号重放都一模一样：判 AccountDead 只会
	// 让客户拿到对不上的 503，并在账号上写一条假故障事件污染监控与调度判决。
	// 2026-09-18 生产实证：客户端一个 thinking.type=auto 就把组 55 唯一账号打成
	// account_dead（文案里的合法值枚举 `enabled`, `disabled` 命中裸子串），
	// tools[].type=mcp 的 403「无权使用内置工具」走 401/403 分支同样被判死。
	// 401 / 429 不在此列：它们的语义本来就与请求内容无关。
	if isRequestScopedFailureText(message) &&
		statusCode >= 400 && statusCode < 500 &&
		statusCode != http.StatusUnauthorized && statusCode != http.StatusTooManyRequests &&
		!isDefinitiveCredentialFailureText(message) && !isBillingInactiveText(message) {
		return sdk.OutcomeClientError
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
	// 中转会把确定性的客户端会话状态错误（function call 配对丢失等）包成 5xx 转发。
	// 换账号重放必然复现，降级账号只会放大故障——按客户端错误透传，不 failover。
	if statusCode >= 500 && isConversationStateText(message) {
		return sdk.OutcomeClientError
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

// enumerationMarkers 参数校验错误在列举合法值前用的引导语。上游一旦开始列合法值，
// 后面的词就只是**字面量**，不再是对账号状态的陈述。
var enumerationMarkers = []string{
	"expected one of",
	"must be one of",
	"should be one of",
	"one of the following",
	"allowed values",
	"valid values",
	"supported values",
	"accepted values",
	"possible values",
	"valid options",
	"available options",
	"choices are",
	"enum values",
}

// stripEchoedLiterals 去掉上游在错误文案里**回显**的字面量，只留下真正的陈述句：
//
//	反引号包裹的片段（火山方舟用它包字段名与枚举值）；
//	enumerationMarkers 之后直到句末的合法值清单（保留引导语本身，它是"这是参数错误"的正信号）。
//
// 2026-09-18 生产事故根因：“… expected one of `adaptive`, `enabled`, `disabled` “ 里的
// `disabled` 被裸子串当成"账号被停用"，一个客户端参数错误把唯一账号判成 account_dead。
func stripEchoedLiterals(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, "`") {
		var b strings.Builder
		b.Grow(len(s))
		inQuote := false
		for _, r := range s {
			if r == '`' {
				inQuote = !inQuote
				b.WriteByte(' ')
				continue
			}
			if inQuote {
				continue
			}
			b.WriteRune(r)
		}
		// 反引号落单（未闭合）时保守回退，避免把整段正文吃掉。
		if !inQuote {
			s = b.String()
		}
	}
	lower := strings.ToLower(s)
	// ToLower 对极少数字符会改变字节长度（如 U+0130），下标就对不上了；
	// 这种情况直接跳过枚举裁剪，反引号剥离已经兜住了绝大多数样本。
	if len(lower) != len(s) {
		return s
	}
	for _, marker := range enumerationMarkers {
		idx := strings.Index(lower, marker)
		if idx < 0 {
			continue
		}
		cut := idx + len(marker)
		end := len(s)
		if rel := strings.IndexAny(s[cut:], ".;\n"); rel >= 0 {
			end = cut + rel
		}
		s = s[:cut] + " " + s[end:]
		lower = strings.ToLower(s)
		if len(lower) != len(s) {
			return s
		}
	}
	return s
}

// classificationTokens 把上游错误文本切成小写词元：非字母数字一律当分隔符，于是
// account_disabled / account-disabled / "Account Disabled" 归一成同一串词元，
// 短语匹配不必为每种写法各写一条 Contains。切词前先剥掉回显字面量。
func classificationTokens(parts ...string) []string {
	combined := stripEchoedLiterals(strings.Join(parts, " "))
	if strings.TrimSpace(combined) == "" {
		return nil
	}
	return strings.FieldsFunc(strings.ToLower(combined), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsPhrase 判断 tokens 里是否出现连续的 phrase。
func containsPhrase(tokens []string, phrase ...string) bool {
	if len(phrase) == 0 || len(tokens) < len(phrase) {
		return false
	}
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		match := true
		for j, want := range phrase {
			if tokens[i+j] != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func containsAnyPhrase(tokens []string, phrases [][]string) bool {
	for _, phrase := range phrases {
		if containsPhrase(tokens, phrase...) {
			return true
		}
	}
	return false
}

// isBillingInactiveText 上游把"账号未激活/欠费"类计费失效包在任意状态码（常见 5xx 包装）
// 里返回的文案。真实样本：ndplaygames/bigsnake 中转 502 "Your account is not active,
// please check your billing details on our website."（2026-08-08）。
func isBillingInactiveText(parts ...string) bool {
	return containsAnyPhrase(classificationTokens(parts...), [][]string{
		{"account", "is", "not", "active"},
		{"account", "not", "active"},
		{"check", "your", "billing"},
		{"billing", "not", "active"},
		{"billing", "inactive"},
	})
}

// isBillingDeadMachineSignal 结构化 error.code/type 明确表示账号计费失效。
// 与限流信号分开：限流会自愈，计费失效需要人工充值/激活。
func isBillingDeadMachineSignal(value string) bool {
	switch normalizeMachineSignal(value) {
	case "billingnotactive", "accountnotactive", "billinginactive":
		return true
	}
	return false
}

// isDefinitiveRateLimitText 与 isDisabledAccountText 同族：都是"文案命中即处罚账号"，
// 所以同样按词元短语匹配、并先剥掉回显字面量——否则 "expected one of `rate_limit`, …"
// 这类参数校验文案会被读成真的撞了限流，把一次客户端错误记成账号限流。
func isDefinitiveRateLimitText(parts ...string) bool {
	return containsAnyPhrase(classificationTokens(parts...), [][]string{
		{"usage", "limit"},
		{"rate", "limit"},
		{"too", "many", "requests"},
		{"quota", "exceeded"},
		{"insufficient", "quota"},
		{"billing", "hard", "limit"},
		{"slow", "down"},
		{"request", "limit", "exceeded"},
	})
}

func isOverloadMachineSignal(value string) bool {
	switch normalizeMachineSignal(value) {
	case "serverisoverloaded", "serveroverloaded", "serviceoverloaded", "modeloverloaded", "engineoverloaded", "overloadederror":
		return true
	}
	return false
}

func isRateLimitMachineSignal(value string) bool {
	switch normalizeMachineSignal(value) {
	case "ratelimiterror", "ratelimitexceeded", "usagelimitreached", "toomanyrequests", "insufficientquota", "quotaexceeded", "billinghardlimitreached", "slowdown":
		return true
	}
	return false
}

// normalizeMachineSignal 抹平上游 error.code/type 的书写风格：下划线、连字符、空格、
// 大小写都去掉。火山方舟写 "InvalidParameter"，OpenAI 写 "invalid_parameter"，
// 归一化后是同一个信号；不归一化就只能靠散文兜底，而散文兜底正是误判的来源。
func normalizeMachineSignal(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isClientRequestMachineSignal(value string) bool {
	switch normalizeMachineSignal(value) {
	case "invalidrequesterror", "invalidrequest", "unknownparameter", "invalidparameter",
		"unsupportedparameter", "contextlengthexceeded", "inputtoolong",
		"contentpolicyviolation", "modelnotfound", "modelnotsupported",
		// 火山方舟（ark）的参数校验族：InvalidParameter / MissingParameter / InvalidArgument。
		// 不收 BadRequest —— 那只是状态码的同义词，覆盖面太宽会盖住真正的账号级 4xx。
		"missingparameter", "missingrequiredparameter", "invalidparametervalue",
		"invalidargument", "unknownfield", "unknownvariant":
		return true
	}
	return false
}

// isConversationStateText 请求携带的会话状态与上游不匹配（tool call 配对丢失等）。
// 这类错误由请求 payload 决定，与所选账号无关。真实样本：bigsnake 中转 502
// "No tool call found for function call output with call_id call_…"（2026-08-12）。
func isConversationStateText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "no tool call found for function call output") ||
		strings.Contains(combined, "no tool output found for function call")
}

// isTemporaryRateLimitText 同样会处罚账号（AccountRateLimited），所以与
// isDefinitiveRateLimitText 用同一份净化后的文本，别让回显字面量把
// 一次参数错误记成限流。
func isTemporaryRateLimitText(parts ...string) bool {
	combined := strings.ToLower(stripEchoedLiterals(strings.Join(parts, " ")))
	if strings.TrimSpace(combined) == "" {
		return false
	}
	return isDefinitiveRateLimitText(combined) ||
		strings.Contains(combined, "try again later") ||
		strings.Contains(combined, "try again in") ||
		strings.Contains(combined, "retry after")
}

// accountStateWords 「主体被停用」的状态词。只有这几个词本身不构成判决依据：
// 必须挂在一个账号级主语上（accountStateSubjects），且附近没有请求级主语
// （requestScopedSubjects）。
var accountStateWords = []string{"disabled", "deactivated", "suspended", "banned"}

// accountStateSubjects 状态词的合法主语：这些东西被停用才说明凭证本身不可用。
// 刻意用复合词（api key / access token）而不是裸 key / token —— 裸词会被
// prompt_cache_key 之类的**参数名**命中。
var accountStateSubjects = [][]string{
	{"account"}, {"accounts"},
	{"organization"}, {"organisation"}, {"org"},
	{"workspace"}, {"tenant"}, {"subscription"},
	{"credential"}, {"credentials"},
	{"api", "key"}, {"api", "keys"}, {"apikey"}, {"apikeys"},
	{"access", "key"}, {"secret", "key"}, {"access", "token"}, {"auth", "token"},
}

// requestScopedSubjects 请求级主语。「这个模型/工具/参数在你账号上没开」是**每请求的
// 能力问题**，不是账号死亡：判死会给客户一个对不上的 503，还污染账号事件流。
// 状态词附近一旦出现这些词，就否决账号级判决。
var requestScopedSubjects = []string{
	"model", "models", "parameter", "parameters", "param", "params",
	"field", "fields", "property", "argument", "arguments", "option", "options",
	"value", "values", "variant", "variants", "type", "types", "enum",
	"tool", "tools", "function", "functions", "feature", "features",
	"capability", "capabilities", "endpoint", "modality", "schema",
}

// accountStateWindow 账号级主语与状态词之间允许隔多少个词。7 个词能覆盖
// "your account has temporarily been suspended" 这类插入语，又不至于把
// 两句话的无关名词凑成一条判决。
//
// requestScopedWindow 刻意更窄：请求级主语只有紧贴状态词时才算否决
// （"this model is disabled" / "the parameter `x` is disabled"），
// 否则 "Your API key has been disabled. Please check the request parameters."
// 这种句子会被句尾的 parameters 一票否决，把真的死号放回池子。
const (
	accountStateWindow  = 7
	requestScopedWindow = 3
)

// isDisabledAccountText 只在文本**完整表达**了"账号/密钥/组织被停用、封禁"时为真。
//
// 反例（2026-09-18 生产）：火山方舟参数校验失败会把合法值列进文案——
// “Invalid request at 'thinking.type': unknown variant `auto`, expected one of
// `adaptive`, `enabled`, `disabled` “ ——老实现用裸 strings.Contains(…, "disabled")
// 把这条纯客户端错误判成 AccountDead：客户收到误导性 503「当前没有可用的服务账号」，
// 唯一账号上多出一条假 upstream_error 事件，还与真上游抖动共用连击计数器。
// 现在的口径：剥掉回显字面量 → 切词 → 状态词必须挂在账号级主语上、
// 且近旁没有模型/工具/参数这类请求级主语。
func isDisabledAccountText(parts ...string) bool {
	tokens := classificationTokens(parts...)
	if len(tokens) == 0 {
		return false
	}
	for i, tok := range tokens {
		if !containsString(accountStateWords, tok) {
			continue
		}
		if containsAnyString(tokenWindow(tokens, i, requestScopedWindow), requestScopedSubjects) {
			continue
		}
		if containsAnyPhrase(tokenWindow(tokens, i, accountStateWindow), accountStateSubjects) {
			return true
		}
	}
	return false
}

// requestScopedFailurePhrases 上游明确在说"错在这次请求"的措辞。
// 这些文案换任何账号重放都一模一样，永远不该变成账号存活判决。
var requestScopedFailurePhrases = [][]string{
	// 火山方舟 serde 层：`Invalid request at 'thinking.type': unknown variant `auto`…`
	{"invalid", "request", "at"},
	{"unknown", "variant"},
	{"unknown", "field"},
	{"unknown", "parameter"},
	{"unknown", "type"},
	{"unknown", "tool", "type"},
	{"unsupported", "parameter"},
	{"unsupported", "tool", "type"},
	{"unrecognized", "request", "argument"},
	{"missing", "required", "parameter"},
	{"invalid", "parameter"},
	{"invalid", "request", "error"},
	// 火山方舟参数校验：`The parameter `temperature` specified in the request are not valid…`
	{"specified", "in", "the", "request", "are", "not", "valid"},
	{"specified", "in", "the", "request", "is", "not", "valid"},
	// 合法值枚举引导语本身就是"这是参数错误"的正信号（值清单已被 stripEchoedLiterals 剥掉）。
	{"expected", "one", "of"},
	{"must", "be", "one", "of"},
	// 火山方舟 403：`The request failed because you do not have access to the built in tool.`
	// 语义是"这把 key 没开通这个内置工具"——请求能力问题，不是账号死亡。
	{"built", "in", "tool"},
	{"builtin", "tool"},
}

// isRequestScopedFailureText 判断上游 4xx 是否明确指向请求本身（参数/字段/工具类型），
// 与所选账号无关。命中即按 ClientError 原样回放，绝不写账号故障事件。
func isRequestScopedFailureText(parts ...string) bool {
	return containsAnyPhrase(classificationTokens(parts...), requestScopedFailurePhrases)
}

// tokenWindow 取 tokens[center] 前后各 width 个词（含 center 本身）。
func tokenWindow(tokens []string, center, width int) []string {
	lo := center - width
	if lo < 0 {
		lo = 0
	}
	hi := center + width + 1
	if hi > len(tokens) {
		hi = len(tokens)
	}
	return tokens[lo:hi]
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func containsAnyString(tokens []string, wanted []string) bool {
	for _, tok := range tokens {
		if containsString(wanted, tok) {
			return true
		}
	}
	return false
}

// isDefinitiveCredentialFailureText only matches errors that establish the
// credential or account itself is unusable. Generic 403/forbidden text is
// intentionally excluded because upstream risk controls are recoverable.
//
// 2026-09-18 收紧：匹配前先剥掉上游回显的字面量（stripEchoedLiterals），否则
// 参数校验错误列出的合法值清单会凭空判死一个账号；"主体被停用"那一支改由
// 收紧后的 isDisabledAccountText 判定（它自己要求主语与状态词邻近），
// 不再用"另外文本里出现过 account"这种旧近似。
//
// 下面两张表刻意保持子串匹配：上半是上游的**机器码**（下划线形制，参数枚举里
// 不会出现），下半是完整的英文陈述句。把它们改成词元短语会把
// "auth token expired" 这类网页端风控提示错升成凭证永久失效（本可 30s 冷却自愈）。
func isDefinitiveCredentialFailureText(parts ...string) bool {
	combined := strings.ToLower(stripEchoedLiterals(strings.Join(parts, " ")))
	if strings.TrimSpace(combined) == "" {
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
		"account_banned",
		"invalid_grant",
	} {
		if strings.Contains(combined, signal) {
			return true
		}
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
	return isDisabledAccountText(combined)
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
