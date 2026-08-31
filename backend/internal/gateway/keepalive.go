package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// imageKeepAliveInterval 控制长耗时图片生成请求的 SSE ping 频率，避免 Cloudflare 524。
// Cloudflare 免费层源站超时约 100 秒，30 秒能留出足够余量。
const imageKeepAliveInterval = 30 * time.Second

// responseStreamKeepAliveInterval keeps long reasoning streams below the
// Cloudflare proxy read timeout without exposing account-specific SSE events.
const responseStreamKeepAliveInterval = 10 * time.Second

const responseStreamKeepAliveComment = ": hopbase-keepalive\n\n"

type ssePingKeepAlive struct {
	w       http.ResponseWriter
	cancel  context.CancelFunc
	done    chan struct{}
	wrote   atomic.Bool
	errMu   sync.RWMutex
	err     error
	onError func(error)
}

// synchronizedResponseWriter serializes heartbeat and upstream SSE writes.
// Plugin response writers are backed by a gRPC stream and are not safe for
// concurrent Send calls.
type synchronizedResponseWriter struct {
	http.ResponseWriter
	mu sync.Mutex
}

func (w *synchronizedResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *synchronizedResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Write(data)
}

func (w *synchronizedResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type sseCommentKeepAlive struct {
	w        http.ResponseWriter
	interval time.Duration
	done     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	errMu    sync.RWMutex
	err      error
	onError  func(error)
}

func startSSECommentKeepAlive(w http.ResponseWriter, interval time.Duration, onError ...func(error)) *sseCommentKeepAlive {
	if w == nil || interval <= 0 {
		return nil
	}
	ka := &sseCommentKeepAlive{
		w:        w,
		interval: interval,
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
	if len(onError) > 0 {
		ka.onError = onError[0]
	}
	go ka.run()
	return ka
}

func (ka *sseCommentKeepAlive) run() {
	defer close(ka.done)
	ticker := time.NewTicker(ka.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ka.stop:
			return
		case <-ticker.C:
			if err := writeResponsePayload(ka.w, []byte(responseStreamKeepAliveComment)); err != nil {
				ka.setError(newDownstreamWriteError(err))
				return
			}
			flushResponseWriter(ka.w)
		}
	}
}

func (ka *sseCommentKeepAlive) Stop() {
	if ka == nil {
		return
	}
	ka.stopOnce.Do(func() { close(ka.stop) })
	<-ka.done
}

func (ka *sseCommentKeepAlive) Err() error {
	if ka == nil {
		return nil
	}
	ka.errMu.RLock()
	defer ka.errMu.RUnlock()
	return ka.err
}

func (ka *sseCommentKeepAlive) setError(err error) {
	if ka == nil || err == nil {
		return
	}
	ka.errMu.Lock()
	if ka.err != nil {
		ka.errMu.Unlock()
		return
	}
	ka.err = err
	onError := ka.onError
	ka.errMu.Unlock()
	if onError != nil {
		onError(err)
	}
}

func startSSEPingKeepAlive(w http.ResponseWriter, onError ...func(error)) *ssePingKeepAlive {
	return startSSEPingKeepAliveWithInterval(w, imageKeepAliveInterval, onError...)
}

func startSSEPingKeepAliveWithInterval(w http.ResponseWriter, interval time.Duration, onError ...func(error)) *ssePingKeepAlive {
	if w == nil {
		return nil
	}
	if interval <= 0 {
		interval = imageKeepAliveInterval
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ctx, cancel := context.WithCancel(context.Background())
	ka := &ssePingKeepAlive{w: w, cancel: cancel, done: make(chan struct{})}
	if len(onError) > 0 {
		ka.onError = onError[0]
	}
	go func() {
		defer close(ka.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeSSEPing(w); err != nil {
					ka.setError(newDownstreamWriteError(err))
					return
				}
				ka.wrote.Store(true)
			}
		}
	}()
	return ka
}

func (ka *ssePingKeepAlive) Stop() {
	if ka == nil {
		return
	}
	ka.cancel()
	<-ka.done
}

func stopSSEPingKeepAlive(ka *ssePingKeepAlive) error {
	if ka == nil {
		return nil
	}
	ka.Stop()
	return ka.Err()
}

func (ka *ssePingKeepAlive) Wrote() bool {
	if ka == nil {
		return false
	}
	return ka.wrote.Load()
}

func (ka *ssePingKeepAlive) Err() error {
	if ka == nil {
		return nil
	}
	ka.errMu.RLock()
	defer ka.errMu.RUnlock()
	return ka.err
}

func (ka *ssePingKeepAlive) setError(err error) {
	if ka == nil || err == nil {
		return
	}
	ka.errMu.Lock()
	if ka.err != nil {
		ka.errMu.Unlock()
		return
	}
	ka.err = err
	onError := ka.onError
	ka.errMu.Unlock()
	if onError != nil {
		onError(err)
	}
}

func writeResponsePayload(w http.ResponseWriter, payload []byte) error {
	if w == nil {
		return io.ErrClosedPipe
	}
	n, err := w.Write(payload)
	if err == nil && n != len(payload) {
		err = io.ErrShortWrite
	}
	return err
}

func writeSSEPing(w http.ResponseWriter) error {
	if err := writeResponsePayload(w, []byte(responseStreamKeepAliveComment)); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEData(w http.ResponseWriter, data []byte) error {
	if err := writeResponsePayload(w, []byte("data: ")); err != nil {
		return err
	}
	if err := writeResponsePayload(w, data); err != nil {
		return err
	}
	if err := writeResponsePayload(w, []byte("\n\n")); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter) error {
	if err := writeResponsePayload(w, []byte("data: [DONE]\n\n")); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func writeSSEError(w http.ResponseWriter, message string) error {
	if message != imageTooLargeSSEErrorMessage {
		message = sanitizedImageSSEErrorMessage
	}
	errEvent, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
		},
	})
	if err := writeSSEData(w, errEvent); err != nil {
		return err
	}
	return writeSSEDone(w)
}

