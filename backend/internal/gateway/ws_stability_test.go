package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const productionOverloadMessage = "server_is_overloaded: Our servers are currently overloaded. Please try again later."

var errTestDownstreamWrite = errors.New("test downstream write failed")

type failingResponseWriter struct {
	header http.Header
	failAt int
	writes int
}

type partialFailingResponseWriter struct {
	header http.Header
	limit  int
}

func newPartialFailingResponseWriter(limit int) *partialFailingResponseWriter {
	return &partialFailingResponseWriter{header: http.Header{}, limit: limit}
}

func (w *partialFailingResponseWriter) Header() http.Header { return w.header }
func (w *partialFailingResponseWriter) WriteHeader(int)     {}
func (w *partialFailingResponseWriter) Write(p []byte) (int, error) {
	n := w.limit
	if n > len(p) {
		n = len(p)
	}
	return n, errTestDownstreamWrite
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func newFailingResponseWriter(failAt int) *failingResponseWriter {
	return &failingResponseWriter{header: http.Header{}, failAt: failAt}
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(int)     {}
func (w *failingResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		return 0, errTestDownstreamWrite
	}
	return len(p), nil
}

type countingWSEventHandler struct {
	delegate WSEventHandler
	rawCalls int
}

func (h *countingWSEventHandler) OnTextDelta(delta string) {
	h.delegate.OnTextDelta(delta)
}

func (h *countingWSEventHandler) OnReasoningDelta(delta string) {
	h.delegate.OnReasoningDelta(delta)
}

func (h *countingWSEventHandler) OnRawEvent(eventType string, data []byte) {
	h.rawCalls++
	h.delegate.OnRawEvent(eventType, data)
}

func (h *countingWSEventHandler) OnRateLimits(usedPercent float64) {
	h.delegate.OnRateLimits(usedPercent)
}

func (h *countingWSEventHandler) Err() error {
	return wsEventHandlerError(h.delegate)
}

func dialGatewayTestWebSocket(t *testing.T, serve func(*websocket.Conn) error) (*websocket.Conn, <-chan error) {
	t.Helper()
	serverResult := make(chan error, 1)
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverResult <- fmt.Errorf("upgrade websocket: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()
		serverResult <- serve(conn)
	}))
	t.Cleanup(server.Close)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, serverResult
}

func writeGatewayTestEvents(conn *websocket.Conn, events ...string) error {
	for _, event := range events {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
			return err
		}
	}
	return nil
}

func waitGatewayTestWebSocket(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("websocket server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("websocket server did not finish")
	}
}

func TestFailureOutcomeProductionOverloadUsesShortRetry(t *testing.T) {
	outcome := failureOutcome(http.StatusServiceUnavailable, nil, nil, productionOverloadMessage, 0)
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", outcome.Kind)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
}

func TestFailureOutcome529UsesShortRetry(t *testing.T) {
	outcome := failureOutcome(529, nil, nil, "Please try again later.", 0)
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", outcome.Kind)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
}

func TestFailureOutcome504IsAccountNeutralAndNotReplayable(t *testing.T) {
	body := []byte(`{"error":{"message":"gateway timeout"}}`)
	outcome := failureOutcome(http.StatusGatewayTimeout, body, http.Header{"Content-Type": []string{"application/json"}}, "gateway timeout", 0)

	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("Kind = %v, want ClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("StatusCode = %d, want 504", outcome.Upstream.StatusCode)
	}
	if outcome.Kind.IsAccountFault() || outcome.Kind.ShouldFailover() {
		t.Fatalf("504 verdict must be account-neutral and non-failover: %v", outcome.Kind)
	}
	if err := forwardErrForOutcome(outcome, errors.New("upstream gateway timeout")); err != nil {
		t.Fatalf("Core-facing error = %v, want nil passthrough", err)
	}
}

