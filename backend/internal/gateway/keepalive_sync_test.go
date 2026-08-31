package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// fakeSyncWriter 记录写入行为的测试 Writer;可注入写失败。
type fakeSyncWriter struct {
	mu          sync.Mutex
	header      http.Header
	code        int
	body        []byte
	writeErr    error
	flushCalled bool
}

func (w *fakeSyncWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *fakeSyncWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.body = append(w.body, b...)
	return len(b), nil
}

func (w *fakeSyncWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
}

func (w *fakeSyncWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushCalled = true
}

func (w *fakeSyncWriter) snapshot() (int, string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ct := ""
	if w.header != nil {
		ct = w.header.Get("Content-Type")
	}
	return w.code, ct, string(w.body)
}

func TestJSONSyncKeepAlive_StopBeforeGraceWritesNothing(t *testing.T) {
	w := &fakeSyncWriter{}
	ka := startJSONSyncKeepAliveWithTiming(w, 200*time.Millisecond, 50*time.Millisecond, nil)
	time.Sleep(30 * time.Millisecond)
	ka.Stop()
	code, ct, body := w.snapshot()
	if code != 0 || ct != "" || body != "" {
		t.Errorf("宽限期内停止不应写任何字节: code=%d ct=%q body=%q", code, ct, body)
	}
	if ka.Started() {
		t.Error("Started 应为 false")
	}
	if ka.Err() != nil {
		t.Errorf("Err 应为 nil, got %v", ka.Err())
	}
}

func TestJSONSyncKeepAlive_BeatsAfterGrace(t *testing.T) {
	w := &fakeSyncWriter{}
	ka := startJSONSyncKeepAliveWithTiming(w, 20*time.Millisecond, 20*time.Millisecond, nil)
	time.Sleep(110 * time.Millisecond)
	ka.Stop()
	code, ct, body := w.snapshot()
	if !ka.Started() {
		t.Fatal("Started 应为 true")
	}
	if code != http.StatusOK {
		t.Errorf("状态码应定格 200, got %d", code)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json, got %q", ct)
	}
	if len(body) < 2 {
		t.Errorf("应至少写出 2 拍心跳, got %d 字节", len(body))
	}
	if strings.TrimSpace(body) != "" {
		t.Errorf("心跳只允许 JSON 前导空白, got %q", body)
	}
	if !w.flushCalled {
		t.Error("每拍后应 Flush")
	}
}

func TestJSONSyncKeepAlive_WriteErrorFiresOnError(t *testing.T) {
	w := &fakeSyncWriter{writeErr: errors.New("client gone")}
	fired := make(chan error, 1)
	ka := startJSONSyncKeepAliveWithTiming(w, 10*time.Millisecond, 20*time.Millisecond, func(err error) { fired <- err })
	select {
	case err := <-fired:
		if err == nil {
			t.Fatal("onError 应携带非空错误")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("写失败应触发 onError")
	}
	ka.Stop()
	if ka.Err() == nil {
		t.Error("Err 应记录下游写失败")
	}
}

func TestJSONSyncKeepAlive_NilWriterNoop(t *testing.T) {
	if ka := startJSONSyncKeepAlive(nil, nil); ka != nil {
		t.Error("nil Writer 应返回 nil(Stop/Err 均安全)")
	}
	var ka *jsonSyncKeepAlive
	ka.Stop()
	if ka.Started() || ka.Err() != nil {
		t.Error("nil 接收者应安全返回零值")
	}
}

func TestWithImagesSyncKeepAlive_FastPathUnchanged(t *testing.T) {
	g := &OpenAIGateway{}
	w := &fakeSyncWriter{}
	want := sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Upstream: sdk.UpstreamResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}}
	outcome, err := g.withImagesSyncKeepAlive(context.Background(), w, func(context.Context) (sdk.ForwardOutcome, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess || string(outcome.Upstream.Body) != `{"data":[]}` {
		t.Errorf("快路径 outcome 应原样透传: %+v", outcome)
	}
	if code, _, body := func() (int, string, string) { return w.snapshot() }(); code != 0 || body != "" {
		t.Errorf("快路径不应写任何字节: code=%d body=%q", code, body)
	}
}

func TestWithImagesSyncKeepAlive_ClientGoneCancelsDispatch(t *testing.T) {
	g := &OpenAIGateway{}
	w := &fakeSyncWriter{writeErr: errors.New("broken pipe")}
	// 缩短宽限期让写失败尽快发生:直接用底层构造。
	dispatchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ka := startJSONSyncKeepAliveWithTiming(w, 10*time.Millisecond, 20*time.Millisecond, func(error) { cancel() })
	<-dispatchCtx.Done() // 写失败应取消 dispatch
	ka.Stop()
	outcome, err := g.withImagesSyncKeepAliveResult(ka, sdk.ForwardOutcome{Kind: sdk.OutcomeUpstreamTransient}, dispatchCtx.Err())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Errorf("客户端断开且非 Success 应转 StreamAborted, got %v", outcome.Kind)
	}
}

func TestWithImagesSyncKeepAlive_SuccessSurvivesClientGone(t *testing.T) {
	g := &OpenAIGateway{}
	w := &fakeSyncWriter{writeErr: errors.New("broken pipe")}
	ka := startJSONSyncKeepAliveWithTiming(w, 10*time.Millisecond, 20*time.Millisecond, nil)
	time.Sleep(60 * time.Millisecond)
	ka.Stop()
	success := sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Usage: &sdk.Usage{Model: "gpt-image-2"}}
	outcome, err := g.withImagesSyncKeepAliveResult(ka, success, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess || outcome.Usage == nil {
		t.Errorf("完成边界断开应保留 Success 与计费: %+v", outcome)
	}
}
