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

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	textOnlyChatBody  = `{"id":"c1","model":"gemini-2.5-flash-image","choices":[{"index":0,"message":{"role":"assistant","content":"好的，这是咖啡杯产品图。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21}}`
	withImageChatBody = `{"id":"c2","model":"gemini-2.5-flash-image","choices":[{"index":0,"message":{"role":"assistant","content":"好的：![image](data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB)"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1300,"total_tokens":1309}}`
)

func geminiImageChatForward(t *testing.T, upstreamURL, body string, stream bool) (*OpenAIGateway, *sdk.ForwardRequest, *httptest.ResponseRecorder) {
	t.Helper()
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/chat/completions")
	headers.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 43, Credentials: map[string]string{
			"base_url": upstreamURL,
			"api_key":  "sk-test",
		}},
		Model:   "gemini-2.5-flash-image",
		Body:    []byte(body),
		Headers: headers,
		Stream:  stream,
	}
	if stream {
		req.Writer = recorder
	}
	return &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, req, recorder
}

// 上游第一次只回文本、第二次带图:必须重试并交付带图响应。
func TestGeminiImageChatRetriesTextOnlyResponse(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(textOnlyChatBody))
			return
		}
		_, _ = w.Write([]byte(withImageChatBody))
	}))
	defer upstream.Close()

	g, req, _ := geminiImageChatForward(t, upstream.URL,
		`{"model":"gemini-2.5-flash-image","messages":[{"role":"user","content":"画一只猫"}],"modalities":["text","image"]}`, false)
	outcome, err := g.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome = %v body=%s", outcome.Kind, outcome.Upstream.Body)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2(无图应重试)", got)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "data:image/png;base64,") {
		t.Fatalf("body = %s", outcome.Upstream.Body)
	}
	if got := usageOutputTokens(outcome.Usage); got != 1300 {
		t.Fatalf("output tokens = %v, want 1300(按最终响应计费)", got)
	}
}

func usageOutputTokens(usage *sdk.Usage) float64 {
	if usage == nil {
		return -1
	}
	for _, metric := range usage.Metrics {
		if metric.Key == usageMetricOutputTokens {
			return metric.Value
		}
	}
	return -1
}

// 三次都只回文本:交付最后一次文本响应(客户看到模型说了什么),不再无限重试。
func TestGeminiImageChatDeliversTextAfterMaxAttempts(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(textOnlyChatBody))
	}))
	defer upstream.Close()

	g, req, _ := geminiImageChatForward(t, upstream.URL,
		`{"model":"gemini-2.5-flash-image","messages":[{"role":"user","content":"画一只猫"}]}`, false)
	outcome, err := g.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome = %v", outcome.Kind)
	}
	if got := atomic.LoadInt64(&calls); got != int64(geminiImageChatMaxAttempts) {
		t.Fatalf("upstream calls = %d, want %d", got, geminiImageChatMaxAttempts)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "好的，这是咖啡杯产品图。") {
		t.Fatalf("body = %s", outcome.Upstream.Body)
	}
}

// 上游 HTTP 错误不在守卫内重试——原样归类交给 core failover。
func TestGeminiImageChatUpstreamErrorNoLocalRetry(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
	}))
	defer upstream.Close()

	g, req, _ := geminiImageChatForward(t, upstream.URL,
		`{"model":"gemini-2.5-flash-image","messages":[{"role":"user","content":"画"}]}`, false)
	outcome, err := g.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey err: %v", err)
	}
	if outcome.Kind == sdk.OutcomeSuccess {
		t.Fatalf("outcome = %v, want failure", outcome.Kind)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if outcome.Upstream.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", outcome.Upstream.StatusCode)
	}
}

// 流式客户:上游被强制非流式(可重试),交付合成 SSE。
func TestGeminiImageChatStreamSynthesizedWithRetry(t *testing.T) {
	var calls int64
	var sawStream atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "stream").Bool() {
			sawStream.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(textOnlyChatBody))
			return
		}
		_, _ = w.Write([]byte(withImageChatBody))
	}))
	defer upstream.Close()

	g, req, recorder := geminiImageChatForward(t, upstream.URL,
		`{"model":"gemini-2.5-flash-image","stream":true,"messages":[{"role":"user","content":"画一只猫"}]}`, true)
	outcome, err := g.forwardAPIKey(context.Background(), req, "")
	if err != nil {
		t.Fatalf("forwardAPIKey err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome = %v", outcome.Kind)
	}
	if sawStream.Load() {
		t.Fatal("上游收到了 stream=true,守卫应强制非流式")
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	sse := recorder.Body.String()
	if !strings.Contains(sse, `"role":"assistant"`) ||
		!strings.Contains(sse, "data:image/png;base64,") ||
		!strings.Contains(sse, `"finish_reason":"stop"`) ||
		!strings.Contains(sse, "data: [DONE]") {
		t.Fatalf("sse = %q", sse)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	if got := usageOutputTokens(outcome.Usage); got != 1300 {
		t.Fatalf("output tokens = %v, want 1300", got)
	}
}

func TestChatBodyHasImage(t *testing.T) {
	if chatBodyHasImage([]byte(textOnlyChatBody)) {
		t.Fatal("纯文本不该判有图")
	}
	if !chatBodyHasImage([]byte(withImageChatBody)) {
		t.Fatal("data URL 应判有图")
	}
	imagesField := `{"choices":[{"message":{"content":"","images":[{"image_url":{"url":"https://cdn.example.com/a.png"}}]}}]}`
	if !chatBodyHasImage([]byte(imagesField)) {
		t.Fatal("message.images 应判有图")
	}
}
