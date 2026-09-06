package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-09-06 客户实测:流式大图入参(2048x2048 PNG ≈ 9000 input tokens)上游预填
// 70~160 秒,而等上游响应头这段空窗我方零字节输出(8 笔 499 全部卡在 57~60 秒、
// bytes_out=-1),客户端 60 秒读超时先行放弃 → 表现为「空响应 / 连接被关闭」。
// 心跳间隔必须夹在「生产首字 p95」与「常见客户端读超时」之间:
// 大于 p95 → 绝大多数请求根本不触发心跳,行为与加之前完全一致;
// 小于 30 秒 → 能托住长预填请求不被读超时掐断。
func TestStreamFirstByteKeepAliveIntervalSitsBetweenP95AndClientTimeout(t *testing.T) {
	const productionFirstByteP95 = 21 * time.Second
	const commonClientReadTimeout = 30 * time.Second

	if streamFirstByteKeepAliveInterval <= productionFirstByteP95 {
		t.Fatalf("interval=%v 必须大于首字 p95 %v,否则正常请求也会被提前提交为 SSE",
			streamFirstByteKeepAliveInterval, productionFirstByteP95)
	}
	if streamFirstByteKeepAliveInterval >= commonClientReadTimeout {
		t.Fatalf("interval=%v 必须小于常见客户端读超时 %v,否则托不住长预填请求",
			streamFirstByteKeepAliveInterval, commonClientReadTimeout)
	}
}

// 空窗心跳只能是协议中立的 SSE 注释:core 的 streamApplicationResponseCommitted
// 不把注释当业务数据,所以心跳发出后换号兜底仍然可用。这条断言防的是有人把心跳
// 改成 data: 帧——那会让每个长预填请求都失去 failover 能力。
func TestStreamFirstByteKeepAliveWritesOnlyNeutralComments(t *testing.T) {
	w := newSignalingResponseWriter()
	ka := startSSEPingKeepAliveWithInterval(w, 5*time.Millisecond)
	waitForHeartbeat(t, w)
	ka.Stop()

	body := w.BodyString()
	if body == "" {
		t.Fatal("心跳未写出任何字节")
	}
	if strings.ReplaceAll(body, responseStreamKeepAliveComment, "") != "" {
		t.Fatalf("心跳夹带了非注释内容: %q", body)
	}
	if strings.Contains(body, "data:") {
		t.Fatalf("心跳不得使用 data: 帧(会被判成业务输出、丢失 failover): %q", body)
	}
}

// 流式请求一进来就把 SSE 响应头准备好,但首拍之前不写字节:上游快时行为不变。
func TestStreamFirstByteKeepAliveSetsSSEHeadersWithoutCommitting(t *testing.T) {
	w := httptest.NewRecorder()
	ka := startSSEPingKeepAliveWithInterval(w, streamFirstByteKeepAliveInterval)
	ka.Stop()

	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if ka.Wrote() || w.Body.Len() != 0 {
		t.Fatalf("首拍前不应写出任何字节, body=%q", w.Body.String())
	}
}
