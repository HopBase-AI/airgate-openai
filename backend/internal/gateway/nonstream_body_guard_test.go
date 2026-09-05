package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func nonStreamResp(path, contentType string, body string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example"+path, nil)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// 2026-09-06 生产矩阵实锤:中继以 200 回空体 / HTML / 错误信封 / 截断 JSON 时被原样透传。
func TestNonStreamHollow200IsUpstreamFailure(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		ctype    string
		body     string
		wantKind sdk.OutcomeKind
	}{
		{"empty body", "/v1/chat/completions", "application/json", "", sdk.OutcomeUpstreamTransient},
		{"whitespace body", "/v1/chat/completions", "application/json", "  \n", sdk.OutcomeUpstreamTransient},
		{"html placeholder", "/v1/chat/completions", "text/html", "<html><body>Welcome to nginx!</body></html>", sdk.OutcomeUpstreamTransient},
		{"truncated json", "/v1/chat/completions", "application/json", `{"id":"x","choices":[{"message":{"content":"tru`, sdk.OutcomeUpstreamTransient},
		{"error envelope", "/v1/chat/completions", "application/json", `{"error":{"message":"internal service failure","type":"server_error"}}`, sdk.OutcomeUpstreamTransient},
		{"error envelope on responses", "/v1/responses", "application/json", `{"error":{"message":"internal service failure","type":"server_error"}}`, sdk.OutcomeUpstreamTransient},
		{"billing wording in 200 envelope is account dead", "/v1/chat/completions", "application/json", `{"error":{"message":"Your account is not active, please check your billing details on our website.","type":"server_error"}}`, sdk.OutcomeAccountDead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			outcome, err := handleNonStreamResponse(nonStreamResp(tc.path, tc.ctype, tc.body), rr, time.Now(), "")
			if outcome.Kind != tc.wantKind {
				t.Fatalf("Kind = %v, want %v (reason=%q)", outcome.Kind, tc.wantKind, outcome.Reason)
			}
			if err == nil {
				t.Fatal("expected a Core-facing error so failover runs")
			}
			if rr.Body.Len() != 0 {
				t.Fatalf("hollow body must not be written to the client: %q", rr.Body.String())
			}
			if outcome.Usage != nil {
				t.Fatal("hollow response must not carry usage")
			}
		})
	}
}

func TestNonStreamGenuineResponsesPassThrough(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		ctype string
		body  string
	}{
		{"chat completion", "/v1/chat/completions", "application/json", `{"id":"chatcmpl-1","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`},
		{"responses api with null error", "/v1/responses", "application/json", `{"id":"resp_1","object":"response","status":"completed","error":null,"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`},
		{"embeddings", "/v1/embeddings", "application/json", `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`},
		{"audio bytes are not json-validated", "/v1/audio/speech", "audio/mpeg", "\xff\xfb\x90binary-not-json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			outcome, err := handleNonStreamResponse(nonStreamResp(tc.path, tc.ctype, tc.body), rr, time.Now(), "")
			if err != nil || outcome.Kind != sdk.OutcomeSuccess {
				t.Fatalf("Kind = %v err = %v, want success passthrough", outcome.Kind, err)
			}
			if !bytes.Equal(rr.Body.Bytes(), []byte(tc.body)) {
				t.Fatalf("body altered: %q", rr.Body.String())
			}
		})
	}
}

func TestExpectsJSONResponsePath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions":     true,
		"/v1/chat/completions?x=1": true,
		"/openai/v1/responses":     true,
		"/v1/embeddings":           true,
		"/v1/messages":             true,
		"/v1/audio/speech":         false,
		"/v1/audio/transcriptions": false,
		"/v1/files/abc/content":    false,
		"/v1/images/generations":   false,
		"/v1/models":               false,
	} {
		if got := expectsJSONResponsePath(path); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
}
