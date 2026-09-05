package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestClassifyResponsesFailureContextWindow(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.AnthropicErrorType != "invalid_request_error" {
		t.Fatalf("unexpected anthropic error type %q", failure.AnthropicErrorType)
	}
}

func TestClassifyResponsesFailureSafetyRejected(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID 916c6516-5f37-9121-b05a-a604888c0055."}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", failure.StatusCode)
	}
	if failure.Code != "safety_rejected" {
		t.Fatalf("unexpected code %q", failure.Code)
	}
}

// TestIsSafetyRejectionTextNearMisses 守住关键词收紧后的边界：
// 提示性文案、把 policy 当名词解释的 400 不应被误判成"安全拒绝"。
func TestIsSafetyRejectionTextNearMisses(t *testing.T) {
	negatives := []string{
		"please ensure your prompt follows our safety policy guidelines",
		"see the safety policy for details",
		"input violates the company policy",
		"this field requires the safety token",
		"",
	}
	for _, msg := range negatives {
		if isSafetyRejectionText(msg) {
			t.Fatalf("did not expect safety match for %q", msg)
		}
	}

	positives := []string{
		"Your request was rejected by the safety system",
		"content_policy_violation",
		"blocked by policy",
		"prompt was blocked by our safety filter",
		"moderation_blocked",
	}
	for _, msg := range positives {
		if !isSafetyRejectionText(msg) {
			t.Fatalf("expected safety match for %q", msg)
		}
	}
}

func TestClassifyResponsesFailureContinuationAnchor(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindContinuationAnchor {
		t.Fatalf("unexpected kind %q", failure.Kind)
	}
	if !failure.isContinuationAnchorError() {
		t.Fatalf("expected continuation anchor error")
	}
}

func TestClassifyHTTPFailureTreatsUsageLimit403AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(403, "The usage limit has been reached. Please try again later.")
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsUsageLimit400AsRateLimited(t *testing.T) {
	got := classifyHTTPFailure(400, "The usage limit has been reached. Please try again later.")
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureKeepsDisabled403AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(403, "Organization disabled due to policy violation")
	if got != sdk.OutcomeAccountDead {
		t.Fatalf("expected AccountDead, got %v", got)
	}
}

func TestClassifyHTTPFailureTreatsDisabled400AsAccountDead(t *testing.T) {
	got := classifyHTTPFailure(400, "Organization disabled due to policy violation")
	if got != sdk.OutcomeAccountDead {
		t.Fatalf("expected AccountDead, got %v", got)
	}
}

// TestClassifyHTTPFailureBodyBillingNotActiveCode 结构化 billing_not_active 在非 429
// 任意错误状态码下判 Dead；429 保留限流语义。
func TestClassifyHTTPFailureBodyBillingNotActiveCode(t *testing.T) {
	body := []byte(`{"error":{"code":"billing_not_active","message":"Billing is not active on this account"}}`)
	for _, status := range []int{402, 403, 502} {
		if got := classifyHTTPFailureBody(status, body, ""); got != sdk.OutcomeAccountDead {
			t.Fatalf("status %d: expected AccountDead, got %v", status, got)
		}
	}
	if got := classifyHTTPFailureBody(429, body, ""); got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("429: expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyHTTPFailureProductionErrorMatrix(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		want    sdk.OutcomeKind
	}{
		{"429 pending concurrency", 429, "Too many pending requests, please retry later", sdk.OutcomeAccountRateLimited},
		{"429 upstream rate limit", 429, "Upstream rate limit exceeded, please retry later", sdk.OutcomeAccountRateLimited},
		{"429 cloudflare 1015", 429, "error code: 1015", sdk.OutcomeAccountRateLimited},
		{"403 insufficient balance", 403, `{"code":"INSUFFICIENT_BALANCE","message":"Insufficient account balance"}`, sdk.OutcomeAccountDead},
		{"401 invalid credential", 401, "invalid token", sdk.OutcomeAccountDead},
		{"502 relay failure", 502, "error code: 502", sdk.OutcomeUpstreamTransient},
		{"503 temporary outage", 503, "Service temporarily unavailable", sdk.OutcomeUpstreamTransient},
		{"503 explicit overload", 503, "server_is_overloaded", sdk.OutcomeUpstreamTransient},
		{"502 billing inactive", 502, "Your account is not active, please check your billing details on our website.", sdk.OutcomeAccountDead},
		{"503 billing inactive", 503, "Your account is not active, please check your billing details on our website.", sdk.OutcomeAccountDead},
		{"403 billing inactive", 403, "Your account is not active, please check your billing details on our website.", sdk.OutcomeAccountDead},
		{"429 billing wording keeps rate limit", 429, "Your account is not active, please check your billing details on our website.", sdk.OutcomeAccountRateLimited},
		{"529 provider overload", 529, "overloaded", sdk.OutcomeUpstreamTransient},
		{"504 upstream timeout is failoverable", 504, "server_is_overloaded", sdk.OutcomeUpstreamTransient},
		{"400 invalid request", 400, "invalid request payload", sdk.OutcomeClientError},
		{"404 model unavailable", 404, "model_not_found", sdk.OutcomeClientError},
		{"419 nonstandard client response", 419, "session expired", sdk.OutcomeClientError},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyHTTPFailure(tt.status, tt.message); got != tt.want {
				t.Fatalf("classifyHTTPFailure(%d, %q) = %v, want %v", tt.status, tt.message, got, tt.want)
			}
		})
	}
}

