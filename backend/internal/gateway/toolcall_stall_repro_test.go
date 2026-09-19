package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// toolcall_stall_repro_test.go —— 复现 2026-09-18 生产 usage_logs #450835
// （gpt-5.5 / Codex Desktop / 78.3s / 200 + stream_aborted）的两种候选成因，
// 用同一套断言把它们区分开。
//
// 待判的两个假设：
//
//	H1「客户端本地执行工具期间上游没数据，于是被我们掐断」
//	H2「上游宣布了工具调用却没吐出参数就断气，被读空闲守卫掐断」
//
// 两者的差别不在耗时，而在**流被掐断时停在哪个事件上**，所以两个场景都用同一个
// 60s 语义的守卫（测试里压成毫秒级）跑，看谁产出生产那条 WARN 的签名：
// completion_event="" / finish_reason=in_progress / last=response.output_item.added。

// scriptedSSEBody 先按脚本吐 SSE，吐完进入静默：连接不关、一个字节也不再来，
// 直到守卫取消 ctx 才让阻塞中的 Read 返回。这正是生产里那条流的形态
//（TCP 还连着，60 秒内零字节）。
type scriptedSSEBody struct {
	chunks [][]byte
	idx    int
	ctx    context.Context
}

func (b *scriptedSSEBody) Read(p []byte) (int, error) {
	if b.idx < len(b.chunks) {
		n := copy(p, b.chunks[b.idx])
		b.idx++
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *scriptedSSEBody) Close() error { return nil }

func sseEvent(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

// 生产那条流在断气前真实吐过的前缀（按 #450835 的插件诊断还原）。
func reproPrefix() [][]byte {
	return [][]byte{
		sseEvent("ping", `{"type":"ping"}`),
		sseEvent("response.created", `{"type":"response.created","response":{"id":"resp_repro","status":"in_progress"}}`),
		sseEvent("response.in_progress", `{"type":"response.in_progress","response":{"id":"resp_repro","status":"in_progress"}}`),
		sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"读取仓库结构"}`),
		sseEvent("response.reasoning_summary_text.done", `{"type":"response.reasoning_summary_text.done","text":"读取仓库结构"}`),
	}
}

// 上游宣布要调用 apply_patch，但 input still 为空——参数一个字符都还没生成。
func announceApplyPatch() []byte {
	return sseEvent("response.output_item.added",
		`{"type":"response.output_item.added","output_index":2,"sequence_number":73,`+
			`"item":{"type":"custom_tool_call","name":"apply_patch","input":"","status":"in_progress","call_id":"call_repro"}}`)
}

// runStreamWithStallGuard 按 forward.go 的做法给 body 包上读空闲守卫，再走真正的
// 流式处理函数，返回判决、回给客户端的字节、以及插件打的诊断日志。
func runStreamWithStallGuard(t *testing.T, chunks [][]byte, idle time.Duration) (sdk.ForwardOutcome, string, string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := &scriptedSSEBody{chunks: chunks, ctx: ctx}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       newStallGuardBody(body, idle, cancel),
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	recorder := httptest.NewRecorder()
	outcome, _ := handleStreamResponseWithOptions(logger, resp, recorder, time.Now(), "", streamResponseOptions{})
	return outcome, recorder.Body.String(), logs.String()
}

// H2：上游宣布 apply_patch 后断气。预期产出生产那条 WARN 的完整签名。
func TestReproToolCallAnnouncedThenSilence(t *testing.T) {
	chunks := append(reproPrefix(), announceApplyPatch())
	outcome, clientBytes, logs := runStreamWithStallGuard(t, chunks, 300*time.Millisecond)

	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("判决 = %v, want %v", outcome.Kind, sdk.OutcomeStreamAborted)
	}
	if !strings.Contains(outcome.Reason, "context canceled") {
		t.Fatalf("Reason = %q, 应含 context canceled（守卫取消上游 ctx 的痕迹）", outcome.Reason)
	}

	// 生产签名逐条比对：完成事件为空、finish_reason 停在 in_progress、
	// 最后一个事件是 added（而不是 done / completed）。
	for _, want := range []string{
		`completion_event=""`,
		"finish_reason=in_progress",
		"last_raw_preview=\"event: response.output_item.added\"",
		"stream_started=true",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("诊断日志缺少生产签名 %s\n--- 实际 ---\n%s", want, logs)
		}
	}

	// 客户端侧：已经收到了推理摘要（所以换号重放已不可能），末尾是我们补的错误帧。
	if !strings.Contains(clientBytes, "response.reasoning_summary_text.delta") {
		t.Fatalf("客户端应已收到推理摘要，实际:\n%s", clientBytes)
	}
	if strings.Contains(clientBytes, "response.completed") {
		t.Fatalf("上游从未发出 completed，客户端不该看到它:\n%s", clientBytes)
	}

	t.Logf("=== H2 客户端收到的尾部 ===\n%s", tailOf(clientBytes, 400))
	t.Logf("=== H2 判决 ===\nKind=%v Reason=%q Usage=%v", outcome.Kind, outcome.Reason, outcome.Usage)
}

// H1：上游把工具调用完整吐出并发了 completed，之后连接照样静默——这正是
// 「客户端拿到工具调用、去本地执行 apply_patch」那段时间的形态。
// 预期：判成功，守卫压根不参与；即客户端本地执行工具多久都与我们无关。
func TestReproToolCallCompletedThenSilenceIsSuccess(t *testing.T) {
	chunks := append(reproPrefix(),
		announceApplyPatch(),
		sseEvent("response.custom_tool_call_input.delta",
			`{"type":"response.custom_tool_call_input.delta","delta":"*** Begin Patch","output_index":2}`),
		sseEvent("response.output_item.done",
			`{"type":"response.output_item.done","output_index":2,`+
				`"item":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** End Patch","status":"completed","call_id":"call_repro"}}`),
		sseEvent("response.completed",
			`{"type":"response.completed","response":{"id":"resp_repro","status":"completed",`+
				`"usage":{"input_tokens":1200,"output_tokens":64,"input_tokens_details":{"cached_tokens":0}}}}`),
	)

	// 守卫窗口极短（10ms）却仍应判成功：收到 completed 就收尾，之后的静默与我们无关。
	outcome, clientBytes, _ := runStreamWithStallGuard(t, chunks, 10*time.Millisecond)

	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("判决 = %v, want %v（工具调用吐完整 + completed 即成功）", outcome.Kind, sdk.OutcomeSuccess)
	}
	if !strings.Contains(clientBytes, "response.completed") {
		t.Fatalf("客户端应收到 completed:\n%s", tailOf(clientBytes, 400))
	}
	if outcome.Usage == nil {
		t.Fatal("成功分支应带 usage（生产那条失败行 usage 全 0，正是因为走不到这里）")
	}

	t.Logf("=== H1 判决 ===\nKind=%v Usage=%+v", outcome.Kind, outcome.Usage)
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