func TestExtractRetryAfterHeaderFormats(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "integer seconds", value: "17", want: 17 * time.Second},
		{name: "decimal seconds fallback", value: "1.5", want: 1500 * time.Millisecond},
		{name: "textual fallback", value: "try again in 250ms", want: 250 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{"Retry-After": []string{tt.value}}
			if got := extractRetryAfterHeader(headers); got != tt.want {
				t.Fatalf("extractRetryAfterHeader(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}

	retryAt := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	got := extractRetryAfterHeader(http.Header{"Retry-After": []string{retryAt}})
	if got < 25*time.Second || got > 31*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, want roughly 30s", got)
	}
}

func TestHandleStreamResponsePostOutputOverloadRemainsTransient(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"visible output"}`,
		"",
		`data: {"type":"response.failed","response":{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("handleStreamResponse error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", outcome.Kind)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
	if !strings.Contains(w.Body.String(), "visible output") {
		t.Fatalf("business output was not forwarded: %q", w.Body.String())
	}
}

func TestHandleStreamResponsePostOutputFailurePreservesResponsesUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_usage","model":"gpt-5.6-sol"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"visible output"}`,
		"",
		`data: {"type":"response.failed","response":{"id":"resp_usage","model":"gpt-5.6-sol","usage":{"input_tokens":12,"output_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":3}},"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("handleStreamResponse error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient || outcome.RetryAfter != 5*time.Second {
		t.Fatalf("outcome = kind:%v retry:%v, want UpstreamTransient/5s", outcome.Kind, outcome.RetryAfter)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage = nil, want response.failed.response.usage")
	}
	if got := usageMetricInt(outcome.Usage, usageMetricInputTokens); got != 10 {
		t.Fatalf("input tokens = %d, want 10 uncached", got)
	}
	if got := usageMetricInt(outcome.Usage, usageMetricCachedInputTokens); got != 2 {
		t.Fatalf("cached input tokens = %d, want 2", got)
	}
	if got := usageMetricInt(outcome.Usage, usageMetricOutputTokens); got != 5 {
		t.Fatalf("output tokens = %d, want 5", got)
	}
	if got := usageMetricInt(outcome.Usage, usageMetricReasoningOutputTokens); got != 3 {
		t.Fatalf("reasoning tokens = %d, want 3", got)
	}
	if outcome.Usage.Model != "gpt-5.6-sol" {
		t.Fatalf("usage model = %q, want gpt-5.6-sol", outcome.Usage.Model)
	}
}

func TestHandleStreamResponsePostOutputAuthenticationFailureRemainsAccountDead(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"visible output"}`,
		"",
		`data: {"type":"response.failed","response":{"error":{"type":"authentication_error","code":"invalid_access_token","message":"The access token is invalid"}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()

	outcome, err := handleStreamResponse(resp, w, time.Now(), "")
	if err != nil {
		t.Fatalf("handleStreamResponse error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeAccountDead {
		t.Fatalf("Kind = %v, want AccountDead", outcome.Kind)
	}
	if !strings.Contains(w.Body.String(), "visible output") {
		t.Fatalf("business output was not forwarded: %q", w.Body.String())
	}
}

func TestWriteAnthropicUpstreamErrorUsesRetryAfterHeader(t *testing.T) {
	headers := http.Header{
		"Retry-After":      []string{"23"},
		"X-Upstream-Trace": []string{"trace-1"},
	}
	body := []byte(`{"error":{"message":"Rate limit reached"}}`)
	outcome, err := (&OpenAIGateway{}).writeAnthropicUpstreamError(nil, http.StatusTooManyRequests, headers, body, time.Now())
	if err == nil {
		t.Fatal("error = nil, want retryable upstream error")
	}
	if outcome.Kind != sdk.OutcomeAccountRateLimited {
		t.Fatalf("Kind = %v, want AccountRateLimited", outcome.Kind)
	}
	if outcome.RetryAfter != 23*time.Second {
		t.Fatalf("RetryAfter = %v, want 23s", outcome.RetryAfter)
	}
	if got := outcome.Upstream.Headers.Get("Retry-After"); got != "23" {
		t.Fatalf("upstream Retry-After = %q, want 23", got)
	}
	if got := outcome.Upstream.Headers.Get("X-Upstream-Trace"); got != "" {
		t.Fatalf("unsafe upstream trace header leaked into outcome: %q", got)
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := headers.Get("Content-Type"); got != "" {
		t.Fatalf("input headers were mutated: Content-Type = %q", got)
	}
}

func TestOAuthWSPreludeOverloadDoesNotCommitClientResponse(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_overload","model":"gpt-5.6-sol"}}`
	failed := `{"type":"response.failed","response":{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`

	t.Run("responses stream", func(t *testing.T) {
		conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
			return writeGatewayTestEvents(conn, created, failed)
		})
		w := httptest.NewRecorder()
		handler := &sseEventWriter{w: w, start: time.Now()}
		result := ReceiveWSResponse(context.Background(), conn, handler)
		waitGatewayTestWebSocket(t, serverResult)

		if w.Body.Len() != 0 || handler.wrote {
			t.Fatalf("pre-output failure committed client response: wrote=%v body=%q", handler.wrote, w.Body.String())
		}
		failure := classifyOAuthWSFailure(result.Err, handler.wrote)
		if failure.kind != sdk.OutcomeUpstreamTransient {
			t.Fatalf("Kind = %v, want UpstreamTransient", failure.kind)
		}
		if failure.retryAfter != 5*time.Second {
			t.Fatalf("RetryAfter = %v, want 5s", failure.retryAfter)
		}
		if err := forwardErrorForOAuthWSFailure(result.Err, handler.wrote, failure.kind, nil); err == nil {
			t.Fatal("pre-output overload lost Core-facing error needed for failover")
		}
	})

	t.Run("chat completions stream", func(t *testing.T) {
		conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
			return writeGatewayTestEvents(conn, created, failed)
		})
		w := httptest.NewRecorder()
		handler := newChatCompletionsStreamWriter(w, "gpt-5.6-sol", 0, "", false, time.Now())
		result := ReceiveWSResponse(context.Background(), conn, handler)
		waitGatewayTestWebSocket(t, serverResult)

		if w.Body.Len() != 0 || handler.wrote {
			t.Fatalf("pre-output failure committed chat stream: wrote=%v body=%q", handler.wrote, w.Body.String())
		}
		failure := classifyOAuthWSFailure(result.Err, handler.wrote)
		if failure.kind != sdk.OutcomeUpstreamTransient || failure.retryAfter != 5*time.Second {
			t.Fatalf("failure = %+v, want UpstreamTransient with 5s retry", failure)
		}
	})
}

