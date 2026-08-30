package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// 终止事件后上游不关流(伪流式心跳中继的真实行为,2026-08-30 生产排查):
// handler 必须在转发终止事件后立即成功返回,不能继续等上游 EOF——否则会输给
// 「客户端收到终止事件即断连」的竞态,把已完整送达的请求误记 499 并丢失计费。
func TestHandleStreamResponseReturnsAtResponsesTerminalWithoutUpstreamEOF(t *testing.T) {
	reader, upstream := io.Pipe()
	defer func() { _ = upstream.Close() }()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	w := newSignalingResponseWriter()
	type result struct {
		outcome sdk.ForwardOutcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := handleStreamResponse(resp, w, time.Now(), "")
		resultCh <- result{outcome: outcome, err: err}
	}()

	stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"
	if _, err := upstream.Write([]byte(stream)); err != nil {
		t.Fatalf("write upstream stream: %v", err)
	}
	// 故意不 Close(upstream):模拟上游在终止事件后吊住连接。

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("handler 未在 response.completed 后返回,仍在等上游 EOF")
	}
	if got.err != nil {
		t.Fatalf("handleStreamResponse() error = %v", got.err)
	}
	if got.outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", got.outcome.Kind)
	}
	if got.outcome.Usage == nil {
		t.Fatal("outcome usage = nil, want parsed usage from response.completed")
	}
	body := w.BodyString()
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("terminal event missing from client body: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("final SSE event not terminated with blank line: %q", body)
	}
}

func TestHandleStreamResponseReturnsAtChatDoneWithoutUpstreamEOF(t *testing.T) {
	reader, upstream := io.Pipe()
	defer func() { _ = upstream.Close() }()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	w := newSignalingResponseWriter()
	type result struct {
		outcome sdk.ForwardOutcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := handleStreamResponse(resp, w, time.Now(), "")
		resultCh <- result{outcome: outcome, err: err}
	}()

	stream := "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
		"data: [DONE]\n\n"
	if _, err := upstream.Write([]byte(stream)); err != nil {
		t.Fatalf("write upstream stream: %v", err)
	}
	// 故意不 Close(upstream):模拟上游在 [DONE] 后吊住连接。

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("handler 未在 [DONE] 后返回,仍在等上游 EOF")
	}
	if got.err != nil {
		t.Fatalf("handleStreamResponse() error = %v", got.err)
	}
	if got.outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", got.outcome.Kind)
	}
	body := w.BodyString()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("[DONE] missing from client body: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("final SSE event not terminated with blank line: %q", body)
	}
}