func TestFailureOutcomeStructured429Semantics(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		message        string
		wantKind       sdk.OutcomeKind
		wantRetryAfter time.Duration
	}{
		{
			name:           "overload code is account neutral",
			body:           `{"error":{"type":"server_error","code":"server_is_overloaded","message":"Please try again later."}}`,
			message:        "Please try again later.",
			wantKind:       sdk.OutcomeUpstreamTransient,
			wantRetryAfter: 5 * time.Second,
		},
		{
			name:           "credential rate limit remains account scoped",
			body:           `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit exceeded."}}`,
			message:        "Rate limit exceeded.",
			wantKind:       sdk.OutcomeAccountRateLimited,
			wantRetryAfter: time.Minute,
		},
		{
			name:           "rate limit code wins over overload prose",
			body:           `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is overloaded; retry later."}}`,
			message:        "The model is overloaded; retry later.",
			wantKind:       sdk.OutcomeAccountRateLimited,
			wantRetryAfter: time.Minute,
		},
		{
			name:           "overload code wins over generic rate type",
			body:           `{"error":{"type":"rate_limit_error","code":"server_is_overloaded","message":"Please try again later."}}`,
			message:        "Please try again later.",
			wantKind:       sdk.OutcomeUpstreamTransient,
			wantRetryAfter: 5 * time.Second,
		},
		{
			name:           "rate limit code wins over invalid request type",
			body:           `{"error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"Unknown parameter: 'rate limit'."}}`,
			message:        "Unknown parameter: 'rate limit'.",
			wantKind:       sdk.OutcomeAccountRateLimited,
			wantRetryAfter: time.Minute,
		},
		{
			name:           "top level overload type wins over rate prose",
			body:           `{"type":"server_is_overloaded","message":"Rate limit exceeded."}`,
			message:        "Rate limit exceeded.",
			wantKind:       sdk.OutcomeUpstreamTransient,
			wantRetryAfter: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			outcome := failureOutcome(http.StatusTooManyRequests, body, nil, tt.message, 0)
			if outcome.Kind != tt.wantKind {
				t.Fatalf("Kind = %v, want %v", outcome.Kind, tt.wantKind)
			}
			if outcome.RetryAfter != tt.wantRetryAfter {
				t.Fatalf("RetryAfter = %v, want %v", outcome.RetryAfter, tt.wantRetryAfter)
			}
			if outcome.Upstream.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("transport status = %d, want 429", outcome.Upstream.StatusCode)
			}
			if string(outcome.Upstream.Body) != tt.body {
				t.Fatalf("upstream body changed: %s", outcome.Upstream.Body)
			}
		})
	}
}

