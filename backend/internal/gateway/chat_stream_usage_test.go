package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestForwardAPIKeyDeepSeekChatStreamAlwaysRequestsUsage(t *testing.T) {
	tests := []struct {
		name              string
		requestModel      string
		body              string
		wantClientUsage   bool
		preservedJSONPath string
		preservedValue    string
	}{
		{
			name:         "absent",
			requestModel: "deepseek-v4-flash-202605",
			body:         `{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name:         "explicit false",
			requestModel: "deepseek-v4-flash-202605",
			body:         `{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":false}}`,
		},
		{
			name:            "explicit true",
			requestModel:    "deepseek-v4-flash-202605",
			body:            `{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`,
			wantClientUsage: true,
		},
		{
			name:              "preserves other stream options",
			requestModel:      "deepseek-v4-flash-202605",
			body:              `{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"vendor_trace":"keep"}}`,
			preservedJSONPath: "stream_options.vendor_trace",
			preservedValue:    "keep",
		},
		{
			name: "model from body",
			body: `{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestBody := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requestBody <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, chatStreamWithUsage)
			}))
			defer server.Close()

			writer := httptest.NewRecorder()
			gateway := &OpenAIGateway{transportPool: NewTransportPool()}
			defer gateway.transportPool.CloseIdle()
			request := &sdk.ForwardRequest{
				Account: &sdk.Account{Credentials: map[string]string{
					"api_key":  "test-key",
					"base_url": server.URL,
				}},
				Model:  tt.requestModel,
				Stream: true,
				Writer: writer,
				Headers: http.Header{
					"Content-Type":       []string{"application/json"},
					"X-Forwarded-Method": []string{http.MethodPost},
					"X-Forwarded-Path":   []string{"/v1/chat/completions"},
				},
				Body: []byte(tt.body),
			}

			outcome, err := gateway.forwardAPIKey(context.Background(), request, "")
			if err != nil {
				t.Fatalf("forwardAPIKey() error = %v", err)
			}
			if outcome.Kind != sdk.OutcomeSuccess {
				t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
			}

			upstreamBody := <-requestBody
			if got := gjson.GetBytes(upstreamBody, "model").String(); got != "deepseek-v4-flash-202605" {
				t.Fatalf("upstream model = %q, want deepseek-v4-flash-202605; body=%s", got, upstreamBody)
			}
			if !gjson.GetBytes(upstreamBody, "stream_options.include_usage").Bool() {
				t.Fatalf("upstream include_usage = false; body=%s", upstreamBody)
			}
			if tt.preservedJSONPath != "" {
				if got := gjson.GetBytes(upstreamBody, tt.preservedJSONPath).String(); got != tt.preservedValue {
					t.Fatalf("upstream %s = %q, want %q; body=%s", tt.preservedJSONPath, got, tt.preservedValue, upstreamBody)
				}
			}

			clientBody := writer.Body.String()
			wantClientBody := chatStreamWithoutUsage
			if tt.wantClientUsage {
				wantClientBody = chatStreamWithUsage
			}
			if clientBody != wantClientBody {
				t.Fatalf("client stream changed:\n got: %q\nwant: %q", clientBody, wantClientBody)
			}
			clientHasUsage := strings.Contains(clientBody, `"usage":{"prompt_tokens":88`)
			if clientHasUsage != tt.wantClientUsage {
				t.Fatalf("client usage visibility = %v, want %v; body=%s", clientHasUsage, tt.wantClientUsage, clientBody)
			}
			if !strings.Contains(clientBody, `"content":"ok"`) || !strings.Contains(clientBody, "data: [DONE]") {
				t.Fatalf("client stream lost completion data: %s", clientBody)
			}

			assertDeepSeekChatUsage(t, outcome.Usage)
		})
	}
}

func TestForwardHTTPRejectsRetiredDeepSeekAliasBeforeUpstream(t *testing.T) {
	upstreamCalled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalled <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	request := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		}},
		Model: "deepseek-v4-flash",
		Headers: http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"X-Forwarded-Path":   []string{"/v1/chat/completions"},
		},
		Body: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
	}

	outcome, err := gateway.forwardHTTP(context.Background(), request)
	if err != nil {
		t.Fatalf("forwardHTTP() error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("outcome = kind %v status %d, want client error 400", outcome.Kind, outcome.Upstream.StatusCode)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "deepseek-v4-flash-202605") {
		t.Fatalf("error body must identify the configured model: %s", outcome.Upstream.Body)
	}
	select {
	case <-upstreamCalled:
		t.Fatal("retired model request reached upstream")
	default:
	}
}

func TestForwardAPIKeyNonDeepSeekChatStreamLeavesBodyUnchanged(t *testing.T) {
	originalBody := []byte("{\n  \"model\": \"gpt-5.4\",\n  \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}],\n  \"stream\": true,\n  \"stream_options\": {\"include_usage\": false, \"vendor_trace\": \"keep\"}\n}")
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nonDeepSeekChatStream)
	}))
	defer server.Close()

	writer := httptest.NewRecorder()
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	request := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		}},
		Model:  "gpt-5.4",
		Stream: true,
		Writer: writer,
		Headers: http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"X-Forwarded-Path":   []string{"/v1/chat/completions"},
		},
		Body: append([]byte(nil), originalBody...),
	}

	outcome, err := gateway.forwardAPIKey(context.Background(), request, "")
	if err != nil {
		t.Fatalf("forwardAPIKey() error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if upstreamBody := <-requestBody; string(upstreamBody) != string(originalBody) {
		t.Fatalf("non-DeepSeek request body changed:\n got: %q\nwant: %q", upstreamBody, originalBody)
	}
	if string(request.Body) != string(originalBody) {
		t.Fatalf("ForwardRequest body changed:\n got: %q\nwant: %q", request.Body, originalBody)
	}
	if writer.Body.String() != nonDeepSeekChatStream {
		t.Fatalf("client stream changed:\n got: %q\nwant: %q", writer.Body.String(), nonDeepSeekChatStream)
	}
}

func TestForwardAPIKeyDeepSeekChatStreamKeepsFinishChunkWhenSuppressingEmbeddedUsage(t *testing.T) {
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatStreamWithEmbeddedUsage)
	}))
	defer server.Close()

	writer := httptest.NewRecorder()
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	request := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		}},
		Model:  "deepseek-v4-flash-202605",
		Stream: true,
		Writer: writer,
		Headers: http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"X-Forwarded-Path":   []string{"/v1/chat/completions"},
		},
		Body: []byte(`{"model":"deepseek-v4-flash-202605","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}

	outcome, err := gateway.forwardAPIKey(context.Background(), request, "")
	if err != nil {
		t.Fatalf("forwardAPIKey() error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if upstreamBody := <-requestBody; !gjson.GetBytes(upstreamBody, "stream_options.include_usage").Bool() {
		t.Fatalf("upstream include_usage = false; body=%s", upstreamBody)
	}

	clientBody := writer.Body.String()
	if clientBody != chatStreamWithEmbeddedUsageSuppressed {
		t.Fatalf("client stream changed:\n got: %q\nwant: %q", clientBody, chatStreamWithEmbeddedUsageSuppressed)
	}
	if strings.Contains(clientBody, `"usage"`) {
		t.Fatalf("client stream leaked injected usage: %s", clientBody)
	}
	if !strings.Contains(clientBody, `"finish_reason":"stop"`) || !strings.Contains(clientBody, "data: [DONE]") {
		t.Fatalf("client stream lost finish chunk: %s", clientBody)
	}
	assertDeepSeekChatUsageForModel(t, outcome.Usage, "deepseek-v4-flash-202605")
}

func TestForwardAPIKeyResponsesStreamDoesNotInjectChatStreamOptions(t *testing.T) {
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_test","model":"deepseek-v4-flash-202605","status":"completed","usage":{"input_tokens":5,"output_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`,
			"",
		}, "\n"))
	}))
	defer server.Close()

	writer := httptest.NewRecorder()
	gateway := &OpenAIGateway{transportPool: NewTransportPool()}
	defer gateway.transportPool.CloseIdle()
	request := &sdk.ForwardRequest{
		Account: &sdk.Account{Credentials: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		}},
		Model:  "deepseek-v4-flash-202605",
		Stream: true,
		Writer: writer,
		Headers: http.Header{
			"Content-Type":       []string{"application/json"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"X-Forwarded-Path":   []string{"/v1/responses"},
		},
		Body: []byte(`{"model":"deepseek-v4-flash-202605","input":"hi","stream":true}`),
	}

	outcome, err := gateway.forwardAPIKey(context.Background(), request, "")
	if err != nil {
		t.Fatalf("forwardAPIKey() error = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success; reason=%s", outcome.Kind, outcome.Reason)
	}
	if upstreamBody := <-requestBody; gjson.GetBytes(upstreamBody, "stream_options").Exists() {
		t.Fatalf("Responses request unexpectedly gained stream_options: %s", upstreamBody)
	}
	if !strings.Contains(writer.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("Responses completion was not forwarded: %s", writer.Body.String())
	}
	if got := usageMetricInt(outcome.Usage, usageMetricInputTokens); got != 5 {
		t.Fatalf("input tokens = %d, want 5", got)
	}
	if got := usageMetricInt(outcome.Usage, usageMetricOutputTokens); got != 2 {
		t.Fatalf("output tokens = %d, want 2", got)
	}
}

func assertDeepSeekChatUsage(t *testing.T, usage *sdk.Usage) {
	assertDeepSeekChatUsageForModel(t, usage, "deepseek-v4-flash-202605")
}

func assertDeepSeekChatUsageForModel(t *testing.T, usage *sdk.Usage, wantModel string) {
	t.Helper()
	if usage == nil {
		t.Fatal("usage = nil")
	}
	if usage.Model != wantModel {
		t.Fatalf("usage model = %q, want %q", usage.Model, wantModel)
	}
	checks := map[string]int{
		usageMetricInputTokens:           75,
		usageMetricCachedInputTokens:     13,
		usageMetricOutputTokens:          8,
		usageMetricReasoningOutputTokens: 3,
		usageMetricTotalTokens:           96,
	}
	for key, want := range checks {
		if got := usageMetricInt(usage, key); got != want {
			t.Fatalf("%s = %d, want %d", key, got, want)
		}
	}
	if usage.AccountCost <= 0 {
		t.Fatalf("account cost = %v, want positive", usage.AccountCost)
	}
}

const chatStreamPrefix = "" +
	`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

const chatUsageChunk = `data: {"id":"chatcmpl_test","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[],"usage":{"prompt_tokens":88,"completion_tokens":8,"total_tokens":96,"prompt_tokens_details":{"cached_tokens":13},"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n"

const chatStreamWithoutUsage = chatStreamPrefix + "data: [DONE]\n\n"
const chatStreamWithUsage = chatStreamPrefix + chatUsageChunk + "data: [DONE]\n\n"

const nonDeepSeekChatStream = "" +
	`data: {"id":"chatcmpl_gpt","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_gpt","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}` + "\n\n" +
	"data: [DONE]\n\n"

const chatStreamWithEmbeddedUsage = "" +
	`data: {"id":"chatcmpl_formal","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_formal","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":88,"completion_tokens":8,"total_tokens":96,"prompt_tokens_details":{"cached_tokens":13},"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n" +
	"data: [DONE]\n\n"

const chatStreamWithEmbeddedUsageSuppressed = "" +
	`data: {"id":"chatcmpl_formal","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_formal","object":"chat.completion.chunk","model":"deepseek-v4-flash-202605","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"
