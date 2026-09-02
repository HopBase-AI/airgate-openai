package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// hedge.go —— 首字前双发对冲(hedged first token)。
//
// 生产里最大的一类失败/变慢形态是「上游收下请求后迟迟不出第一个字」:2026-09-02 首字看门狗
// 单日开火 58 次(elapsed p50 35s),响应头阶段挂死 18 次;同一上游对同一请求重发几乎都在
// 十秒内正常返回——挂住的是上游那一次处理,不是链路。串行策略(等 30s → 换号 → 再等)
// 让客户每次都要多等 30~60s。
//
// 对冲:请求发出后 hedge_after(默认 10s)仍没有任何真实输出,就对同一上游再发一份一模一样
// 的请求;谁先出真实输出谁赢,另一路立即取消。可行的前提是插件在首个真实输出前一律缓冲
// 不下发(见 writeOrBufferSSELine),两路都处于「未提交」状态,由 hedgeGate 保证只有一路
// 能写到客户端。
//
// 边界:
//   - 只对 SSE token 流(chat/responses stream=true)生效,图像与非流式不对冲;
//   - 每请求最多对冲一次;全局同时在飞的对冲不超过 hedgeMaxInflight,上游整体变慢时不放大;
//   - 默认关闭,按账号凭证 hedge_after(如 "10s")或插件 config hedge_after 开启;
//   - 输掉的那一路上游可能仍会计费,这是用费用换首字确定性,开关留给运营决定。

const hedgeMaxInflight = 8

var hedgeInflight = make(chan struct{}, hedgeMaxInflight)

// hedgeAfterFor 对冲触发时延:账号凭证 hedge_after > 插件 config hedge_after > 关闭(0)。
func (g *OpenAIGateway) hedgeAfterFor(account *sdk.Account) time.Duration {
	if d := accountTimeoutOverride(account, "hedge_after"); d > 0 {
		return d
	}
	if g == nil || g.ctx == nil || g.ctx.Config() == nil {
		return 0
	}
	if d := g.ctx.Config().GetDuration("hedge_after"); d > 0 {
		return d
	}
	return 0
}

// hedgeGate 两路尝试共享的客户端 writer 门闸:SSE 注释(保活)在无人认领前任何一路都可写;
// 第一次写出应用数据(或显式 WriteHeader)的那一路成为 owner,此后另一路的一切写入被丢弃。
type hedgeGate struct {
	mu          sync.Mutex
	w           http.ResponseWriter
	owner       int
	headersSent bool
	closed      bool
}

func newHedgeGate(w http.ResponseWriter) *hedgeGate {
	return &hedgeGate{w: w}
}

func (g *hedgeGate) writer(id int) *hedgeWriter {
	return &hedgeWriter{gate: g, id: id, hdr: http.Header{}}
}

func (g *hedgeGate) ownerID() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owner
}

// close 之后任何写入都被丢弃:胜者已返回,输家的收尾写(错误事件/保活)不能再碰客户端。
func (g *hedgeGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// commitHeaders 把某一路暂存的响应头落到真实 writer 并发出状态行(只做一次)。
func (g *hedgeGate) commitHeadersLocked(from *hedgeWriter, status int) {
	if g.headersSent {
		return
	}
	dst := g.w.Header()
	for k, v := range from.hdr {
		dst[k] = append([]string(nil), v...)
	}
	g.w.WriteHeader(status)
	g.headersSent = true
}

// hedgeWriter 某一路尝试看到的 http.ResponseWriter。
type hedgeWriter struct {
	gate *hedgeGate
	id   int
	hdr  http.Header
}

func (w *hedgeWriter) Header() http.Header { return w.hdr }

func (w *hedgeWriter) WriteHeader(status int) {
	g := w.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if g.owner == 0 {
		g.owner = w.id
	}
	if g.owner != w.id {
		return
	}
	g.commitHeadersLocked(w, status)
}

func (w *hedgeWriter) Write(b []byte) (int, error) {
	g := w.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return len(b), nil
	}
	if !isSSECommentPayload(b) {
		if g.owner == 0 {
			g.owner = w.id
		}
		if g.owner != w.id {
			return len(b), nil
		}
	} else if g.owner != 0 && g.owner != w.id {
		return len(b), nil
	}
	g.commitHeadersLocked(w, http.StatusOK)
	return g.w.Write(b)
}

func (w *hedgeWriter) Flush() {
	g := w.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || (g.owner != 0 && g.owner != w.id) || !g.headersSent {
		return
	}
	if f, ok := g.w.(http.Flusher); ok {
		f.Flush()
	}
}

// isSSECommentPayload 只含 SSE 注释行(以 ':' 开头)与空行的载荷——保活帧,不构成应用数据。
func isSSECommentPayload(b []byte) bool {
	saw := false
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if line[0] != ':' {
			return false
		}
		saw = true
	}
	return saw
}