func TestFailureOutcomeStructuredSignalBoundaries(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		message        string
		wantKind       sdk.OutcomeKind
		wantRetryAfter time.Duration
	}{
		{
			name:           "403 rate limit keeps account cooldown despite overload prose",
			status:         http.StatusForbidden,
			body:           `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is overloaded; retry later."}}`,
			message:        "The model is overloaded; retry later.",
			wantKind:       sdk.OutcomeAccountRateLimited,
			wantRetryAfter: 10 * time.Minute,
		},
		{
			name:     "400 invalid request mentioning overload stays client error",
			status:   http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"invalid_request","message":"The overloaded field is invalid."}}`,
			message:  "The overloaded field is invalid.",
			wantKind: sdk.OutcomeClientError,
		},
		{
			name:     "400 unknown rate limit parameter stays client error",
			status:   http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"unknown_parameter","message":"Unknown parameter: 'rate_limit'."}}`,
			message:  "Unknown parameter: 'rate_limit'.",
			wantKind: sdk.OutcomeClientError,
		},
		{
			name:     "400 unknown natural language rate limit parameter stays client error",
			status:   http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"unknown_parameter","message":"Unknown parameter: 'rate limit'."}}`,
			message:  "Unknown parameter: 'rate limit'.",
			wantKind: sdk.OutcomeClientError,
		},
		{
			name:     "400 unknown usage limit parameter stays client error",
			status:   http.StatusBadRequest,
			body:     `{"error":{"type":"invalid_request_error","code":"unknown_parameter","message":"Unknown parameter: 'usage_limit'."}}`,
			message:  "Unknown parameter: 'usage_limit'.",
			wantKind: sdk.OutcomeClientError,
		},
		{
			name:     "404 overload code does not override client boundary",
			status:   http.StatusNotFound,
			body:     `{"error":{"type":"server_error","code":"server_is_overloaded","message":"Please try again later."}}`,
			message:  "Please try again later.",
			wantKind: sdk.OutcomeClientError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := failureOutcome(tt.status, []byte(tt.body), nil, tt.message, 0)
			if outcome.Kind != tt.wantKind {
				t.Fatalf("Kind = %v, want %v", outcome.Kind, tt.wantKind)
			}
			if outcome.RetryAfter != tt.wantRetryAfter {
				t.Fatalf("RetryAfter = %v, want %v", outcome.RetryAfter, tt.wantRetryAfter)
			}
		})
	}
}

func TestFailureOutcomeStructuredOverloadPreservesRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"type":"server_error","code":"server_is_overloaded","message":"Please try again later."}}`)
	outcome := failureOutcome(http.StatusTooManyRequests, body, nil, "Please try again later.", 17*time.Second)
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", outcome.Kind)
	}
	if outcome.RetryAfter != 17*time.Second {
		t.Fatalf("RetryAfter = %v, want 17s", outcome.RetryAfter)
	}
}

func TestForwardAPIKeyHTTP429StructuredOverloadIsTransient(t *testing.T) {
	upstreamBody := `{"error":{"type":"server_error","code":"server_is_overloaded","message":"Please try again later."}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer server.Close()

	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	outcome, err := gateway.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 56, Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		}},
		Model: "gpt-5.6-sol",
		Headers: http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"X-Forwarded-Path":   []string{"/v1/responses"},
		},
		Body: []byte(`{"model":"gpt-5.6-sol","input":"ping"}`),
	}, "")
	if err != nil {
		t.Fatalf("forwardAPIKey() error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", outcome.Kind)
	}
	if outcome.Kind.IsAccountFault() {
		t.Fatalf("overload must not penalize the selected account: %v", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("transport status = %d, want 429", outcome.Upstream.StatusCode)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
}

func TestClassifyResponsesFailureProductionSSEMatrix(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		want           sdk.OutcomeKind
		wantRetryAfter time.Duration
	}{
		{
			name:           "server overloaded is account neutral transient",
			raw:            `{"type":"response.failed","response":{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
			want:           sdk.OutcomeUpstreamTransient,
			wantRetryAfter: 5 * time.Second,
		},
		{
			name: "rate limit code wins over overload prose",
			raw:  `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is overloaded; retry later."}}}`,
			want: sdk.OutcomeAccountRateLimited,
		},
		{
			name:           "overload code wins over rate limit type and prose",
			raw:            `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":"server_is_overloaded","message":"Rate limit exceeded."}}}`,
			want:           sdk.OutcomeUpstreamTransient,
			wantRetryAfter: 5 * time.Second,
		},
		{
			name: "rate limit code wins over invalid request type",
			raw:  `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"The overloaded field is invalid."}}}`,
			want: sdk.OutcomeAccountRateLimited,
		},
		{
			name: "rate limit type wins over overload prose",
			raw:  `{"type":"response.failed","response":{"error":{"type":"rate_limit_error","message":"The model is overloaded; retry later."}}}`,
			want: sdk.OutcomeAccountRateLimited,
		},
		{
			name: "generic upstream error is transient",
			raw:  `{"type":"response.failed","response":{"error":{"type":"server_error","code":"upstream_error","message":"Upstream request failed"}}}`,
			want: sdk.OutcomeUpstreamTransient,
		},
		{
			name: "context window remains client error",
			raw:  `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window"}}}`,
			want: sdk.OutcomeClientError,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			failure := classifyResponsesFailure([]byte(tt.raw))
			if failure == nil {
				t.Fatal("expected classified failure")
			}
			if got := failure.outcomeKind(); got != tt.want {
				t.Fatalf("outcome = %v, want %v; failure=%+v", got, tt.want, failure)
			}
			if failure.RetryAfter != tt.wantRetryAfter {
				t.Fatalf("RetryAfter = %v, want %v", failure.RetryAfter, tt.wantRetryAfter)
			}
		})
	}
}