func TestOAuthWSClientErrorsReturnNilGoErrorWithoutCommitting(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{
			name:  "context length",
			event: `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}}`,
		},
		{
			name:  "unsupported model",
			event: `{"type":"error","error":{"type":"invalid_request_error","code":"model_not_supported","message":"The requested model is not supported."}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
				return writeGatewayTestEvents(conn, tt.event)
			})
			w := httptest.NewRecorder()
			handler := &sseEventWriter{w: w, start: time.Now()}
			result := ReceiveWSResponse(context.Background(), conn, handler)
			waitGatewayTestWebSocket(t, serverResult)

			if result.Err == nil {
				t.Fatal("result.Err = nil, want classified client failure")
			}
			if handler.wrote || w.Body.Len() != 0 {
				t.Fatalf("client error committed stream: wrote=%v body=%q", handler.wrote, w.Body.String())
			}
			failure := classifyOAuthWSFailure(result.Err, handler.wrote)
			if failure.kind != sdk.OutcomeClientError || failure.statusCode != http.StatusBadRequest {
				t.Fatalf("failure = %+v, want ClientError/400", failure)
			}
			if err := forwardErrorForOAuthWSFailure(result.Err, false, failure.kind, nil); err != nil {
				t.Fatalf("Core-facing error = %v, want nil so original 400 is returned without account retry", err)
			}
		})
	}
}

func TestOAuthWSPostOutputOverloadRemainsTransient(t *testing.T) {
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		return writeGatewayTestEvents(conn,
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}`,
			`{"type":"response.output_text.delta","delta":"visible output"}`,
			`{"type":"response.failed","response":{"usage":{"input_tokens":12,"output_tokens":3},"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
		)
	})
	w := httptest.NewRecorder()
	handler := &sseEventWriter{w: w, start: time.Now()}
	result := ReceiveWSResponse(context.Background(), conn, handler)
	waitGatewayTestWebSocket(t, serverResult)

	if !handler.wrote || !strings.Contains(w.Body.String(), "visible output") {
		t.Fatalf("business output was not committed: wrote=%v body=%q", handler.wrote, w.Body.String())
	}
	failure := classifyOAuthWSFailure(result.Err, handler.wrote)
	if failure.kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("Kind = %v, want UpstreamTransient", failure.kind)
	}
	if failure.retryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", failure.retryAfter)
	}
	if result.InputTokens != 12 || result.OutputTokens != 3 {
		t.Fatalf("usage = input:%d output:%d, want 12/3", result.InputTokens, result.OutputTokens)
	}
	usage := newTokenUsage("gpt-5.6-sol", "", result.InputTokens, result.OutputTokens, 0, 0, 0)
	if err := forwardErrorForOAuthWSFailure(result.Err, handler.wrote, failure.kind, usage); err != nil {
		t.Fatalf("post-output overload Core-facing error = %v, want nil to preserve Usage", err)
	}
}

func TestOAuthWSPostOutputAuthenticationFailureRemainsAccountDead(t *testing.T) {
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		return writeGatewayTestEvents(conn,
			`{"type":"response.created","response":{"id":"resp_auth","model":"gpt-5.6-sol"}}`,
			`{"type":"response.output_text.delta","delta":"visible output"}`,
			`{"type":"error","error":{"type":"authentication_error","code":"account_deactivated","message":"This account has been deactivated"}}`,
		)
	})
	w := httptest.NewRecorder()
	handler := &sseEventWriter{w: w, start: time.Now()}
	result := ReceiveWSResponse(context.Background(), conn, handler)
	waitGatewayTestWebSocket(t, serverResult)

	if result.Err == nil || !handler.wrote {
		t.Fatalf("result.Err=%v wrote=%v, want committed auth failure", result.Err, handler.wrote)
	}
	failure := classifyOAuthWSFailure(result.Err, handler.wrote)
	if failure.kind != sdk.OutcomeAccountDead {
		t.Fatalf("Kind = %v, want AccountDead", failure.kind)
	}
	if failure.statusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", failure.statusCode)
	}
	if err := forwardErrorForOAuthWSFailure(result.Err, handler.wrote, failure.kind, nil); err != nil {
		t.Fatalf("post-output account failure Core-facing error = %v, want nil", err)
	}
}

func TestWSResultHasBillableUsageIncludesImageTool(t *testing.T) {
	tests := []struct {
		name      string
		result    WSResult
		numImages int
	}{
		{name: "image tool input", result: WSResult{ToolImageInputTokens: 1}},
		{name: "image tool output", result: WSResult{ToolImageOutputTokens: 1}},
		{name: "delivered image", numImages: 1},
		{name: "reasoning only", result: WSResult{ReasoningOutputTokens: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !wsResultHasBillableUsage(tt.result, tt.numImages) {
				t.Fatal("billable partial usage was ignored")
			}
		})
	}
	if wsResultHasBillableUsage(WSResult{}, 0) {
		t.Fatal("empty result reported billable usage")
	}
}

func TestOAuthWSPostOutputDisconnectIsStreamAborted(t *testing.T) {
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		return writeGatewayTestEvents(conn,
			`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}`,
			`{"type":"response.output_text.delta","delta":"partial"}`,
		)
	})
	w := httptest.NewRecorder()
	handler := &sseEventWriter{w: w, start: time.Now()}
	result := ReceiveWSResponse(context.Background(), conn, handler)
	waitGatewayTestWebSocket(t, serverResult)

	if result.Err == nil || !handler.wrote {
		t.Fatalf("result.Err=%v wrote=%v, want post-output disconnect", result.Err, handler.wrote)
	}
	if failure := classifyOAuthWSFailure(result.Err, handler.wrote); failure.kind != sdk.OutcomeStreamAborted {
		t.Fatalf("Kind = %v, want StreamAborted", failure.kind)
	}
}

func TestReceiveWSResponseReturnsDownstreamWriteErrorAndStopsReading(t *testing.T) {
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		return writeGatewayTestEvents(conn,
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_text.delta","delta":"first"}`,
			`{"type":"response.output_text.delta","delta":"must not be read"}`,
		)
	})
	writer := &sseEventWriter{w: newFailingResponseWriter(1), start: time.Now()}
	handler := &countingWSEventHandler{delegate: writer}
	start := time.Now()
	result := ReceiveWSResponse(context.Background(), conn, handler)
	waitGatewayTestWebSocket(t, serverResult)

	var downstreamErr *downstreamWriteError
	if !errors.As(result.Err, &downstreamErr) {
		t.Fatalf("Err = %v, want downstreamWriteError", result.Err)
	}
	if handler.rawCalls != 2 {
		t.Fatalf("raw callbacks = %d, want 2; upstream reading continued after write failure", handler.rawCalls)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("downstream write failure returned too slowly: %v", time.Since(start))
	}
	if failure := classifyOAuthWSFailure(result.Err, writer.wrote); failure.kind != sdk.OutcomeStreamAborted {
		t.Fatalf("Kind = %v, want StreamAborted", failure.kind)
	}
	if err := forwardErrorForOAuthWSFailure(result.Err, writer.wrote, sdk.OutcomeStreamAborted, nil); err != nil {
		t.Fatalf("Core-facing error = %v, want nil to prevent failover", err)
	}
}