// hedgeAttemptStarter 发起一路上游请求:返回响应(已收到响应头)与取消函数。
type hedgeAttemptStarter func(ctx context.Context) (*http.Response, context.CancelFunc, error)

type hedgeResult struct {
	id      int
	outcome sdk.ForwardOutcome
	err     error
}

// runHedgedStream 第一路 resp 已到响应头;hedgeAfter 内无真实输出则起第二路。
// 返回胜者的判决;两路都失败时返回第一路的判决(分类口径以主路为准)。
func runHedgedStream(
	ctx context.Context,
	logger *slog.Logger,
	resp *http.Response,
	cancelFirst context.CancelFunc,
	clientWriter http.ResponseWriter,
	start time.Time,
	reqServiceTier string,
	options streamResponseOptions,
	hedgeAfter time.Duration,
	startSecond hedgeAttemptStarter,
) (sdk.ForwardOutcome, error) {
	gate := newHedgeGate(clientWriter)
	defer gate.close()
	results := make(chan hedgeResult, 2)

	go func() {
		outcome, err := handleStreamResponseWithOptions(logger, resp, gate.writer(1), start, reqServiceTier, options)
		results <- hedgeResult{id: 1, outcome: outcome, err: err}
	}()

	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()
	var (
		secondStarted bool
		cancelSecond  context.CancelFunc = func() {}
		finished                         = map[int]hedgeResult{}
	)
	for {
		select {
		case <-timer.C:
			if secondStarted || gate.ownerID() != 0 {
				continue
			}
			if _, done := finished[1]; done {
				continue
			}
			select {
			case hedgeInflight <- struct{}{}:
			default:
				logger.Warn("hedge_skipped_inflight_cap", "cap", hedgeMaxInflight)
				continue
			}
			secondStarted = true
			secondCtx, cancel := context.WithCancel(ctx)
			cancelSecond = cancel
			logger.Info("hedge_started", "after_ms", time.Since(start).Milliseconds())
			go func() {
				defer func() { <-hedgeInflight }()
				resp2, cancel2, err := startSecond(secondCtx)
				if err != nil {
					results <- hedgeResult{id: 2, outcome: upstreamTransportOutcome(secondCtx, err), err: err}
					return
				}
				defer cancel2()
				defer func() { _ = resp2.Body.Close() }()
				if resp2.StatusCode >= http.StatusBadRequest {
					body, _ := io.ReadAll(resp2.Body)
					outcome := failureOutcome(resp2.StatusCode, body, resp2.Header.Clone(), truncate(string(body), 200), extractRetryAfterHeader(resp2.Header))
					results <- hedgeResult{id: 2, outcome: outcome}
					return
				}
				outcome, err := handleStreamResponseWithOptions(logger, resp2, gate.writer(2), start, reqServiceTier, options)
				results <- hedgeResult{id: 2, outcome: outcome, err: err}
			}()
		case r := <-results:
			finished[r.id] = r
			owner := gate.ownerID()
			if owner == r.id {
				// 胜者收尾:另一路立即取消,它之后的任何写入都被门闸丢弃。
				if r.id == 1 {
					cancelSecond()
				} else {
					cancelFirst()
				}
				logger.Info("hedge_settled", "winner", r.id, "hedged", secondStarted,
					"first_token_ms", outcomeFirstTokenMs(r.outcome))
				return r.outcome, r.err
			}
			if owner != 0 {
				// 输家先结束(通常是被取消),等胜者。
				continue
			}
			// 无人认领:这一路在出字前就失败了。
			otherRunning := (r.id == 1 && secondStarted && !hasFinished(finished, 2)) || (r.id == 2 && !hasFinished(finished, 1))
			if otherRunning {
				continue
			}
			if r.id == 1 && !secondStarted {
				// 主路在对冲触发前就失败:交回 core 走常规 failover。
				return r.outcome, r.err
			}
			// 两路都没出字:以主路判决为准。
			cancelSecond()
			if first, ok := finished[1]; ok {
				return first.outcome, first.err
			}
			return r.outcome, r.err
		}
	}
}

func outcomeFirstTokenMs(o sdk.ForwardOutcome) int64 {
	if o.Usage == nil {
		return 0
	}
	return o.Usage.FirstTokenMs
}

func hasFinished(m map[int]hedgeResult, id int) bool {
	_, ok := m[id]
	return ok
}

// cloneUpstreamRequest 以同一方法/URL/头/正文重建上游请求(第一路的 body reader 已被消费)。
func cloneUpstreamRequest(ctx context.Context, method, url string, header http.Header, body []byte) (*http.Request, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	if !strings.HasPrefix(strings.ToLower(req.Header.Get("Content-Type")), "multipart/") && len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