func TestClassifyWSErrorEventMachineSignalPriority(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want responsesFailureKind
	}{
		{
			name: "rate limit code wins over overload prose",
			raw:  `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is overloaded; retry later."}}`,
			want: responsesFailureKindRateLimited,
		},
		{
			name: "overload code wins over rate limit type and prose",
			raw:  `{"type":"error","error":{"type":"rate_limit_error","code":"server_is_overloaded","message":"Rate limit exceeded."}}`,
			want: responsesFailureKindServer,
		},
		{
			name: "rate limit code wins over invalid request type",
			raw:  `{"type":"error","error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"The overloaded field is invalid."}}`,
			want: responsesFailureKindRateLimited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := classifyWSErrorEvent([]byte(tt.raw))
			if failure == nil {
				t.Fatal("expected classified failure")
			}
			if failure.Kind != tt.want {
				t.Fatalf("Kind = %q, want %q; failure=%+v", failure.Kind, tt.want, failure)
			}
		})
	}
}

func TestClassifyAnthropicBodyTreatsUsageLimit403AsRateLimited(t *testing.T) {
	body := []byte(`{"error":{"message":"The usage limit has been reached. Try again later."}}`)
	got := classifyAnthropicBody(403, body)
	if got != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected AccountRateLimited, got %v", got)
	}
}

func TestClassifyWSErrorEventUsageLimitReached(t *testing.T) {
	// ChatGPT OAuth 触发 usage limit 时走 WS error 事件，带 resets_in_seconds。
	raw := []byte(`{"type":"error","error":{"type":"usage_limit_reached","code":"rate_limit_exceeded","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited kind, got %q", failure.Kind)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeAccountRateLimited {
		t.Fatalf("expected OutcomeAccountRateLimited, got %v", kind)
	}
	if failure.RetryAfter < 59*time.Minute || failure.RetryAfter > 61*time.Minute {
		t.Fatalf("expected RetryAfter~=1h from resets_in_seconds, got %s", failure.RetryAfter)
	}
}

func TestClassifyWSErrorEventDefinitiveAccountFailure(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "authentication error",
			raw:  `{"type":"error","error":{"type":"authentication_error","code":"invalid_access_token","message":"The access token is invalid"}}`,
		},
		{
			name: "account deactivated",
			raw:  `{"type":"error","error":{"type":"invalid_request_error","code":"account_deactivated","message":"This account has been deactivated"}}`,
		},
		{
			name: "organization disabled",
			raw:  `{"type":"error","error":{"type":"permission_error","code":"forbidden","message":"Organization disabled due to policy violation"}}`,
		},
		{
			name: "unauthorized code",
			raw:  `{"type":"error","error":{"type":"permission_error","code":"unauthorized","message":"Access denied"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := classifyWSErrorEvent([]byte(tt.raw))
			if failure == nil {
				t.Fatal("expected classified failure")
			}
			if failure.Kind != responsesFailureKindAccountDead {
				t.Fatalf("Kind = %q, want account_dead", failure.Kind)
			}
			if failure.StatusCode != http.StatusUnauthorized {
				t.Fatalf("StatusCode = %d, want 401", failure.StatusCode)
			}
			if got := failure.outcomeKind(); got != sdk.OutcomeAccountDead {
				t.Fatalf("Outcome = %v, want AccountDead", got)
			}
		})
	}
}

