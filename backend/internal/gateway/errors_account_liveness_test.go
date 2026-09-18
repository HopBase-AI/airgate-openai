package gateway

// 2026-09-18 生产实证（DeepSeek V4.1 Flash 发布后复测，组 55 单供给账号 124）：
// 客户端一个非法参数值就能把唯一账号判成 account_dead，客户收到误导性 503
// 「当前没有可用的服务账号」，账号上还多出一条假 upstream_error 事件
// （account_events 37514 / 37600）——假事件会淹没真故障，并与真上游抖动
// 共用连击计数器，把软降级窗口提前触发。
//
// 本文件钉死两件事：
//  1. 参数校验类 4xx（含 403「无权使用内置工具」）一律 ClientError，不碰账号存活；
//  2. 真正的「账号/密钥被停用、欠费」仍然判死——防止修过头。
//
// 文案全部逐字取自复测报告，勿改写。

import (
	"strings"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// 火山方舟真实文案。Request id 尾巴刻意保留：线上就是这么回放的。
const (
	arkThinkingTypeInvalid = "Invalid request at 'thinking.type': unknown variant `auto`, " +
		"expected one of `adaptive`, `enabled`, `disabled` Request id: 021789696798712ab3c4d5e6f"
	arkBuiltInToolForbidden = "The request failed because you do not have access to the built in tool. " +
		"Request id: 021789697538291ab3c4d5e6f"
	arkTemperatureOutOfRange = "The parameter `temperature` specified in the request are not valid: " +
		"decimal above maximum value, expected a value <= 2, but got 5.000000 instead. " +
		"Request id: 021789696339537ec6553b2bbb2bcb1bdf9627ab55f9f64f17bfe"
	arkUnknownInputType = "The parameter `input.type` specified in the request are not valid: " +
		"unknown type: local_shell_call. Request id: 021789696212345ab3c4d5e6f"
	arkUnknownToolType = "unknown tool type: local_shell"
	arkMaxOutputTokens = "max_output_tokens must be less than or equal to 393216 " +
		"Request id: 021789696615067ab3c4d5e6f"
)

// TestClassifyHTTPFailureParameterValidationNeverKillsAccount 参数校验类 4xx 不参与账号存活判定。
func TestClassifyHTTPFailureParameterValidationNeverKillsAccount(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
	}{
		// 事故原样本：合法值枚举里的 `disabled` 曾命中裸子串 → account_dead。
		{"ark thinking.type unknown variant 400", 400, arkThinkingTypeInvalid},
		{"ark thinking.type unknown variant 403", 403, arkThinkingTypeInvalid},
		// 第二条误导路径：403「这把 key 没开通内置工具」是请求能力问题，不是账号死。
		{"ark built-in tool forbidden 403", 403, arkBuiltInToolForbidden},
		{"ark temperature out of range", 400, arkTemperatureOutOfRange},
		{"ark unknown input type", 400, arkUnknownInputType},
		{"ark unknown tool type", 400, arkUnknownToolType},
		{"ark max_output_tokens ceiling", 400, arkMaxOutputTokens},
		// 枚举文案的其它常见写法。
		{"unknown field", 400, "json: unknown field \"namespace\""},
		{"must be one of", 400, "Invalid value for 'tool_choice': must be one of auto, none, disabled"},
		// 「模型/参数在这个端点上没开」说的是请求，不是凭证。
		{"model disabled for this endpoint", 400, "The model gpt-x is disabled for this endpoint, expected one of chat, responses"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyHTTPFailure(tt.status, tt.message); got != sdk.OutcomeClientError {
				t.Fatalf("classifyHTTPFailure(%d, %q) = %v, want client_error", tt.status, tt.message, got)
			}
		})
	}
}

// TestClassifyHTTPFailureBodyArkParameterValidation 走完整响应体链路（结构化 code 优先）。
// 火山方舟的 code 是 InvalidParameter / type 是 BadRequest，与 OpenAI 形制不同，
// 归一化后必须落到 client_error。
func TestClassifyHTTPFailureBodyArkParameterValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "InvalidParameter code",
			body: `{"error":{"code":"InvalidParameter","message":"` + arkUnknownInputType + `","param":"input.type","type":"BadRequest"}}`,
		},
		{
			name: "no structured code, prose only",
			body: `{"error":{"message":"` + arkThinkingTypeInvalid + `"}}`,
		},
		{
			name: "built-in tool 403 prose only",
			body: `{"error":{"message":"` + arkBuiltInToolForbidden + `"}}`,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, status := range []int{400, 403} {
				if got := classifyHTTPFailureBody(status, []byte(tt.body), ""); got != sdk.OutcomeClientError {
					t.Fatalf("status %d: got %v, want client_error", status, got)
				}
			}
		})
	}
}