func TestHandleStreamResponseDownstreamWriteFailureIsNeutralAndClosesUpstream(t *testing.T) {
	writers := []struct {
		name string
		w    http.ResponseWriter
	}{
		{name: "zero byte", w: newFailingResponseWriter(1)},
		{name: "partial", w: newPartialFailingResponseWriter(7)},
	}

	for _, tt := range writers {
		t.Run(tt.name, func(t *testing.T) {
			body := &trackingReadCloser{Reader: strings.NewReader(strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol"}}`,
				"",
				`data: {"type":"response.output_text.delta","delta":"visible"}`,
				"",
				`data: {"type":"response.output_text.delta","delta":"must not be forwarded"}`,
				"",
			}, "\n"))}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       body,
			}

			outcome, err := handleStreamResponse(resp, tt.w, time.Now(), "")
			if err != nil {
				t.Fatalf("Core-facing error = %v, want nil", err)
			}
			if outcome.Kind != sdk.OutcomeStreamAborted {
				t.Fatalf("Kind = %v, want StreamAborted", outcome.Kind)
			}
			if !body.closed {
				t.Fatal("upstream body was not closed after downstream write failure")
			}
			if !strings.Contains(outcome.Reason, errTestDownstreamWrite.Error()) {
				t.Fatalf("Reason = %q, want downstream write error", outcome.Reason)
			}
		})
	}
}