func TestClassifyWSErrorEventDoesNotTreatFeatureDisabledAsDeadAccount(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"permission_error","code":"feature_unavailable","message":"Image generation is disabled for this model"}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatal("expected classified failure")
	}
	if failure.Kind == responsesFailureKindAccountDead {
		t.Fatalf("feature-level disabled error killed account: %+v", failure)
	}
}

func TestClassifyWSErrorEventOpenAICompatSSEError(t *testing.T) {
	raw := []byte(`{"error":{"message":"An error occurred while processing your request. Please include the request ID 349f8894 in your message.","type":"server_error","code":"upstream_error"}}`)
	failure := classifyWSErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindServer {
		t.Fatalf("expected server kind, got %q", failure.Kind)
	}
	if failure.Message != "An error occurred while processing your request. Please include the request ID 349f8894 in your message." {
		t.Fatalf("unexpected message %q", failure.Message)
	}
}

func TestClassifyGenericSSEErrorEventTopLevelModelNotFound(t *testing.T) {
	raw := []byte(`{"message":"The model gpt-5.3-codex-spark does not exist.","type":"invalid_request_error","code":"model_not_found"}`)
	failure := classifyGenericSSEErrorEvent(raw)
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("expected client kind, got %q", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400, got %d", failure.StatusCode)
	}
	if kind := failure.outcomeKind(); kind != sdk.OutcomeClientError {
		t.Fatalf("expected OutcomeClientError, got %v", kind)
	}
}

func TestHandleStreamResponseSanitizesFirstSSEError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"upstream secret request ID 349f8894\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n")),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	body := w.Body.String()
	if strings.Contains(body, "upstream secret") || strings.Contains(body, "349f8894") {
		t.Fatalf("response leaked upstream error: %q", body)
	}
}

func TestHandleStreamResponseTreatsCompletedEmptyStreamAsFailure(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err == nil {
		t.Fatalf("expected empty stream error")
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected OutcomeUpstreamTransient, got %v", outcome.Kind)
	}
	if !strings.Contains(outcome.Reason, "上游流式响应为空") {
		t.Fatalf("unexpected reason %q", outcome.Reason)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("empty stream should not be forwarded before validation, got %q", w.Body.String())
	}
}

func TestHandleStreamResponseFlushesBufferedPreludeWhenOutputArrives(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, `"content":"ok"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("buffered stream was not forwarded completely: %q", got)
	}
}

func TestHandleStreamResponseTreatsReasoningContentAsOutput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{"content":"","reasoning_content":"The answer is 42."},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	// [DONE] 后由我们补空行终结最后一个事件帧(终止事件即收尾,不再等上游 EOF),
	// 上游这份流恰好缺终帧空行,因此期望值比上游原文多一个 "\n"。
	want := body + "\n"
	if got != want {
		t.Fatalf("reasoning-only stream should be forwarded unchanged:\n got: %q\nwant: %q", got, want)
	}
}

func TestStreamChatChoicesHaveOutputRecognizesReasoningContent(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "delta",
			data: `{"choices":[{"delta":{"content":"","reasoning_content":"answer"}}]}`,
			want: true,
		},
		{
			name: "message",
			data: `{"choices":[{"message":{"content":"","reasoning_content":"answer"}}]}`,
			want: true,
		},
		{
			name: "empty reasoning",
			data: `{"choices":[{"delta":{"content":"","reasoning_content":"  "}}]}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamChatChoicesHaveOutput(tt.data); got != tt.want {
				t.Fatalf("streamChatChoicesHaveOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleStreamResponseTreatsResponsesImageContentAsOutput(t *testing.T) {
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","output":[]}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","content":[]}}`,
		"",
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":""}`,
		"",
		`event: response.content_part.done`,
		`data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","status":"completed","usage":{"input_tokens":10,"output_tokens":2},"output":[{"id":"rs_1","type":"reasoning","encrypted_content":"secret"},{"id":"msg_1","type":"message","content":[{"type":"output_image","image_url":"data:image/png;base64,AAA"}]}]}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"output_image"`) || !strings.Contains(got, "data:image/png;base64,AAA") {
		t.Fatalf("image content stream was not forwarded completely: %q", got)
	}
}

