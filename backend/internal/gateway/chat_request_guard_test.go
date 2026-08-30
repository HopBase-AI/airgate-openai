package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestRejectInvalidChatMessagesTable(t *testing.T) {
	for _, tt := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantReject bool
		wantPart   string
	}{
		{"空数组拒绝", "POST", "/v1/chat/completions", `{"model":"glm-5.3","messages":[]}`, true, "不能为空"},
		{"缺字段拒绝", "POST", "/v1/chat/completions", `{"model":"glm-5.3"}`, true, "缺少 messages"},
		{"正常请求放行", "POST", "/v1/chat/completions", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, false, ""},
		{"messages 非数组拒绝", "POST", "/v1/chat/completions", `{"model":"glm-5.3","messages":"hi"}`, true, "不能为空"},
		{"非 chat 路径不管", "POST", "/v1/responses", `{"model":"gpt-5.6","input":"hi"}`, false, ""},
		{"GET 不管", "GET", "/v1/chat/completions", `{}`, false, ""},
		{"非 JSON 交上游", "POST", "/v1/chat/completions", `not-json`, false, ""},
		{"空体交上游", "POST", "/v1/chat/completions", ``, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outcome, rejected := rejectInvalidChatMessages(&sdk.ForwardRequest{Body: []byte(tt.body)},
				tt.method, tt.path, time.Now())
			if rejected != tt.wantReject {
				t.Fatalf("rejected = %v, want %v", rejected, tt.wantReject)
			}
			if rejected {
				if outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusBadRequest {
					t.Fatalf("outcome = %+v", outcome)
				}
				if !strings.Contains(string(outcome.Upstream.Body), tt.wantPart) {
					t.Fatalf("body = %s, want contains %q", outcome.Upstream.Body, tt.wantPart)
				}
			}
		})
	}
}

// 端到端:空 messages 绝不触达上游。
func TestForwardEmptyMessagesNeverHitsUpstream(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/chat/completions")
	headers.Set("Content-Type", "application/json")
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	outcome, err := g.Forward(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 64, Credentials: map[string]string{"base_url": upstream.URL, "api_key": "k"}},
		Model:   "glm-5.3",
		Body:    []byte(`{"model":"glm-5.3","messages":[]}`),
		Headers: headers,
	})
	if err != nil {
		t.Fatalf("Forward err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError || outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("outcome = %v/%d", outcome.Kind, outcome.Upstream.StatusCode)
	}
	if got := atomic.LoadInt64(&calls); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}