// TestClassifyHTTPFailureRealAccountDeathStillDies 防止修过头：真正的账号级失效仍要判死。
func TestClassifyHTTPFailureRealAccountDeathStillDies(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
	}{
		{"organization disabled", 403, "Organization disabled due to policy violation"},
		{"organization disabled 400", 400, "Organization disabled due to policy violation"},
		{"account disabled", 403, "Your account has been disabled. Contact support for details."},
		{"account deactivated", 400, "This account has been deactivated"},
		{"account suspended", 403, "Your account is suspended for violating our usage policies"},
		{"account banned", 403, "This account has been banned"},
		{"api key disabled", 403, "The API key has been disabled by the organization owner"},
		{"structured account_disabled", 400, "account_disabled"},
		{"workspace suspended", 403, "Your workspace is suspended"},
		{"billing inactive", 403, "Your account is not active, please check your billing details on our website."},
		{"billing inactive wrapped in 502", 502, "Your account is not active, please check your billing details on our website."},
		{"invalid credential 401", 401, "invalid token"},
		{"insufficient balance 403", 403, `{"code":"INSUFFICIENT_BALANCE","message":"Insufficient account balance"}`},
		// 401 永远与请求内容无关：哪怕文案里带参数校验措辞也照判死。
		{"401 wins over request-scoped wording", 401, arkThinkingTypeInvalid},
		// 403 兜底仍是账号级（例如「这个账号不允许调该模型」，换号可能就通了）。
		{"403 generic forbidden stays account level", 403, "Forbidden"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyHTTPFailure(tt.status, tt.message); got != sdk.OutcomeAccountDead {
				t.Fatalf("classifyHTTPFailure(%d, %q) = %v, want account_dead", tt.status, tt.message, got)
			}
		})
	}
}

// TestClassifyResponsesErrorParameterValidationIsClient in-band SSE 错误走同一口径：
// 参数校验不 failover、不记账号故障。
func TestClassifyResponsesErrorParameterValidationIsClient(t *testing.T) {
	tests := []struct {
		name    string
		errType string
		errCode string
		msg     string
	}{
		{"ark InvalidParameter code", "BadRequest", "InvalidParameter", arkUnknownInputType},
		{"ark unknown variant prose", "", "", arkThinkingTypeInvalid},
		{"ark built-in tool", "", "", arkBuiltInToolForbidden},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			failure := classifyResponsesError(tt.errType, tt.errCode, tt.msg)
			if failure.Kind != responsesFailureKindClient {
				t.Fatalf("Kind = %q, want client", failure.Kind)
			}
			if got := failure.outcomeKind(); got != sdk.OutcomeClientError {
				t.Fatalf("outcomeKind = %v, want client_error", got)
			}
		})
	}
}

// TestClassifyResponsesErrorCredentialFailureStillAccountDead 防止修过头。
func TestClassifyResponsesErrorCredentialFailureStillAccountDead(t *testing.T) {
	failure := classifyResponsesError("authentication_error", "invalid_access_token", "The access token is invalid")
	if failure.outcomeKind() != sdk.OutcomeAccountDead {
		t.Fatalf("outcomeKind = %v, want account_dead", failure.outcomeKind())
	}
}

// TestIsDisabledAccountTextRequiresFullSemantics 单词出现不算数：状态词必须挂在账号级主语上。
func TestIsDisabledAccountTextRequiresFullSemantics(t *testing.T) {
	positives := []string{
		"Your account has been disabled",
		"Organization disabled due to policy violation",
		"account_deactivated",
		"This API key has been suspended",
		"the api_key is disabled",
		"Your workspace was banned",
		"credentials have been deactivated",
	}
	for _, text := range positives {
		if !isDisabledAccountText(text) {
			t.Errorf("isDisabledAccountText(%q) = false, want true", text)
		}
	}

	// 否决窗口刻意窄：句尾出现 parameters 不该把一个真死号放回池子。
	if !isDisabledAccountText("Your API key has been disabled. Please check the request parameters.") {
		t.Error("a trailing request-scoped noun must not veto a real account death")
	}

	negatives := []string{
		// 事故原样本。
		arkThinkingTypeInvalid,
		"expected one of `adaptive`, `enabled`, `disabled`",
		"unknown variant `auto`, expected one of adaptive, enabled, disabled",
		// 参数名里带 key/token，不是账号。
		"The parameter `prompt_cache_key` is disabled for this endpoint",
		"field `logprobs` is disabled for this model",
		// 请求级能力，不是凭证失效。
		"This model is disabled for your account",
		"tool type `web_search` is disabled",
		"",
	}
	for _, text := range negatives {
		if isDisabledAccountText(text) {
			t.Errorf("isDisabledAccountText(%q) = true, want false", text)
		}
	}
}