func TestHandleStreamResponseNormalizesImageGenerationCallStatus(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"generating","result":"aGVsbG8=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","status":"completed","usage":{"input_tokens":10,"output_tokens":2},"output":[{"id":"ig_1","type":"image_generation_call","status":"generating","result":"aGVsbG8=","size":"1024x1024"}]}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("expected OutcomeSuccess, got %v", outcome.Kind)
	}
	got := w.Body.String()
	if strings.Contains(got, `"status":"generating"`) {
		t.Fatalf("image_generation_call status should be normalized: %q", got)
	}
	if !strings.Contains(got, `"item":{"id":"ig_1","type":"image_generation_call","status":"completed"`) {
		t.Fatalf("output_item.done should be normalized: %q", got)
	}
	if !strings.Contains(got, `"output":[{"id":"ig_1","type":"image_generation_call","status":"completed"`) {
		t.Fatalf("response.completed output should be normalized: %q", got)
	}
}

func TestHandleStreamResponseTreatsPartialImageB64AsOutput(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.image_generation_call.partial_image","item_id":"ig_1","output_index":0,"partial_image_b64":"aGVsbG8=","partial_image_index":0}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("expected OutcomeStreamAborted, got %v", outcome.Kind)
	}
	if err == nil {
		t.Fatalf("expected missing completion event error")
	}
	if !strings.Contains(w.Body.String(), `"partial_image_b64":"aGVsbG8="`) {
		t.Fatalf("partial image event should be forwarded: %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "\"code\":\"upstream_error\"") {
		t.Fatalf("partial stream should end with a sanitized SSE error: %q", w.Body.String())
	}
}

func TestHandleStreamResponseRecordsDeliveredImagesWhenStreamAborts(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aGVsbG8=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.output_item.done","item":{"id":"ig_2","type":"image_generation_call","status":"completed","result":"d29ybGQ=","size":"1024x1024"}}`,
		"",
		`data: {"type":"response.output_item.done","item":{"id":"ig_3","type":"image_generation_call","status":"completed","result":"YWlyZ2F0ZQ==","size":"1024x1024"}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("expected OutcomeStreamAborted, got %v", outcome.Kind)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage = nil, want delivered image usage")
	}
	if got := usageMetricInt(outcome.Usage, usageMetricImages); got != 3 {
		t.Fatalf("images metric = %d, want 3", got)
	}
	if got := usageCostMetadata(outcome.Usage, usageCostImageTool, "image_count"); got != "3" {
		t.Fatalf("image_count metadata = %q, want 3", got)
	}
	if got := usageCostMetadata(outcome.Usage, usageCostImageTool, "size"); got != "1024x1024" {
		t.Fatalf("size metadata = %q, want 1024x1024", got)
	}
}

func TestClassifyResponsesFailureResetsAtAbsolute(t *testing.T) {
	// resets_at 是 Unix 时间戳（绝对时间），RetryAfter 应该反推出大致等于
	// future - now；这里留充分的断言窗口避免时钟抖动。
	future := time.Now().Add(2 * time.Hour).Unix()
	raw := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + formatInt(future) + `}}}`)
	failure := classifyResponsesFailure(raw)
	if failure == nil || failure.Kind != responsesFailureKindRateLimited {
		t.Fatalf("expected rate_limited failure, got %+v", failure)
	}
	if failure.RetryAfter < time.Hour+30*time.Minute || failure.RetryAfter > 2*time.Hour+5*time.Minute {
		t.Fatalf("expected RetryAfter~=2h, got %s", failure.RetryAfter)
	}
}

func formatInt(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// TestClassifyHTTPFailureConversationState502AsClientError 中转把 tool call 配对
// 丢失包成 502：换账号重放必然复现，必须按 ClientError 透传（2026-08-12 生产样本）。
func TestClassifyHTTPFailureConversationState502AsClientError(t *testing.T) {
	msg := "No tool call found for function call output with call_id call_eTVUs8Fj"
	for _, status := range []int{502, 503} {
		if got := classifyHTTPFailure(status, msg); got != sdk.OutcomeClientError {
			t.Fatalf("status %d: expected ClientError, got %v", status, got)
		}
	}
	// 4xx 原本就是 ClientError，确保不回归
	if got := classifyHTTPFailure(400, msg); got != sdk.OutcomeClientError {
		t.Fatalf("400: expected ClientError, got %v", got)
	}
}

// TestClassifyResponsesFailureConversationState in-band SSE 错误同样按 client 归类。
func TestClassifyResponsesFailureConversationState(t *testing.T) {
	failure := classifyResponsesError("server_error", "", "No tool output found for function call call_abc123")
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("expected client kind, got %v", failure.Kind)
	}
}
