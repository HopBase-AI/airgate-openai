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

func TestIsUpstreamKeepAliveSSEData(t *testing.T) {
	keepAlives := []string{
		`{"type":"response.output_text.delta","item_id":"SSE-Keep-Alive","output_index":0,"delta":"\u200b","SSE-Keep-Alive":true}`,
		`{"type":"response.output_text.delta","item_id":"SSE-Keep-Alive","delta":""}`,
		`{"SSE-Keep-Alive":true}`,
	}
	for _, data := range keepAlives {
		if !isUpstreamKeepAliveSSEData(data) {
			t.Errorf("应识别为上游保活帧: %s", data)
		}
		if streamDataHasOutput(data) {
			t.Errorf("保活帧不得算作真实输出: %s", data)
		}
	}

	real := []string{
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`,
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`[DONE]`,
	}
	for _, data := range real {
		if isUpstreamKeepAliveSSEData(data) {
			t.Errorf("正常数据被误判为保活帧: %s", data)
		}
	}
	if !streamDataHasOutput(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`) {
		t.Error("真实增量应算输出")
	}
}

// 零宽字符不构成可见内容:上游用 U+200B 假装吐字时不得触发「已出内容」。
func TestHasVisibleDeltaIgnoresZeroWidth(t *testing.T) {
	for _, blank := range []string{"", " ", "\t\n", "\u200b", "\u200b\u200c\u200d", "\ufeff", "\u00a0"} {
		if hasVisibleDelta(blank) {
			t.Errorf("不可见内容被判为可见: %q", blank)
		}
		data := `{"type":"response.output_text.delta","delta":` + quoteJSON(blank) + `}`
		if streamDataHasOutput(data) {
			t.Errorf("不可见增量不得算输出: %s", data)
		}
	}
	for _, visible := range []string{"a", " x ", "\u200b你好\u200b"} {
		if !hasVisibleDelta(visible) {
			t.Errorf("可见内容被判为不可见: %q", visible)
		}
	}
}

func quoteJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 上游只发保活帧、迟迟不出内容:看门狗到点应判可重试(core 会换账号),
// 且客户端不得收到任何上游内容(只应有我们自己的 SSE 注释心跳)。
func TestFirstOutputTimeoutYieldsRetryableOutcome(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// 只吐保活帧,永不产出真实内容。
		for i := 0; i < 3; i++ {
			_, _ = pw.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"SSE-Keep-Alive\",\"delta\":\"\\u200b\",\"SSE-Keep-Alive\":true}\n\n"))
			time.Sleep(30 * time.Millisecond)
		}
		<-time.After(2 * time.Second)
		_ = pw.Close()
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	rec := httptest.NewRecorder()
	outcome, err := handleStreamResponseWithKeepAliveOptions(
		nil, resp, rec, time.Now(), "",
		20*time.Millisecond, // 我们自己的心跳间隔
		streamResponseOptions{firstOutputTimeout: 200 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient(可 failover)", outcome.Kind)
	}
	body := rec.Body.String()
	if strings.Contains(body, "SSE-Keep-Alive") || strings.Contains(body, "\u200b") {
		t.Errorf("上游保活帧不得转发给客户端, got: %q", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, ":") {
			t.Errorf("超时前只应写出 SSE 注释心跳, got line: %q", line)
		}
	}
}

// 已经出过真实内容再断流:不得被看门狗改判(重试会让客户看到重复内容)。
func TestFirstOutputTimeoutNotAppliedAfterRealOutput(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	outcome, err := handleStreamResponseWithKeepAliveOptions(
		nil, sseResponse(body), rec, time.Now(), "",
		time.Second,
		streamResponseOptions{firstOutputTimeout: 50 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.Kind == sdk.OutcomeUpstreamTransient {
		t.Error("已出内容的流不应被看门狗判为可重试")
	}
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Errorf("真实内容应照常下发, got: %q", rec.Body.String())
	}
}

// 保活帧混在真实内容前:保活帧被吞、真实内容照常,TTFT 记的是真实内容时刻。
func TestKeepAliveFramesDroppedBeforeRealContent(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"SSE-Keep-Alive","delta":"\u200b","SSE-Keep-Alive":true}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Hi"}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	outcome, err := handleStreamResponseWithKeepAliveOptions(
		nil, sseResponse(body), rec, time.Now(), "", time.Second, streamResponseOptions{},
	)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, "SSE-Keep-Alive") {
		t.Errorf("保活帧不得转发, got: %q", out)
	}
	if !strings.Contains(out, "Hi") {
		t.Errorf("真实内容缺失, got: %q", out)
	}
	if outcome.Usage != nil && outcome.Usage.FirstTokenMs < 0 {
		t.Errorf("FirstTokenMs 异常: %d", outcome.Usage.FirstTokenMs)
	}
}