// TestBareSubstringWouldStillMisfire 把陷阱本身钉死：老实现的裸子串在这条真实文案上
// 依然会命中 "disabled"——所以这里不是"上游改文案了"，而是匹配口径必须收紧。
// 任何人想把 isDisabledAccountText 改回 strings.Contains，这条用例会立刻说明后果。
func TestBareSubstringWouldStillMisfire(t *testing.T) {
	if !strings.Contains(strings.ToLower(arkThinkingTypeInvalid), "disabled") {
		t.Fatal("sample lost its `disabled` literal; the regression it guards is gone")
	}
	if isDisabledAccountText(arkThinkingTypeInvalid) {
		t.Fatal("tightened matcher must not read a legal-value enumeration as an account death")
	}
	if got := classifyHTTPFailure(400, arkThinkingTypeInvalid); got != sdk.OutcomeClientError {
		t.Fatalf("classifyHTTPFailure = %v, want client_error", got)
	}
}

// TestIsDefinitiveRateLimitTextIgnoresEchoedLiterals 同族函数同样不能被合法值枚举带偏。
func TestIsDefinitiveRateLimitTextIgnoresEchoedLiterals(t *testing.T) {
	if isDefinitiveRateLimitText("Invalid value for `mode`, expected one of `rate_limit`, `usage_limit`, `off`") {
		t.Fatal("legal-value enumeration must not read as a real rate limit")
	}
	if isDefinitiveRateLimitText("The parameter `rate_limit` specified in the request are not valid") {
		t.Fatal("echoed parameter name must not read as a real rate limit")
	}
	// 真限流不受影响。
	for _, text := range []string{
		"The usage limit has been reached. Please try again later.",
		"Upstream rate limit exceeded, please retry later",
		"rate_limit_exceeded",
		"Too many requests",
	} {
		if !isDefinitiveRateLimitText(text) {
			t.Errorf("isDefinitiveRateLimitText(%q) = false, want true", text)
		}
	}
}

// TestIsDefinitiveCredentialFailureTextIgnoresEchoedLiterals 凭证判定同样先剥回显字面量，
// 但网页端风控的 "auth token expired" 仍归可恢复冷却（见 classifyWebReverseError）。
func TestIsDefinitiveCredentialFailureTextIgnoresEchoedLiterals(t *testing.T) {
	if isDefinitiveCredentialFailureText(arkThinkingTypeInvalid) {
		t.Fatal("parameter validation must not read as a credential failure")
	}
	if isDefinitiveCredentialFailureText("Invalid value, expected one of `account_disabled`, `active`") {
		t.Fatal("legal-value enumeration must not read as a credential failure")
	}
	for _, text := range []string{
		`{"error":"invalid access token"}`,
		`{"error":"account_deactivated","message":"This account has been deactivated"}`,
		"The API key has been disabled",
	} {
		if !isDefinitiveCredentialFailureText(text) {
			t.Errorf("isDefinitiveCredentialFailureText(%q) = false, want true", text)
		}
	}
}

// TestStripEchoedLiterals 回显字面量剥离本身的边界。
func TestStripEchoedLiterals(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // 用 contains / not-contains 断言，见下
	}{
		{"backticked enum", arkThinkingTypeInvalid, "disabled"},
		{"enumeration tail without backticks", "unknown variant auto, expected one of adaptive, enabled, disabled", "disabled"},
		{"allowed values tail", "bad value; allowed values are enabled, disabled", "disabled"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripEchoedLiterals(tt.in)
			if strings.Contains(got, tt.want) {
				t.Fatalf("stripEchoedLiterals(%q) = %q, still contains %q", tt.in, got, tt.want)
			}
		})
	}

	// 未闭合的反引号保守回退：宁可不剥，也不能把正文吃掉。
	unbalanced := "Your account `acct_1 has been disabled"
	if !strings.Contains(stripEchoedLiterals(unbalanced), "disabled") {
		t.Fatal("unbalanced backtick must fall back to the original text")
	}
	if !isDisabledAccountText(unbalanced) {
		t.Fatal("unbalanced backtick must not hide a real account death")
	}
}
