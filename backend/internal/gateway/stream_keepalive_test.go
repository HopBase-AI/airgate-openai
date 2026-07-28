package gateway

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

type signalingResponseWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
	writes chan []byte
}

func newSignalingResponseWriter() *signalingResponseWriter {
	return &signalingResponseWriter{
		header: make(http.Header),
		writes: make(chan []byte, 8),
	}
}

func (w *signalingResponseWriter) Header() http.Header { return w.header }

func (w *signalingResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = statusCode
	}
}

func (w *signalingResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_, _ = w.body.Write(data)
	w.mu.Unlock()
	copyOfData := append([]byte(nil), data...)
	w.writes <- copyOfData
	return len(data), nil
}

func (w *signalingResponseWriter) Flush() {}

func (w *signalingResponseWriter) BodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func waitForHeartbeat(t *testing.T, w *signalingResponseWriter) {
	t.Helper()
	select {
	case data := <-w.writes:
		if got := string(data); got != responseStreamKeepAliveComment {
			t.Fatalf("first write = %q, want heartbeat %q", got, responseStreamKeepAliveComment)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream heartbeat")
	}
}

func TestHandleStreamResponseHeartbeatPrecedesDelayedOutput(t *testing.T) {
	reader, upstream := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	w := newSignalingResponseWriter()
	type result struct {
		outcome sdk.ForwardOutcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := handleStreamResponseWithKeepAlive(nil, resp, w, time.Now(), "", 10*time.Millisecond)
		resultCh <- result{outcome: outcome, err: err}
	}()

	waitForHeartbeat(t, w)
	stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"
	if _, err := upstream.Write([]byte(stream)); err != nil {
		t.Fatalf("write upstream stream: %v", err)
	}
	if err := upstream.Close(); err != nil {
		t.Fatalf("close upstream stream: %v", err)
	}

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("handleStreamResponseWithKeepAlive() error = %v", got.err)
	}
	if got.outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", got.outcome.Kind)
	}
	body := w.BodyString()
	if !bytes.HasPrefix([]byte(body), []byte(responseStreamKeepAliveComment)) {
		t.Fatalf("body does not start with heartbeat: %q", body)
	}
	if !bytes.Contains([]byte(body), []byte(`"type":"response.created"`)) ||
		!bytes.Contains([]byte(body), []byte(`"delta":"ok"`)) {
		t.Fatalf("buffered upstream events missing from body: %q", body)
	}
}

func TestHandleStreamResponseHeartbeatDoesNotCommitFailedAttempt(t *testing.T) {
	reader, upstream := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       reader,
	}
	w := newSignalingResponseWriter()
	type result struct {
		outcome sdk.ForwardOutcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := handleStreamResponseWithKeepAlive(nil, resp, w, time.Now(), "", 10*time.Millisecond)
		resultCh <- result{outcome: outcome, err: err}
	}()

	waitForHeartbeat(t, w)
	failure := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"secret upstream failure\"}}}\n\n"
	if _, err := upstream.Write([]byte(failure)); err != nil {
		t.Fatalf("write upstream failure: %v", err)
	}
	if err := upstream.Close(); err != nil {
		t.Fatalf("close upstream stream: %v", err)
	}

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("handleStreamResponseWithKeepAlive() error = %v", got.err)
	}
	if !got.outcome.Kind.ShouldFailover() {
		t.Fatalf("outcome kind = %v, want failover-eligible", got.outcome.Kind)
	}
	if body := w.BodyString(); body != responseStreamKeepAliveComment {
		t.Fatalf("failed attempt leaked upstream data: %q", body)
	}
}
