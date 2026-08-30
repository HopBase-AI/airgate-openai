package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestRestoreChatResponseModelData(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		upstream string
		public   string
		want     string
	}{
		{
			name:     "chat 顶层 model 命中还原",
			data:     `{"id":"c1","model":"tokenforge/kimi-k3","choices":[]}`,
			upstream: "tokenforge/kimi-k3",
			public:   "kimi-k3",
			want:     `{"id":"c1","model":"kimi-k3","choices":[]}`,
		},
		{
			name:     "responses 事件 response.model 命中还原",
			data:     `{"type":"response.created","response":{"id":"r1","model":"tokenforge/kimi-k3"}}`,
			upstream: "tokenforge/kimi-k3",
			public:   "kimi-k3",
			want:     `{"type":"response.created","response":{"id":"r1","model":"kimi-k3"}}`,
		},
		{
			name:     "值不等于上游 ID 时不动",
			data:     `{"model":"other-model"}`,
			upstream: "tokenforge/kimi-k3",
			public:   "kimi-k3",
			want:     `{"model":"other-model"}`,
		},
		{
			name:     "未映射(空上游)原样透传",
			data:     `{"model":"tokenforge/kimi-k3"}`,
			upstream: "",
			public:   "",
			want:     `{"model":"tokenforge/kimi-k3"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restoreChatResponseModelData(tt.data, tt.upstream, tt.public); got != tt.want {
				t.Fatalf("restoreChatResponseModelData() = %s, want %s", got, tt.want)
			}
		})
	}
}

// 非流式:上游回显的模型 ID 不得泄露给客户端,usage 也应落在公开名上。
func TestHandleNonStreamResponseRestoresMappedModel(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","model":"tokenforge/kimi-k3",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":5}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()
	outcome, err := handleNonStreamResponseWithOptions(resp, w, time.Now(), "", streamResponseOptions{
		publicModel:   "kimi-k3",
		upstreamModel: "tokenforge/kimi-k3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	got := w.Body.String()
	if strings.Contains(got, "tokenforge/") {
		t.Fatalf("upstream model id leaked to client: %s", got)
	}
	if !strings.Contains(got, `"model":"kimi-k3"`) {
		t.Fatalf("public model missing from client body: %s", got)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "kimi-k3" {
		t.Fatalf("usage model = %+v, want kimi-k3", outcome.Usage)
	}
}

// 流式:每个 SSE chunk 里的上游模型 ID 都要还原,终止事件后正常收尾。
func TestHandleStreamResponseRestoresMappedModel(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"tokenforge/kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"tokenforge/kimi-k3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	w := newSignalingResponseWriter()
	outcome, err := handleStreamResponseWithOptions(nil, resp, w, time.Now(), "", streamResponseOptions{
		publicModel:   "kimi-k3",
		upstreamModel: "tokenforge/kimi-k3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	got := w.BodyString()
	if strings.Contains(got, "tokenforge/") {
		t.Fatalf("upstream model id leaked in SSE stream: %s", got)
	}
	if !strings.Contains(got, `"model":"kimi-k3"`) {
		t.Fatalf("public model missing from SSE stream: %s", got)
	}
}