func writeImagesRESTSSE(w http.ResponseWriter, body []byte) error {
	if err := writeSSEData(w, body); err != nil {
		return err
	}
	return writeSSEDone(w)
}

// ── 同步 JSON 图像请求的边缘保活 ──
//
// CF 代理层对「源站 ~100 秒无字节」的连接会切断,而图像上游延迟方差实测 16s~125s+:
// 同步请求撞上慢路径时,上游照常完成并计成本,客户端却永远拿不到响应(2026-08-31
// 生产实测,Caddy 记录 200@125s 而客户端已被边缘断开)。解法:等待超过宽限期后先
// 定格 200 + JSON 头,按周期写出 JSON 前导空白(RFC 8259 合法,主流解析器无感)保持
// 字节流动;最终响应仍经 outcome 由 core 追加写出。
//
// 代价与边界:心跳启动后状态码已定格 200,此后的失败只能以 200 + 错误 JSON body
// 返回——但这类请求在旧行为下会被边缘直接切断、客户端什么都拿不到,不存在比现状
// 更差的路径;宽限期内完成或失败的请求(绝大多数)语义与旧行为完全一致。
//
// core 侧配套:非流式的 POST /v1/images/{generations,edits} 也传入客户端 Writer,
// 并以 X-Airgate-Images-Sync-Writer: 1 标记(见 core buildPluginRequest)。

// headerImagesSyncWriter 是 core 标记「同步图像请求带真实客户端 Writer」的约定头。
const headerImagesSyncWriter = "X-Airgate-Images-Sync-Writer"

const (
	// imagesSyncKeepAliveGrace 首拍前的宽限期:期内完成的请求保持完全标准的同步语义。
	imagesSyncKeepAliveGrace = 40 * time.Second
	// imagesSyncKeepAliveInterval 首拍后的心跳间隔:打点 40s/65s/90s…,任意无字节窗口 <100s。
	imagesSyncKeepAliveInterval = 25 * time.Second
)

type jsonSyncKeepAlive struct {
	w        http.ResponseWriter
	grace    time.Duration
	interval time.Duration
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	started  atomic.Bool
	onError  func(error)
	errMu    sync.RWMutex
	err      error
}

func startJSONSyncKeepAlive(w http.ResponseWriter, onError func(error)) *jsonSyncKeepAlive {
	return startJSONSyncKeepAliveWithTiming(w, imagesSyncKeepAliveGrace, imagesSyncKeepAliveInterval, onError)
}

func startJSONSyncKeepAliveWithTiming(w http.ResponseWriter, grace, interval time.Duration, onError func(error)) *jsonSyncKeepAlive {
	if w == nil || interval <= 0 {
		return nil
	}
	ka := &jsonSyncKeepAlive{
		w:        w,
		grace:    grace,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		onError:  onError,
	}
	go ka.run()
	return ka
}

func (ka *jsonSyncKeepAlive) run() {
	defer close(ka.done)
	graceTimer := time.NewTimer(ka.grace)
	defer graceTimer.Stop()
	select {
	case <-ka.stop:
		return
	case <-graceTimer.C:
	}
	// 首拍:定格响应头。此后最终响应只能追加在空白之后(状态码已 200)。
	ka.w.Header().Set("Content-Type", "application/json")
	ka.w.WriteHeader(http.StatusOK)
	ka.started.Store(true)
	if !ka.beat() {
		return
	}
	ticker := time.NewTicker(ka.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ka.stop:
			return
		case <-ticker.C:
			if !ka.beat() {
				return
			}
		}
	}
}

func (ka *jsonSyncKeepAlive) beat() bool {
	if err := writeResponsePayload(ka.w, []byte(" ")); err != nil {
		ka.setError(newDownstreamWriteError(err))
		return false
	}
	flushResponseWriter(ka.w)
	return true
}

func (ka *jsonSyncKeepAlive) Stop() {
	if ka == nil {
		return
	}
	ka.stopOnce.Do(func() { close(ka.stop) })
	<-ka.done
}

// Started 返回是否已写出首拍(即响应头是否已定格 200)。
func (ka *jsonSyncKeepAlive) Started() bool {
	if ka == nil {
		return false
	}
	return ka.started.Load()
}

func (ka *jsonSyncKeepAlive) Err() error {
	if ka == nil {
		return nil
	}
	ka.errMu.RLock()
	defer ka.errMu.RUnlock()
	return ka.err
}

func (ka *jsonSyncKeepAlive) setError(err error) {
	if ka == nil || err == nil {
		return
	}
	ka.errMu.Lock()
	if ka.err != nil {
		ka.errMu.Unlock()
		return
	}
	ka.err = err
	onError := ka.onError
	ka.errMu.Unlock()
	if onError != nil {
		onError(err)
	}
}
