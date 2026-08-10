package gateway

import (
	"context"
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

func writeImagesRESTSSE(w http.ResponseWriter, body []byte) error {
	if err := writeSSEData(w, body); err != nil {
		return err
	}
	return writeSSEDone(w)
}
