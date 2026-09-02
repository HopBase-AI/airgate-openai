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

// hedge_test.go —— 首字前双发对冲。

func TestHedgeGate_ClaimAndDrop(t *testing.T) {
	rec := httptest.NewRecorder()
	gate := newHedgeGate(rec)
	w1, w2 := gate.writer(1), gate.writer(2)
	w1.Header().Set("Content-Type", "text/event-stream")
	w2.Header().Set("Content-Type", "text/event-stream")

	// 无人认领时保活注释任何一路都可写,且不构成认领
	if _, err := w1.Write([]byte(": hopbase-keepalive\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte(": hopbase-keepalive\n\n")); err != nil {
		t.Fatal(err)
	}
	if gate.ownerID() != 0 {
		t.Fatalf("保活注释不应认领,owner=%d", gate.ownerID())
	}
	// 第二路先写应用数据 → 认领;第一路之后的数据被丢
	w2.WriteHeader(http.StatusOK)
	_, _ = w2.Write([]byte("data: {\"from\":2}\n\n"))
	_, _ = w1.Write([]byte("data: {\"from\":1}\n\n"))
	_, _ = w1.Write([]byte(": late-keepalive\n\n"))
	if gate.ownerID() != 2 {
		t.Fatalf("owner = %d, want 2", gate.ownerID())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `{"from":2}`) || strings.Contains(body, `{"from":1}`) || strings.Contains(body, "late-keepalive") {
		t.Fatalf("门闸放错了数据: %q", body)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("认领方暂存的响应头未落到真实 writer: %v", rec.Header())
	}
	gate.close()
	_, _ = w2.Write([]byte("data: {\"after\":\"close\"}\n\n"))
	if strings.Contains(rec.Body.String(), "after") {
		t.Fatal("close 之后仍写入了客户端")
	}
}

// hedgeServer 第 n 次请求按 behaviors[n-1] 行为;"stall"=只发保活直到请求被取消,"ok"=正常出字,"eof"=200 后立刻断。
func hedgeServer(t *testing.T, behaviors ...string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := int(atomic.AddInt32(&hits, 1))
		b := "ok"
		if n-1 < len(behaviors) {
			b = behaviors[n-1]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		switch b {
		case "stall":
			for {
				_, _ = w.Write([]byte(": upstream-ping\n\n"))
				fl.Flush()
				select {
				case <-r.Context().Done():
					return
				case <-done:
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		case "eof":
			return
		default:
			_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi-from-" + b + "\"}}]}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"))
			fl.Flush()
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	return srv, &hits
}

func hedgeStart(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("first attempt: %v", err)
	}
	return resp, cancel
}

func hedgeSecondStarter(url string) hedgeAttemptStarter {
	return func(ctx context.Context) (*http.Response, context.CancelFunc, error) {
		ctx2, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(ctx2, http.MethodPost, url, strings.NewReader(`{"model":"gpt-5.6-sol","stream":true}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		return resp, cancel, nil
	}
}

var hedgeLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// 主路卡住不出字,对冲路正常:客户端拿到对冲路的内容,主路被取消。
func TestRunHedgedStream_SecondWinsWhenFirstStalls(t *testing.T) {
	srv, hits := hedgeServer(t, "stall", "ok2")
	resp, cancel := hedgeStart(t, srv.URL)
	defer cancel()
	rec := httptest.NewRecorder()
	start := time.Now()
	outcome, err := runHedgedStream(context.Background(), hedgeLogger, resp, cancel, rec, start, "",
		streamResponseOptions{firstOutputTimeout: 5 * time.Second}, 200*time.Millisecond, hedgeSecondStarter(srv.URL))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, reason=%q", outcome.Kind, outcome.Reason)
	}
	if body := rec.Body.String(); !strings.Contains(body, "hi-from-ok2") || strings.Contains(body, "upstream-ping") {
		t.Fatalf("客户端应只收到对冲路的内容(上游保活不透传): %q", body)
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("上游应收到 2 次请求,got %d", *hits)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("耗时 %v,对冲没有及时接管", elapsed)
	}
}

// 主路很快出字:对冲不触发,上游只收到 1 次请求。
func TestRunHedgedStream_FirstWinsWithoutHedge(t *testing.T) {
	srv, hits := hedgeServer(t, "ok1", "ok2")
	resp, cancel := hedgeStart(t, srv.URL)
	defer cancel()
	rec := httptest.NewRecorder()
	outcome, err := runHedgedStream(context.Background(), hedgeLogger, resp, cancel, rec, time.Now(), "",
		streamResponseOptions{firstOutputTimeout: 5 * time.Second}, 500*time.Millisecond, hedgeSecondStarter(srv.URL))
	if err != nil || outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome=%v err=%v", outcome.Kind, err)
	}
	if !strings.Contains(rec.Body.String(), "hi-from-ok1") {
		t.Fatalf("body=%q", rec.Body.String())
	}
	time.Sleep(600 * time.Millisecond)
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("主路已胜出,不应再发对冲,hits=%d", *hits)
	}
}

// 两路都不出字:按主路的看门狗判决返回可换号的瞬时故障,客户端没有收到任何应用数据。
func TestRunHedgedStream_BothStallReturnsPrimaryFailure(t *testing.T) {
	srv, hits := hedgeServer(t, "stall", "stall")
	resp, cancel := hedgeStart(t, srv.URL)
	defer cancel()
	rec := httptest.NewRecorder()
	outcome, _ := runHedgedStream(context.Background(), hedgeLogger, resp, cancel, rec, time.Now(), "",
		streamResponseOptions{firstOutputTimeout: 400 * time.Millisecond}, 100*time.Millisecond, hedgeSecondStarter(srv.URL))
	if outcome.Kind != sdk.OutcomeUpstreamTransient || !outcome.Kind.ShouldFailover() {
		t.Fatalf("Kind = %v, want transient failover; reason=%q", outcome.Kind, outcome.Reason)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("两路都没出字却写了应用数据: %q", rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 2 {
		t.Fatalf("hits=%d", *hits)
	}
}

// 主路在对冲触发前就失败(上游 200 后立刻断):立即交回 core 走常规 failover,不再起对冲。
func TestRunHedgedStream_PrimaryFailsBeforeHedge(t *testing.T) {
	srv, hits := hedgeServer(t, "eof", "ok2")
	resp, cancel := hedgeStart(t, srv.URL)
	defer cancel()
	rec := httptest.NewRecorder()
	start := time.Now()
	outcome, _ := runHedgedStream(context.Background(), hedgeLogger, resp, cancel, rec, start, "",
		streamResponseOptions{firstOutputTimeout: 5 * time.Second}, 2*time.Second, hedgeSecondStarter(srv.URL))
	if outcome.Kind == sdk.OutcomeSuccess {
		t.Fatalf("空流不应判成功")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("主路失败后应立即返回,耗时 %v", time.Since(start))
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("不应起对冲,hits=%d", *hits)
	}
}

func TestHedgeAfterFor(t *testing.T) {
	g := &OpenAIGateway{}
	if d := g.hedgeAfterFor(&sdk.Account{}); d != 0 {
		t.Fatalf("默认应关闭,got %v", d)
	}
	if d := g.hedgeAfterFor(&sdk.Account{Credentials: map[string]string{"hedge_after": "10s"}}); d != 10*time.Second {
		t.Fatalf("账号级开关未生效: %v", d)
	}
}
