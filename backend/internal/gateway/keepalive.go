package gateway

import (
	"context"
	"encoding/json"
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
	w      http.ResponseWriter
	cancel context.CancelFunc
	done   chan struct{}
	wrote  atomic.Bool
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
}

func startSSECommentKeepAlive(w http.ResponseWriter, interval time.Duration) *sseCommentKeepAlive {
	if w == nil || interval <= 0 {
		return nil
	}
	ka := &sseCommentKeepAlive{
		w:        w,
		interval: interval,
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
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
			if _, err := ka.w.Write([]byte(responseStreamKeepAliveComment)); err != nil {
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

func startSSEPingKeepAlive(w http.ResponseWriter) *ssePingKeepAlive {
	if w == nil {
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ctx, cancel := context.WithCancel(context.Background())
	ka := &ssePingKeepAlive{w: w, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(ka.done)
		t := time.NewTicker(imageKeepAliveInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ka.wrote.Store(true)
				writeSSEPing(w)
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

func (ka *ssePingKeepAlive) Wrote() bool {
	if ka == nil {
		return false
	}
	return ka.wrote.Load()
}

func writeSSEErrorIfStarted(w http.ResponseWriter, ka *ssePingKeepAlive, message string) {
	if ka == nil || !ka.Wrote() {
		return
	}
	writeSSEError(w, message)
}

func writeSSEPing(w http.ResponseWriter) {
	_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEData(w http.ResponseWriter, data []byte) {
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEDone(w http.ResponseWriter) {
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, message string) {
	if message != imageTooLargeSSEErrorMessage {
		message = sanitizedImageSSEErrorMessage
	}
	errEvent, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
		},
	})
	writeSSEData(w, errEvent)
	writeSSEDone(w)
}

func writeImagesRESTSSE(w http.ResponseWriter, body []byte) {
	writeSSEData(w, body)
	writeSSEDone(w)
}