func TestHandleStreamResponseKeepAliveFailureCancelsBlockedUpstream(t *testing.T) {
	body := newBlockingReadCloser()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	type result struct {
		outcome sdk.ForwardOutcome
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		outcome, err := handleStreamResponseWithKeepAlive(nil, resp, newFailingResponseWriter(1), time.Now(), "", 5*time.Millisecond)
		resultCh <- result{outcome: outcome, err: err}
	}()

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Core-facing error = %v, want nil", got.err)
		}
		if got.outcome.Kind != sdk.OutcomeStreamAborted {
			t.Fatalf("Kind = %v, want StreamAborted", got.outcome.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("keepalive write failure did not stop the blocked upstream read")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("blocked upstream body was not closed")
	}
}

func TestParseSSEStreamStopsOnDownstreamWriteError(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"first"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"must not be read"}`,
		"",
	}, "\n")
	writer := &sseEventWriter{w: newFailingResponseWriter(1), start: time.Now()}
	handler := &countingWSEventHandler{delegate: writer}
	result := ParseSSEStream(strings.NewReader(sse), handler)

	var downstreamErr *downstreamWriteError
	if !errors.As(result.Err, &downstreamErr) {
		t.Fatalf("Err = %v, want downstreamWriteError", result.Err)
	}
	if handler.rawCalls != 2 {
		t.Fatalf("raw callbacks = %d, want 2", handler.rawCalls)
	}
}

func TestChatCompletionsFinalizePropagatesDoneWriteError(t *testing.T) {
	w := newFailingResponseWriter(3)
	writer := newChatCompletionsStreamWriter(w, "gpt-5.6-sol", 0, "", false, time.Now())
	writer.OnRawEvent("response.created", []byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
	writer.OnRawEvent("response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"ok"}`))
	writer.OnRawEvent("response.completed", []byte(`{"type":"response.completed","response":{"status":"completed"}}`))
	if err := writer.Err(); err != nil {
		t.Fatalf("Err before final [DONE] = %v", err)
	}

	err := writer.finalize()
	var downstreamErr *downstreamWriteError
	if !errors.As(err, &downstreamErr) {
		t.Fatalf("finalize error = %v, want downstreamWriteError", err)
	}
}

