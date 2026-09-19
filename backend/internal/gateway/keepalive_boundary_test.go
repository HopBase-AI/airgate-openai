package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// keepalive_boundary_test.go —— 心跳在出字后继续发送的安全性与有效性。
//
// 背景：2026-09-19 把读空闲上限放宽到 150s 的同时，让心跳持续整条流（原先首帧写出
// 即停）。持续发心跳的前提是绝不把 SSE 事件劈开——上游一个事件由三次 Write 组成
//（"event: x" / "data: y" / 空行），心跳插在中间会让客户端把半个事件当成无 data 的
// 事件丢弃、把剩下的 data 行当成无类型事件，整条流就坏了。

// TestKeepAliveNeverSplitsSSEEvent 并发压力下的不变式：输出流里每个 "event:" 行的
// 下一行必须是它自己的 "data:" 行，中间不允许出现心跳帧。
//
// 心跳间隔压到 1ms、事件写入之间插入让出，制造远高于生产的插入频率。
func TestKeepAliveNeverSplitsSSEEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newSynchronizedResponseWriter(recorder)

	ka := startSSECommentKeepAlive(w, time.Millisecond)
	if ka == nil {
		t.Fatal("心跳未启动")
	}

	// 模拟主循环：逐行写出 300 个事件，每行之间让出调度，给心跳最大的插入机会。
	const events = 300
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < events; i++ {
			for _, line := range []string{
				"event: response.output_text.delta",
				`data: {"type":"response.output_text.delta","delta":"x"}`,
				"",
			} {
				if _, err := w.Write([]byte(line + "\n")); err != nil {
					t.Errorf("写出失败: %v", err)
					return
				}
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	wg.Wait()
	ka.Stop()

	lines := strings.Split(recorder.Body.String(), "\n")
	eventLines := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		eventLines++
		if i+1 >= len(lines) {
			t.Fatalf("第 %d 行 %q 之后没有内容", i, line)
		}
		if next := lines[i+1]; !strings.HasPrefix(next, "data: ") {
			t.Fatalf("事件被劈开了：第 %d 行 %q 之后是 %q，期望 data: 行", i, line, next)
		}
	}
	if eventLines != events {
		t.Fatalf("写出的事件数 = %d, want %d", eventLines, events)
	}
	// 反向确认：这次压测里心跳确实插入过，否则上面的断言是空过的。
	if !strings.Contains(recorder.Body.String(), strings.TrimSpace(responseStreamKeepAliveComment)) {
		t.Fatal("整轮压测一个心跳帧都没插入，本用例没有真正验证到边界安全")
	}
}

// TestKeepAliveContinuesDuringUpstreamSilence 上游宣布工具调用后长时间静默时，
// 客户端必须持续收到心跳——这正是放宽读空闲上限后要守住的东西：
// 窗口放到 150s，但客户端若在静默期间自己断开，放宽就白做了。
func TestKeepAliveContinuesDuringUpstreamSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chunks := append(reproPrefix(), announceApplyPatch())
	body := &scriptedSSEBody{chunks: chunks, ctx: ctx}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		// 读空闲上限 600ms：留出足够时间让 20ms 一拍的心跳打出多帧。
		Body: newStallGuardBody(body, 600*time.Millisecond, cancel),
	}

	recorder := httptest.NewRecorder()
	outcome, _ := handleStreamResponseWithKeepAliveOptions(
		nil, resp, recorder, time.Now(), "", 20*time.Millisecond, streamResponseOptions{})

	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("判决 = %v, want %v", outcome.Kind, sdk.OutcomeStreamAborted)
	}

	out := recorder.Body.String()
	beats := strings.Count(out, strings.TrimSpace(responseStreamKeepAliveComment))
	if beats < 3 {
		t.Fatalf("静默期间只收到 %d 个心跳帧，期望 ≥3（改动前是 0：首帧写出即停）\n--- 输出 ---\n%s", beats, out)
	}

	// 心跳必须落在事件之间，不能劈开宣布工具调用的那个事件。
	idx := strings.Index(out, "event: response.output_item.added")
	if idx < 0 {
		t.Fatalf("客户端没收到工具调用事件:\n%s", out)
	}
	rest := out[idx:]
	nl := strings.Index(rest, "\n")
	if nl < 0 || !strings.HasPrefix(rest[nl+1:], "data: ") {
		if len(rest) > 300 {
			rest = rest[:300]
		}
		t.Fatalf("工具调用事件被心跳劈开了:\n%s", rest)
	}
}

// TestStreamIdleTimeoutDefaults 守卫默认值：读空闲 150s（2026-09-19 由 60s 放宽，
// 依据是实测「宣布工具调用→首个参数」最长 42.96s），账号级覆盖仍然优先。
func TestStreamIdleTimeoutDefaults(t *testing.T) {
	if defaultStreamIdleTimeout != 150*time.Second {
		t.Fatalf("defaultStreamIdleTimeout = %v, want 150s", defaultStreamIdleTimeout)
	}
	g := &OpenAIGateway{}
	if got := g.streamIdleTimeoutFor(nil); got != 150*time.Second {
		t.Fatalf("无账号覆盖时 = %v, want 150s", got)
	}
}