func TestReceiveWSResponseCancellationInterruptsBlockedRead(t *testing.T) {
	serverReady := make(chan struct{})
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		close(serverReady)
		_, _, err := conn.ReadMessage()
		if err == nil {
			return errors.New("server read unexpectedly succeeded")
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan WSResult, 1)
	go func() {
		resultCh <- ReceiveWSResponse(ctx, conn, nil)
	}()
	<-serverReady
	// Let ReceiveWSResponse enter ReadMessage so cancellation exercises the
	// watcher path instead of the pre-read context fast path.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case result := <-resultCh:
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("Err = %v, want context.Canceled", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled context did not interrupt ReadMessage")
	}
	waitGatewayTestWebSocket(t, serverResult)
}

func TestReceiveWSResponseJoinsContextWatcherOnSuccess(t *testing.T) {
	conn, serverResult := dialGatewayTestWebSocket(t, func(conn *websocket.Conn) error {
		if err := writeGatewayTestEvents(conn, `{"type":"response.completed","response":{"status":"completed"}}`); err != nil {
			return err
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("connection closed after successful receive: %w", err)
		}
		if string(msg) != "watcher-stopped" {
			return fmt.Errorf("unexpected follow-up message %q", msg)
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := ReceiveWSResponse(ctx, conn, nil)
	if result.Err != nil {
		t.Fatalf("ReceiveWSResponse error: %v", result.Err)
	}

	// If the watcher were still alive, cancel would close conn and this write
	// would fail. A successful write proves the normal-return path joined it.
	cancel()
	time.Sleep(20 * time.Millisecond)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("watcher-stopped")); err != nil {
		t.Fatalf("connection was closed by leaked context watcher: %v", err)
	}
	waitGatewayTestWebSocket(t, serverResult)
}
