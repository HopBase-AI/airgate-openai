package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func testPNGDataURL(width, height int, pixel func(int, int) color.RGBA) string {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, pixel(x, y))
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testPNGBase64(width, height int, pixel func(int, int) color.RGBA) string {
	dataURL := testPNGDataURL(width, height, pixel)
	return strings.TrimPrefix(dataURL, "data:image/png;base64,")
}

func testPNGBytes(width, height int, pixel func(int, int) color.RGBA) []byte {
	b64 := testPNGBase64(width, height, pixel)
	data, _ := base64.StdEncoding.DecodeString(b64)
	return data
}

func testPNGDataURLFromImage(img image.Image) string {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestIsImagesRequest(t *testing.T) {
	cases := []struct {
		path     string
		want     bool
		wantEdit bool
	}{
		{"/v1/images/generations", true, false},
		{"/images/generations", true, false},
		{"/v1/responses", false, false},
		{"/v1/chat/completions", false, false},
		{"/v1/images/edits", true, true},
		{"/images/edits", true, true},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := isImagesRequest(tc.path); got != tc.want {
			t.Errorf("isImagesRequest(%q) = %v, want %v", tc.path, got, tc.want)
		}
		if got := isImagesEditRequest(tc.path); got != tc.wantEdit {
			t.Errorf("isImagesEditRequest(%q) = %v, want %v", tc.path, got, tc.wantEdit)
		}
	}
}

func TestPrefersAsyncResponse(t *testing.T) {
	if prefersAsyncResponse(http.Header{"Content-Type": []string{"application/json"}}) {
		t.Fatal("plain Images API request should keep the standard synchronous response")
	}

	for _, headers := range []http.Header{
		{"Prefer": []string{"respond-async"}},
		{"Prefer": []string{"wait=60, respond-async"}},
		{"Prefer": []string{"respond-async; handling=lenient"}},
	} {
		if !prefersAsyncResponse(headers) {
			t.Fatalf("Prefer header should opt into async response: %+v", headers)
		}
	}
}

func TestImageTaskLocation(t *testing.T) {
	const taskID = "018f2f8a-9f8a-7c11-9f2a-8a9f8a7c119f"
	if got := imageTaskLocation("/v1/images/generations", taskID); got != "/v1/images/tasks?task_id="+taskID {
		t.Fatalf("v1 location = %q", got)
	}
	if got := imageTaskLocation("/images/generations", taskID); got != "/images/tasks?task_id="+taskID {
		t.Fatalf("non-v1 location = %q", got)
	}
}

// TestShouldUseImagesWebReverse pins WebReverse off across every previously
// matching shape. WebReverse is deprecated — kept compiled, but never picked.
// If someone re-enables it, this test will flag every legacy-positive case so
// the rollout is deliberate.
func TestShouldUseImagesWebReverse(t *testing.T) {
	cases := []struct {
		name    string
		account *sdk.Account
		model   string
	}{
		{"free oauth with gpt-image-2 (legacy positive)", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "free",
		}}, "gpt-image-2"},
		{"plus oauth with gpt-image-2", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "plus",
		}}, "gpt-image-2"},
		{"oauth without plan type", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
		}}, "gpt-image-2"},
		{"free oauth with other model", &sdk.Account{Credentials: map[string]string{
			"access_token": "token",
			"plan_type":    "free",
		}}, "gpt-image-1"},
		{"apikey account", &sdk.Account{Credentials: map[string]string{
			"api_key": "sk-test",
		}}, "gpt-image-2"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUseImagesWebReverse(tc.account, tc.model); got {
				t.Fatalf("shouldUseImagesWebReverse() = true; WebReverse is deprecated, must be false")
			}
		})
	}
}

func TestApplyWebReverseSizeHint(t *testing.T) {
	const prompt = "draw a cat"
	cases := []struct {
		name string
		size string
		want string
	}{
		{
			name: "frontend landscape size",
			size: "2048x1360",
			want: "Generate a landscape image at 2048x1360 resolution. draw a cat",
		},
		{
			name: "portrait size",
			size: "1024x1536",
			want: "Generate a portrait image at 1024x1536 resolution. draw a cat",
		},
		{
			name: "square size",
			size: "1024x1024",
			want: "Generate a square image at 1024x1024 resolution. draw a cat",
		},
		{
			name: "spaced uppercase separator",
			size: " 2048 X 1360 ",
			want: "Generate a landscape image at 2048x1360 resolution. draw a cat",
		},
		{
			name: "oversized landscape is clamped",
			size: "4096x2304",
			want: "Generate a landscape image at 3840x2160 resolution. draw a cat",
		},
		{
			name: "oversized portrait is clamped",
			size: "2304x4096",
			want: "Generate a portrait image at 2160x3840 resolution. draw a cat",
		},
		{
			name: "ratio is ignored",
			size: "16:9",
			want: prompt,
		},
		{
			name: "invalid size is ignored",
			size: "0x1024",
			want: prompt,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyWebReverseSizeHint(prompt, tc.size); got != tc.want {
				t.Fatalf("applyWebReverseSizeHint() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHandleImagesResponse_TokenAttribution 覆盖官方响应格式：
//   - usage.input_tokens / output_tokens 落入 Outcome.Usage
//   - cached tokens 从 input 中扣减，避免重复计费
//   - API Key Images 的图像输出 token 单独归入 image cost
func TestHandleImagesResponse_TokenAttribution(t *testing.T) {
	body := `{
		"created": 1713833628,
		"data": [{"b64_json": "iVBORw0..."}],
		"usage": {
			"total_tokens": 4210,
			"input_tokens": 50,
			"output_tokens": 4160,
			"input_tokens_details": {"text_tokens": 50, "cached_tokens": 10}
		}
	}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	w := httptest.NewRecorder()

	outcome, err := handleImagesResponse(resp, w, nil, time.Now(), "gpt-image-1.5", "2048x2048")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want Success", outcome.Kind)
	}
	u := outcome.Usage
	if u == nil {
		t.Fatal("Usage = nil, want non-nil")
	}
	if u.Model != "gpt-image-1.5" {
		t.Errorf("Model = %q, want gpt-image-1.5", u.Model)
	}
	if got := usageMetricInt(u, usageMetricInputTokens); got != 40 {
		t.Errorf("input_tokens = %d, want 40 (50 - 10 cached)", got)
	}
	if got := usageMetricInt(u, usageMetricOutputTokens); got != 4160 {
		t.Errorf("output_tokens = %d, want 4160", got)
	}
	if got := usageMetricInt(u, usageMetricCachedInputTokens); got != 10 {
		t.Errorf("cached_input_tokens = %d, want 10", got)
	}

	if got := usageCostByKey(u, usageCostInput); !almostEqual(got, tokenCost(40, 5), 1e-9) {
		t.Errorf("input cost = %v, want %v", got, tokenCost(40, 5))
	}
	wantCost := tokenCost(40, 5) + tokenCost(10, 0.5) + tokenCost(4160, 30)
	if !almostEqual(u.AccountCost, wantCost, 1e-9) {
		t.Errorf("AccountCost = %v, want %v", u.AccountCost, wantCost)
	}

	if w.Code != http.StatusOK {
		t.Errorf("writer status = %d, want 200", w.Code)
	}
	gotBody, _ := io.ReadAll(w.Result().Body)
	if len(gotBody) != len(body) {
		t.Errorf("response body len = %d, want %d", len(gotBody), len(body))
	}
}

func TestWriteSSEPingUsesCoreRecognizedComment(t *testing.T) {
	w := httptest.NewRecorder()
	writeSSEPing(w)

	if got, want := w.Body.String(), responseStreamKeepAliveComment; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestImageKeepAliveEmptyOverloadRemainsFailoverEligible(t *testing.T) {
	w := newSignalingResponseWriter()
	ka := startSSEPingKeepAliveWithInterval(w, 5*time.Millisecond)
	waitForHeartbeat(t, w)
	ka.Stop()

	outcome := failureOutcome(
		http.StatusServiceUnavailable,
		nil,
		http.Header{"Retry-After": []string{"5"}},
		"server_is_overloaded",
		5*time.Second,
	)
	if outcome.Kind != sdk.OutcomeUpstreamTransient || !outcome.Kind.ShouldFailover() {
		t.Fatalf("Kind = %v, want failover-eligible UpstreamTransient", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusServiceUnavailable || len(outcome.Upstream.Body) != 0 {
		t.Fatalf("Upstream = status:%d body:%q, want empty 503", outcome.Upstream.StatusCode, outcome.Upstream.Body)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
	if err := forwardErrForOutcome(outcome, errors.New("upstream overloaded")); err == nil {
		t.Fatal("empty 503 lost Core-facing error needed for failover")
	}
	if body := w.BodyString(); strings.ReplaceAll(body, responseStreamKeepAliveComment, "") != "" {
		t.Fatalf("heartbeat committed business output: %q", body)
	}
}

func TestStartSSEPingKeepAliveDoesNotCommitImmediately(t *testing.T) {
	w := httptest.NewRecorder()
	sseKA := startSSEPingKeepAlive(w)
	sseKA.Stop()

	if sseKA.Wrote() {
		t.Fatalf("首个 ping 前不应标记流已写入")
	}
	if body := w.Body.String(); body != "" {
		t.Fatalf("body = %q，首个 ping 前应为空", body)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
}

func TestImageKeepAliveWriteFailureSignalsCancellation(t *testing.T) {
	canceled := make(chan error, 1)
	ka := startSSEPingKeepAliveWithInterval(newFailingResponseWriter(1), 5*time.Millisecond, func(err error) {
		canceled <- err
	})
	defer ka.Stop()

	select {
	case err := <-canceled:
		var downstreamErr *downstreamWriteError
		if !errors.As(err, &downstreamErr) {
			t.Fatalf("callback error = %v, want downstreamWriteError", err)
		}
	case <-time.After(time.Second):
		t.Fatal("image keepalive write failure did not signal cancellation")
	}
	var downstreamErr *downstreamWriteError
	if !errors.As(ka.Err(), &downstreamErr) {
		t.Fatalf("keepalive Err = %v, want downstreamWriteError", ka.Err())
	}
}

func TestHandleImagesResponseStreamWriteFailureIsNeutralAndKeepsUsage(t *testing.T) {
	body := `{"created":1713833628,"model":"gpt-image-1","data":[{"b64_json":"iVBORw0"}],"usage":{"input_tokens":4,"output_tokens":2}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	w := newFailingResponseWriter(1)
	sseKA := startSSEPingKeepAlive(w)

	outcome, err := handleImagesResponse(resp, w, sseKA, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("Core-facing error = %v, want nil", err)
	}
	if outcome.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("Kind = %v, want StreamAborted", outcome.Kind)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage = nil, want completed upstream image usage")
	}
	if got := usageMetricInt(outcome.Usage, usageMetricInputTokens); got != 4 {
		t.Fatalf("input tokens = %d, want 4", got)
	}
}

func TestHandleImagesResponse_StreamWrapsRESTJSONAsSSE(t *testing.T) {
	body := `{"created":1713833628,"data":[{"b64_json":"iVBORw0"}],"usage":{"input_tokens":1,"output_tokens":2}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	w := httptest.NewRecorder()
	sseKA := startSSEPingKeepAlive(w)

	outcome, err := handleImagesResponse(resp, w, sseKA, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want Success", outcome.Kind)
	}
	if outcome.Upstream.Body != nil {
		t.Fatalf("Upstream.Body = %q, want nil for streamed response", outcome.Upstream.Body)
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("writer status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("writer Content-Type = %q, want text/event-stream", got)
	}
	gotBody := w.Body.String()
	wantBody := "data: " + body + "\n\ndata: [DONE]\n\n"
	if gotBody != wantBody {
		t.Fatalf("writer body = %q, want %q", gotBody, wantBody)
	}
}

func TestHandleImagesResponse_APIKeyBillingUsesRequestSize(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"},{"url":"https://example/b.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1.5", "3840x2160")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	wantCost := tokenCost(10, 5) + tokenCost(100, 30)
	if got := outcome.Usage.AccountCost; !almostEqual(got, wantCost, 1e-9) {
		t.Fatalf("AccountCost = %v, want %v", got, wantCost)
	}
	if got, want := usageImageUnitPrice(outcome.Usage), "30"; got != want {
		t.Fatalf("image unit_price = %q, want %q", got, want)
	}
	if got, want := usageCostMetadata(outcome.Usage, usageCostImage, "unit"), "USD/1M tokens"; got != want {
		t.Fatalf("image unit = %q, want %q", got, want)
	}
	if got, want := usageCostMetadata(outcome.Usage, usageCostImage, "billing_model"), "gpt-5.5"; got != want {
		t.Fatalf("image billing_model = %q, want %q", got, want)
	}
	if got, want := usageCostMetadata(outcome.Usage, usageCostImage, "image_tier"), "4k"; got != want {
		t.Fatalf("image_tier = %q, want %q", got, want)
	}
}

func TestHandleImagesResponse_NonStreamReturnsBodyWithoutWriter(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if len(outcome.Upstream.Body) != len(body) {
		t.Fatalf("Upstream.Body len = %d, want %d", len(outcome.Upstream.Body), len(body))
	}
	if got := outcome.Upstream.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

// TestHandleImagesResponse_FallbackModelWhenBodyLacksModel 验证 Images 响应里
// 没有 model 字段时，会回退到请求侧传入的 fallbackModel，避免 fillUsageCost 查不到定价。
func TestHandleImagesResponse_FallbackModelWhenBodyLacksModel(t *testing.T) {
	body := `{"data":[{"url":"https://example/a.png"}],"usage":{"input_tokens":10,"output_tokens":100}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}

	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "gpt-image-1")
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-1" {
		t.Fatalf("Usage.Model = %q, want gpt-image-1 (fallback)", outcome.Usage.Model)
	}
	// Writer 为 nil 时 Upstream.Body/Headers 应带回给 core
	if len(outcome.Upstream.Body) != len(body) {
		t.Errorf("Upstream.Body len = %d, want %d", len(outcome.Upstream.Body), len(body))
	}
	if outcome.Upstream.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("Upstream.Headers Content-Type not preserved")
	}
	if outcome.Usage.AccountCost <= 0 {
		t.Errorf("AccountCost = %v, want > 0", outcome.Usage.AccountCost)
	}
}

// TestFillUsageCostPerImageBySize_1K 按默认 gpt-5.5 输出价格估算图像 token 成本。
func TestFillUsageCostPerImageBySize_1K(t *testing.T) {
	usage := &sdk.Usage{Model: "gpt-image-1"}
	fillUsageCostPerImageBySize(usage, 3, "1024x1024", "")
	want := tokenCost(3*lookupImageGenOutputTokens("1024x1024", ""), 30)
	if !almostEqual(usage.AccountCost, want, 1e-9) {
		t.Errorf("AccountCost = %v, want %v", usage.AccountCost, want)
	}
}

// TestFillUsageCostPerImageBySize_GeminiUsesDedicatedPrice 复现生产曾出现的
// Azure Gemini 生图错扣：198 input + 1571 image output 不应回退到 GPT-5.5 价。
func TestFillUsageCostPerImageBySize_GeminiUsesDedicatedPrice(t *testing.T) {
	const modelID = "gemini-3.1-flash-image-preview"
	usage := newTokenUsage(modelID, "", 198, 1571, 0, 0, 0)
	fillUsageCostPerImageBySize(usage, 1, "1264x848", "")

	want := tokenCost(198, 0.5) + tokenCost(1571, 3)
	if !almostEqual(usage.AccountCost, want, 1e-12) {
		t.Fatalf("AccountCost = %v, want %v", usage.AccountCost, want)
	}
	if got, wantModel := usageCostMetadata(usage, usageCostImage, "billing_model"), modelID; got != wantModel {
		t.Fatalf("image billing_model = %q, want %q", got, wantModel)
	}
	if got, wantPrice := usageImageUnitPrice(usage), "3"; got != wantPrice {
		t.Fatalf("image unit_price = %q, want %q", got, wantPrice)
	}
}

// TestImageActualSizeFromBase64 验证从 base64 PNG header 解码出真实宽高。
// auto 计费场景关键 fallback：上游若不返 size 字段，靠这个保证按真图计费。
func TestImageActualSizeFromBase64(t *testing.T) {
	// 生成 1536×1024 的真 PNG，编 base64
	b64 := testPNGBase64(1536, 1024, func(x, y int) color.RGBA {
		return color.RGBA{R: 100, G: 100, B: 100, A: 255}
	})
	got, ok := imageActualSizeFromBase64(b64)
	if !ok {
		t.Fatal("imageActualSizeFromBase64 = ok=false, want true")
	}
	if got != "1536x1024" {
		t.Errorf("size = %q, want 1536x1024", got)
	}
	// 异常 base64 应返回 false
	if _, ok := imageActualSizeFromBase64("not-valid-base64!!!"); ok {
		t.Error("invalid base64 should return ok=false")
	}
	// 空字符串
	if _, ok := imageActualSizeFromBase64(""); ok {
		t.Error("empty string should return ok=false")
	}
}

// TestImageTierForSize 覆盖 Core 固定图价需要的 1K/2K/4K 分档元数据。
func TestImageTierForSize(t *testing.T) {
	cases := []struct {
		size string
		want string
	}{
		{"1024x1024", "1k"},
		{"1536x1024", "1k"},
		{"1024x1536", "1k"},
		{"2048x2048", "2k"},
		{"2048x1152", "2k"},
		{"1152x2048", "2k"},
		{"3840x2160", "4k"},
		{"2160x3840", "4k"},
		{"", "1k"},
		{"auto", "1k"},
		{"garbage", "1k"},
	}
	for _, tc := range cases {
		if got := imageTierForSize(tc.size); got != tc.want {
			t.Errorf("imageTierForSize(%q) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

func TestImageUpstreamModelIDForAccount(t *testing.T) {
	tests := []struct {
		name          string
		baseURL       string
		upstreamModel string
		model         string
		want          string
	}{
		{name: "yhshu public alias", baseURL: "https://www.yhshu.ai", model: "gpt-image-2", want: yhshuGPTImage2UpstreamModel},
		{name: "yhshu subdomain and path", baseURL: "https://api.yhshu.ai/v1", model: "gpt-image-2", want: yhshuGPTImage2UpstreamModel},
		{name: "lookalike host keeps public alias", baseURL: "https://yhshu.ai.example.com", model: "gpt-image-2", want: "gpt-image-2"},
		{name: "explicit bare public ID", baseURL: "https://relay.example.com/v1", upstreamModel: "gpt-image-2", model: "gpt-image-2", want: "gpt-image-2"},
		{name: "explicit custom ID collapses legacy tier", baseURL: "https://relay.example.com/v1", upstreamModel: "vendor-image-v2", model: "gpt-image-2-4k", want: "vendor-image-v2"},
		{name: "other upstream keeps public alias", baseURL: "https://relay.example.com", model: "gpt-image-2", want: "gpt-image-2"},
		{name: "other yhshu model unchanged", baseURL: "https://www.yhshu.ai", model: "gemini-3-pro-image", want: "gemini-3-pro-image"},
		{name: "codex variant unchanged", baseURL: "https://www.yhshu.ai", model: "gpt-image-2-codex", want: "gpt-image-2-codex"},
		{name: "yhshu legacy tier alias", baseURL: "https://www.yhshu.ai", model: "gpt-image-2-4k", want: yhshuGPTImage2UpstreamModel},
		{name: "other upstream legacy tier unchanged", baseURL: "https://relay.example.com", model: "gpt-image-2-4k", want: "gpt-image-2-4k"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &sdk.Account{Credentials: map[string]string{
				"base_url":                       tt.baseURL,
				gptImage2UpstreamModelCredential: tt.upstreamModel,
			}}
			if got := imageUpstreamModelIDForAccount(account, tt.model); got != tt.want {
				t.Fatalf("imageUpstreamModelIDForAccount(%q, %q) = %q, want %q", tt.baseURL, tt.model, got, tt.want)
			}
		})
	}
}

func TestNormalizeImagesResponseModelAliasesPreservesRequestedPublicID(t *testing.T) {
	for _, fallbackModel := range []string{"gpt-image-2", "gpt-image-2-1k", "gpt-image-2-2k", "gpt-image-2-4k"} {
		t.Run(fallbackModel, func(t *testing.T) {
			for _, upstreamModel := range []string{yhshuGPTImage2UpstreamModel, "gpt-image-2"} {
				body := []byte(fmt.Sprintf(`{"model":%q,"data":[{"model":%q,"b64_json":"AA=="}]}`, upstreamModel, upstreamModel))
				normalized := normalizeImagesResponseModelAliases(body, fallbackModel)
				if got := gjson.GetBytes(normalized, "model").String(); got != fallbackModel {
					t.Fatalf("upstream=%q root model = %q, want %q; body=%s", upstreamModel, got, fallbackModel, normalized)
				}
				if got := gjson.GetBytes(normalized, "data.0.model").String(); got != fallbackModel {
					t.Fatalf("upstream=%q item model = %q, want %q; body=%s", upstreamModel, got, fallbackModel, normalized)
				}
			}
		})
	}
}

func TestRewriteImagesRequestModelMultipartPreservesPayload(t *testing.T) {
	var original bytes.Buffer
	writer := multipart.NewWriter(&original)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "relight the product"); err != nil {
		t.Fatal(err)
	}
	imageBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x01, 0x02}
	imageHeader := make(textproto.MIMEHeader)
	imageHeader.Set("Content-Disposition", `form-data; name="image"; filename="input.png"`)
	imageHeader.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(imageHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(imageBytes); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	contentType := writer.FormDataContentType()
	patched, patchedContentType, err := rewriteImagesRequestModel(
		original.Bytes(),
		contentType,
		"gpt-image-2-2k",
	)
	if err != nil {
		t.Fatalf("rewriteImagesRequestModel: %v", err)
	}
	if patchedContentType != contentType {
		t.Fatalf("content type = %q, want %q", patchedContentType, contentType)
	}

	parsed, err := parseImagesRequest(patched, patchedContentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest: %v", err)
	}
	if parsed.Model != "gpt-image-2-2k" || parsed.Prompt != "relight the product" {
		t.Fatalf("rewritten fields = %+v", parsed)
	}
	if len(parsed.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(parsed.Images))
	}
	_, gotImageBytes, err := decodeDataImageURL(parsed.Images[0])
	if err != nil {
		t.Fatalf("decodeDataImageURL: %v", err)
	}
	if !bytes.Equal(gotImageBytes, imageBytes) {
		t.Fatalf("image bytes changed: got %x want %x", gotImageBytes, imageBytes)
	}
}

// TestFillUsageCostPerImageBySize 验证图像输出 token 归入 image cost。
func TestFillUsageCostPerImageBySize(t *testing.T) {
	cases := []struct {
		name      string
		size      string
		numImages int
	}{
		{"1K single", "1024x1024", 1},
		{"1K triple", "1536x1024", 3},
		{"2K single", "2048x2048", 1},
		{"4K double", "3840x2160", 2},
		{"auto fallback to 1K", "auto", 4},
		{"zero images skipped", "1024x1024", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := &sdk.Usage{Model: "gpt-image-2"}
			fillUsageCostPerImageBySize(usage, tc.numImages, tc.size, "")
			want := tokenCost(tc.numImages*lookupImageGenOutputTokens(tc.size, ""), 30)
			if !almostEqual(usage.AccountCost, want, 1e-9) {
				t.Errorf("AccountCost = %v, want %v", usage.AccountCost, want)
			}
			if !almostEqual(usageCostByKey(usage, usageCostInput), 0, 1e-9) {
				t.Errorf("input cost = %v, want 0", usageCostByKey(usage, usageCostInput))
			}
			if tc.numImages > 0 {
				if got, want := usageCostMetadata(usage, usageCostImage, "billing_model"), "gpt-5.5"; got != want {
					t.Errorf("billing_model = %q, want %q", got, want)
				}
			}
		})
	}
}

func usageCostByKey(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, detail := range usage.CostDetails {
		if detail.Key == key {
			return detail.AccountCost
		}
	}
	return 0
}

func usageImageUnitPrice(usage *sdk.Usage) string {
	if usage == nil {
		return ""
	}
	for _, detail := range usage.CostDetails {
		if detail.Key == usageCostImage {
			return detail.Metadata["unit_price"]
		}
	}
	return ""
}

func usageCostMetadata(usage *sdk.Usage, key, metadataKey string) string {
	if usage == nil {
		return ""
	}
	for _, detail := range usage.CostDetails {
		if detail.Key == key && detail.Metadata != nil {
			return detail.Metadata[metadataKey]
		}
	}
	return ""
}

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// TestParseUsage_ToolImageGen 验证 parseUsage 从 JSON body 中提取
// tool_usage.image_gen 的 input/output tokens。
func TestParseUsage_ToolImageGen(t *testing.T) {
	body := []byte(`{
		"usage": {"input_tokens": 100, "output_tokens": 50, "input_tokens_details": {"cached_tokens": 20}},
		"tool_usage": {"image_gen": {"input_tokens": 5, "output_tokens": 4160}}
	}`)
	got := parseUsage(body)
	if got.inputTokens != 80 { // 100 - 20 cached
		t.Errorf("inputTokens = %d, want 80", got.inputTokens)
	}
	if got.outputTokens != 50 {
		t.Errorf("outputTokens = %d, want 50", got.outputTokens)
	}
	if got.cachedInputTokens != 20 {
		t.Errorf("cachedInputTokens = %d, want 20", got.cachedInputTokens)
	}
	if got.toolImageInputTokens != 5 {
		t.Errorf("toolImageInputTokens = %d, want 5", got.toolImageInputTokens)
	}
	if got.toolImageOutputTokens != 4160 {
		t.Errorf("toolImageOutputTokens = %d, want 4160", got.toolImageOutputTokens)
	}
}

func TestParseUsage_ImageGenerationCallSummary(t *testing.T) {
	b64 := strings.TrimPrefix(testPNGDataURL(1024, 1536, func(x, y int) color.RGBA {
		return color.RGBA{R: 1, G: 2, B: 3, A: 255}
	}), "data:image/png;base64,")
	body := []byte(fmt.Sprintf(`{
		"usage": {"input_tokens": 100, "output_tokens": 50},
		"output": [{
			"type": "image_generation_call",
			"result": %q,
			"size": "1024x1536",
			"quality": "high"
		}]
	}`, b64))
	got := parseUsage(body)
	if got.imageGenCallCount != 1 {
		t.Fatalf("imageGenCallCount = %d, want 1", got.imageGenCallCount)
	}
	if got.imageGenCallSize != "1024x1536" {
		t.Fatalf("imageGenCallSize = %q, want 1024x1536", got.imageGenCallSize)
	}
}

func TestParseUsage_ImageGenerationCallSummaryFromSSEBody(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aGVsbG8=","size":"1024x1024"}}`,
		``,
		`data: {"type":"response.output_item.done","item":{"id":"ig_2","type":"image_generation_call","status":"completed","result":"d29ybGQ=","size":"1024x1024"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","usage":{"input_tokens":10,"output_tokens":20},"output":[]}}`,
		``,
	}, "\n"))
	got := parseUsage(body)
	if got.imageGenCallCount != 2 {
		t.Fatalf("imageGenCallCount = %d, want 2", got.imageGenCallCount)
	}
	if got.imageGenCallSize != "1024x1024" {
		t.Fatalf("imageGenCallSize = %q, want 1024x1024", got.imageGenCallSize)
	}
}

func TestParseUsage_ImageGenerationCallSummaryDedupsCompletedOutput(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aGVsbG8=","size":"1024x1024"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","usage":{"input_tokens":10,"output_tokens":20},"output":[{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aGVsbG8=","size":"1024x1024"},{"id":"ig_2","type":"image_generation_call","status":"completed","result":"d29ybGQ=","size":"1024x1024"}]}}`,
		``,
	}, "\n"))
	got := parseUsage(body)
	if got.imageGenCallCount != 2 {
		t.Fatalf("imageGenCallCount = %d, want 2", got.imageGenCallCount)
	}
}

func TestCollectImageGenCallSummary(t *testing.T) {
	b64 := strings.TrimPrefix(testPNGDataURL(1024, 1024, func(x, y int) color.RGBA {
		return color.RGBA{R: 5, G: 6, B: 7, A: 255}
	}), "data:image/png;base64,")
	event := []byte(fmt.Sprintf(`{
		"type":"response.output_item.done",
		"item":{
			"type":"image_generation_call",
			"result":%q,
			"size":"1024x1024"
		}
	}`, b64))
	ok, size := collectImageGenCallSummary(event)
	if !ok {
		t.Fatal("expected image generation call summary")
	}
	if size != "1024x1024" {
		t.Fatalf("size = %q, want 1024x1024", size)
	}
}

// TestParseSSEUsage_ToolImageGen 验证 SSE response.completed 事件中
// response.tool_usage.image_gen 被正确抽取到累加器指针。
func TestParseSSEUsage_ToolImageGen(t *testing.T) {
	data := []byte(`{
		"type":"response.completed",
		"response":{
			"model":"gpt-5.4",
			"usage":{"input_tokens":100,"output_tokens":50},
			"tool_usage":{"image_gen":{"input_tokens":8,"output_tokens":4160}}
		}
	}`)
	usage := &sdk.Usage{}
	var toolIn, toolOut int
	parseSSEUsage(data, usage, &toolIn, &toolOut)
	if usage.Model != "gpt-5.4" {
		t.Errorf("Model = %q, want gpt-5.4", usage.Model)
	}
	if usageMetricInt(usage, usageMetricInputTokens) != 100 || usageMetricInt(usage, usageMetricOutputTokens) != 50 {
		t.Errorf("Input/Output = %d/%d, want 100/50", usageMetricInt(usage, usageMetricInputTokens), usageMetricInt(usage, usageMetricOutputTokens))
	}
	if toolIn != 8 || toolOut != 4160 {
		t.Errorf("toolIn/Out = %d/%d, want 8/4160", toolIn, toolOut)
	}
}

// TestFillUsageCostWithImageTool 叠加计费：主 model (gpt-5.4) 的 chat token
// 和 image tool token 都按同一个对话模型价格计费。
func TestFillUsageCostWithImageTool(t *testing.T) {
	usage := newTokenUsage("gpt-5.4", "", 1000, 500, 0, 0, 0)
	fillUsageCostWithImageTool(usage, 1, "1024x1024", 0, 1056)

	// 主 gpt-5.4 standard: input=$2.5/1M → 0.0025, output=$15/1M → 0.0075
	// image tool output 1056 tokens also uses gpt-5.4 output=$15/1M → 0.01584
	// total account cost = 0.0025 + 0.0075 + 0.01584 = 0.02584
	if !almostEqual(usageCostByKey(usage, usageCostInput), 0.0025, 1e-9) {
		t.Errorf("input cost = %v, want 0.0025", usageCostByKey(usage, usageCostInput))
	}
	wantCost := 0.0025 + 0.0075 + tokenCost(1056, 15)
	if !almostEqual(usage.AccountCost, wantCost, 1e-9) {
		t.Errorf("AccountCost = %v, want %v", usage.AccountCost, wantCost)
	}
	if got := usage.Metrics[0].Metadata["unit_price"]; got != "2.5" {
		t.Errorf("input unit_price = %q, want 2.5", got)
	}
	if got, want := usageCostMetadata(usage, usageCostImageTool, "billing_model"), "gpt-5.4"; got != want {
		t.Errorf("image billing_model = %q, want %q", got, want)
	}
	if got, want := usageCostMetadata(usage, usageCostImageTool, "unit_price"), "15"; got != want {
		t.Errorf("image unit_price = %q, want %q", got, want)
	}
	if got, want := usageCostMetadata(usage, usageCostImageTool, "unit"), "USD/1M tokens"; got != want {
		t.Errorf("image unit = %q, want %q", got, want)
	}
}

func TestAddUsageCostForModel_CombinesResponsesContextAndImageCost(t *testing.T) {
	usage := newTokenUsage("gpt-image-2", "", 12, 0, 0, 0, 0)
	addUsageCostForModel(usage, "gpt-5.4", "", 1000, 500, 0, 0, "responses_context", "上下文")
	fillUsageCostPerImageBySize(usage, 1, "1024x1024", "")

	if got := usageCostByKey(usage, "responses_context_"+usageCostInput); !almostEqual(got, 0.0025, 1e-9) {
		t.Errorf("context input cost = %v, want 0.0025", got)
	}
	if got := usageCostByKey(usage, "responses_context_"+usageCostOutput); !almostEqual(got, 0.0075, 1e-9) {
		t.Errorf("context output cost = %v, want 0.0075", got)
	}
	wantImageCost := tokenCost(lookupImageGenOutputTokens("1024x1024", ""), 30)
	if got := usageCostByKey(usage, usageCostImage); !almostEqual(got, wantImageCost, 1e-9) {
		t.Errorf("image cost = %v, want %v", got, wantImageCost)
	}
	wantCost := tokenCost(12, 5) + 0.0025 + 0.0075 + wantImageCost
	if !almostEqual(usage.AccountCost, wantCost, 1e-9) {
		t.Errorf("AccountCost = %v, want %v", usage.AccountCost, wantCost)
	}
	if usage.Model != "gpt-image-2" {
		t.Errorf("Usage.Model = %q, want gpt-image-2", usage.Model)
	}
	if got, want := usageCostMetadata(usage, usageCostImage, "billing_model"), "gpt-5.5"; got != want {
		t.Errorf("image billing_model = %q, want %q", got, want)
	}
}

// TestFillUsageCostWithImageTool_NoToolUsage 退化为 fillUsageCost 行为不变。
func TestFillUsageCostWithImageTool_NoToolUsage(t *testing.T) {
	usage := newTokenUsage("gpt-5.4", "", 1000, 500, 0, 0, 0)
	fillUsageCostWithImageTool(usage, 0, "", 0, 0)
	if usageMetricInt(usage, usageMetricInputTokens) != 1000 || usageMetricInt(usage, usageMetricOutputTokens) != 500 {
		t.Errorf("token counts mutated when no image tool usage")
	}
	if !almostEqual(usageCostByKey(usage, usageCostInput), 0.0025, 1e-9) {
		t.Errorf("input cost = %v, want 0.0025", usageCostByKey(usage, usageCostInput))
	}
}

// TestCollectImageGenCall 抽取 output_item.done 里的 image_generation_call 条目。
func TestCollectImageGenCall(t *testing.T) {
	item := map[string]any{
		"type":           "image_generation_call",
		"status":         "completed",
		"id":             "ig_123",
		"result":         "iVBORw0KGgoAAA",
		"size":           "1024x1024",
		"quality":        "high",
		"output_format":  "png",
		"background":     "opaque",
		"revised_prompt": "a cute shiba inu",
	}
	var ws WSResult
	collectImageGenCall(&ws, item)
	if len(ws.ImageGenCalls) != 1 {
		t.Fatalf("ImageGenCalls len = %d, want 1", len(ws.ImageGenCalls))
	}
	got := ws.ImageGenCalls[0]
	if got.Result != "iVBORw0KGgoAAA" || got.Size != "1024x1024" || got.RevisedPrompt != "a cute shiba inu" {
		t.Errorf("ImageGenCall fields not populated: %+v", got)
	}
	// 非 image_generation_call 的 item 应被忽略
	collectImageGenCall(&ws, map[string]any{"type": "message"})
	if len(ws.ImageGenCalls) != 1 {
		t.Errorf("non-image item should be ignored")
	}
	// 缺 result 的也应被忽略，但要保留上游诊断，便于 failover 日志定位真实原因。
	collectImageGenCall(&ws, map[string]any{
		"type":   "image_generation_call",
		"id":     "ig_123",
		"status": "failed",
		"error":  map[string]any{"message": "safety system rejected the prompt"},
	})
	if len(ws.ImageGenCalls) != 1 {
		t.Errorf("item without result should be ignored")
	}
	if len(ws.ImageGenCallDiagnostics) != 1 {
		t.Fatalf("ImageGenCallDiagnostics len = %d, want 1", len(ws.ImageGenCallDiagnostics))
	}
	if len(ws.ImageGenCallFailures) != 1 {
		t.Fatalf("ImageGenCallFailures len = %d, want 1", len(ws.ImageGenCallFailures))
	}
	if ws.ImageGenCallFailures[0].Message != "safety system rejected the prompt" {
		t.Errorf("ImageGenCallFailures[0].Message = %q", ws.ImageGenCallFailures[0].Message)
	}
	if !strings.Contains(ws.ImageGenCallDiagnostics[0], "status=failed") || !strings.Contains(ws.ImageGenCallDiagnostics[0], "safety system rejected the prompt") {
		t.Errorf("diagnostic missing upstream cause: %s", ws.ImageGenCallDiagnostics[0])
	}
}

func TestCollectImageGenCallPartialImageMergesMetadata(t *testing.T) {
	var ws WSResult
	collectImageGenCallMetadata(&ws, map[string]any{
		"type":         "image_generation_call",
		"id":           "ig_123",
		"output_index": 0,
		"status":       "in_progress",
		"size":         "1024x1024",
		"model":        "gpt-image-1.5",
	})
	collectImageGenCallPartial(&ws, map[string]any{
		"type":          "response.image_generation_call.partial_image",
		"output_index":  0,
		"partial_image": "iVBORw0KGgoAAA",
	})

	if len(ws.ImageGenCalls) != 1 {
		t.Fatalf("ImageGenCalls len = %d, want 1", len(ws.ImageGenCalls))
	}
	got := ws.ImageGenCalls[0]
	if got.ID != "ig_123" {
		t.Fatalf("ID = %q, want ig_123", got.ID)
	}
	if !got.HasOutputIndex || got.OutputIndex != 0 {
		t.Fatalf("OutputIndex = %d / %v, want 0 / true", got.OutputIndex, got.HasOutputIndex)
	}
	if got.Result != "iVBORw0KGgoAAA" {
		t.Fatalf("Result = %q, want partial image", got.Result)
	}
	if got.Size != "1024x1024" {
		t.Fatalf("Size = %q, want 1024x1024", got.Size)
	}
	if got.Model != "gpt-image-1.5" {
		t.Fatalf("Model = %q, want gpt-image-1.5", got.Model)
	}
}

func TestClassifyImageGenCallFailuresSafetyRejected(t *testing.T) {
	failures := []ImageGenCallFailure{{
		Status:    "failed",
		ErrorCode: "content_policy_violation",
		Message:   "Your request was rejected by the safety system. If you believe this is an error, contact us at help.openai.com and include the request ID 916c6516-5f37-9121-b05a-a604888c0055.",
	}}
	failure := classifyImageGenCallFailures(failures, "")
	if failure == nil {
		t.Fatalf("expected failure")
	}
	if failure.Kind != responsesFailureKindClient {
		t.Fatalf("Kind = %q, want client", failure.Kind)
	}
	if failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", failure.StatusCode)
	}
	if failure.Code != "safety_rejected" {
		t.Fatalf("Code = %q, want safety_rejected", failure.Code)
	}
	body := buildImagesErrorBodyWithCode(failure.StatusCode, failure.Code, failure.Message)
	if got := gjson.GetBytes(body, "error.code").String(); got != "safety_rejected" {
		t.Fatalf("error.code = %q, want safety_rejected; body=%s", got, body)
	}
}

func TestClassifyUpstreamTaskErrorSafetyRejected(t *testing.T) {
	body := []byte(`{"error":{"message":"Your request was rejected by the safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`)
	taskErr := classifyUpstreamTaskError(http.StatusBadGateway, body)
	if taskErr.Type != "invalid_request" {
		t.Fatalf("Type = %q, want invalid_request", taskErr.Type)
	}
	if taskErr.Code != "safety_rejected" {
		t.Fatalf("Code = %q, want safety_rejected", taskErr.Code)
	}
	if taskErr.Retryable {
		t.Fatalf("Retryable = true, want false")
	}
}

func TestImageTaskQualityEcho(t *testing.T) {
	input, attrs, err := imageGenerateHandler{}.BuildInput(&sdk.ForwardRequest{
		Body:    []byte(`{"model":"gpt-image-2","prompt":"a shiba","size":"1024x1024","quality":"high"}`),
		Headers: http.Header{},
	}, "/v1/images/generations")
	if err != nil {
		t.Fatalf("BuildInput returned err: %v", err)
	}
	if got := input["quality"]; got != "high" {
		t.Fatalf("input quality = %v, want high", got)
	}
	if got := attrs["quality"]; got != "high" {
		t.Fatalf("attribute quality = %q, want high", got)
	}

	resp := buildImageTaskResponse(&sdk.HostTask{
		ID:     123,
		Status: sdk.TaskStatusCompleted,
		Input: map[string]interface{}{
			"prompt":  "a shiba",
			"size":    "1024x1024",
			"quality": "high",
		},
	})
	if got := resp["quality"]; got != "high" {
		t.Fatalf("response quality = %v, want high", got)
	}
}

func TestImageTaskBuildInputKeepsOpenAICompatibleGeminiModelAndSize(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Airgate-Group-ID", "123")
	headers.Set("X-Airgate-API-Key-ID", "456")
	input, attrs, err := imageGenerateHandler{}.BuildInput(&sdk.ForwardRequest{
		Model:   "gemini-3.1-flash-lite-image",
		Body:    []byte(`{"prompt":"a product hero","size":"1024x1024","quality":"standard"}`),
		Headers: headers,
	}, "/v1/images/generations")
	if err != nil {
		t.Fatalf("BuildInput returned err: %v", err)
	}
	if got := input["model"]; got != "gemini-3.1-flash-lite-image" {
		t.Fatalf("input model = %v", got)
	}
	if got := input["size"]; got != "1024x1024" {
		t.Fatalf("input size = %v", got)
	}
	if got := input["group_id"]; got != int64(123) {
		t.Fatalf("group_id = %v", got)
	}
	if got := input["api_key_id"]; got != int64(456) {
		t.Fatalf("api_key_id = %v", got)
	}
	if got := attrs["model"]; got != "gemini-3.1-flash-lite-image" {
		t.Fatalf("attribute model = %q", got)
	}
	if got := attrs["size"]; got != "1024x1024" {
		t.Fatalf("attribute size = %q", got)
	}
}

func TestImageTaskBuildInputRejectsUnsupportedGeminiLiteSize(t *testing.T) {
	_, _, err := imageGenerateHandler{}.BuildInput(&sdk.ForwardRequest{
		Model:   "gemini-3.1-flash-lite-image",
		Body:    []byte(`{"prompt":"a product hero","size":"2048x2048"}`),
		Headers: http.Header{},
	}, "/v1/images/generations")
	if err == nil {
		t.Fatal("expected unsupported size error")
	}
	if !strings.Contains(err.Error(), "gemini-3.1-flash-lite-image") || !strings.Contains(err.Error(), "2048x2048") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateImageModelSize(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		size    string
		wantErr bool
	}{
		{name: "gpt image 4k", model: "gpt-image-2", size: "3840x2160"},
		{name: "banana lite 1k", model: "gemini-3.1-flash-lite-image", size: "1024x1536"},
		{name: "banana lite rejects 2k", model: "gemini-3.1-flash-lite-image", size: "2048x2048", wantErr: true},
		{name: "banana 2 rejects 4k", model: "gemini-3.1-flash-image", size: "3840x2160", wantErr: true},
		{name: "banana 2 chat variant rejects 4k", model: "gemini-3.1-flash-image-c", size: "3840x2160", wantErr: true},
		{name: "unknown model passes through", model: "custom-image-model", size: "2048x2048"},
		{name: "empty size passes through", model: "gemini-3.1-flash-lite-image", size: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageModelSize(tt.model, tt.size)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestForwardAPIKeyRejectsUnsupportedImageSizeBeforeUpstream(t *testing.T) {
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"base_url": server.URL,
			"api_key":  "sk-test",
		}},
		Model:   "gemini-3.1-flash-lite-image",
		Body:    []byte(`{"prompt":"a product hero","size":"2048x2048"}`),
		Headers: headers,
	}, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("outcome kind = %v, want client error", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", outcome.Upstream.StatusCode)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstreamCalls = %d, want 0", upstreamCalls)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "2048x2048") {
		t.Fatalf("body = %s", outcome.Upstream.Body)
	}
}

func TestForwardAPIKeyKeepsPublicGPTImage2AcrossOutboundPaths(t *testing.T) {
	tiers := []struct {
		size      string
		wantModel string
	}{
		{size: "1024x1024", wantModel: "gpt-image-2"},
		{size: "2048x1152", wantModel: "gpt-image-2"},
		{size: "3840x2160", wantModel: "gpt-image-2"},
	}
	paths := []struct {
		name      string
		path      string
		multipart bool
	}{
		{name: "generate_json", path: "/v1/images/generations"},
		{name: "edit_json", path: "/v1/images/edits"},
		{name: "edit_multipart", path: "/v1/images/edits", multipart: true},
	}

	_, imageBytes, err := decodeDataImageURL(tinyPNGDataURL)
	if err != nil {
		t.Fatalf("decodeDataImageURL: %v", err)
	}
	for _, pathCase := range paths {
		for _, tier := range tiers {
			t.Run(pathCase.name+"_"+tier.size, func(t *testing.T) {
				var upstreamModel string
				var upstreamContentType string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamContentType = r.Header.Get("Content-Type")
					if strings.HasPrefix(strings.ToLower(upstreamContentType), "multipart/") {
						if err := r.ParseMultipartForm(2 << 20); err != nil {
							t.Errorf("ParseMultipartForm: %v", err)
						} else {
							upstreamModel = r.FormValue("model")
							if _, _, err := r.FormFile("image"); err != nil {
								if _, _, arrayErr := r.FormFile("image[]"); arrayErr != nil {
									t.Errorf("outbound edit dropped image: %v / %v", err, arrayErr)
								}
							}
						}
					} else {
						body, _ := io.ReadAll(r.Body)
						upstreamModel = gjson.GetBytes(body, "model").String()
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"model":%q,"data":[{"b64_json":"AA=="}]}`, tier.wantModel)
				}))
				defer server.Close()

				headers := http.Header{}
				headers.Set("X-Forwarded-Path", pathCase.path)
				var body []byte
				if pathCase.multipart {
					var buf bytes.Buffer
					writer := multipart.NewWriter(&buf)
					_ = writer.WriteField("model", "gpt-image-2")
					_ = writer.WriteField("prompt", "a product hero")
					_ = writer.WriteField("size", tier.size)
					part, createErr := writer.CreateFormFile("image", "input.png")
					if createErr != nil {
						t.Fatal(createErr)
					}
					_, _ = part.Write(imageBytes)
					if closeErr := writer.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					body = buf.Bytes()
					headers.Set("Content-Type", writer.FormDataContentType())
					if _, parseErr := parseImagesRequest(body, headers.Get("Content-Type"), true); parseErr != nil {
						t.Fatalf("test multipart request must parse before forwarding: %v", parseErr)
					}
				} else {
					body = []byte(fmt.Sprintf(`{"model":"gpt-image-2","prompt":"a product hero","size":%q}`, tier.size))
					if pathCase.path == "/v1/images/edits" {
						body = []byte(fmt.Sprintf(`{"model":"gpt-image-2","prompt":"a product hero","size":%q,"image":%q}`, tier.size, tinyPNGDataURL))
					}
					headers.Set("Content-Type", "application/json")
				}

				g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
					Account: &sdk.Account{ID: 1, Credentials: map[string]string{
						"base_url": server.URL,
						"api_key":  "sk-test",
					}},
					Model:   "gpt-image-2",
					Body:    body,
					Headers: headers,
				}, "")
				if err != nil {
					t.Fatalf("forwardAPIKey returned err: %v", err)
				}
				if outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("Kind = %v, want success; body=%s", outcome.Kind, outcome.Upstream.Body)
				}
				if upstreamModel != tier.wantModel {
					t.Fatalf("upstream model = %q, want %q (content-type %q)", upstreamModel, tier.wantModel, upstreamContentType)
				}
				if pathCase.path == "/v1/images/edits" && !strings.HasPrefix(strings.ToLower(upstreamContentType), "multipart/form-data") {
					t.Fatalf("edit content-type = %q, want multipart/form-data", upstreamContentType)
				}
				if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-2" {
					t.Fatalf("billing model = %v, want public gpt-image-2", outcome.Usage)
				}
			})
		}
	}
}

func TestForwardAPIKeyMapsYhshuGPTImage2AcrossOutboundPaths(t *testing.T) {
	paths := []struct {
		name      string
		path      string
		multipart bool
	}{
		{name: "generate_json", path: "/v1/images/generations"},
		{name: "edit_json", path: "/v1/images/edits"},
		{name: "edit_multipart", path: "/v1/images/edits", multipart: true},
	}

	_, imageBytes, err := decodeDataImageURL(tinyPNGDataURL)
	if err != nil {
		t.Fatalf("decodeDataImageURL: %v", err)
	}
	for index, pathCase := range paths {
		t.Run(pathCase.name, func(t *testing.T) {
			var upstreamModel string
			var upstreamPath string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamPath = r.URL.Path
				if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/") {
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Errorf("ParseMultipartForm: %v", err)
					} else {
						upstreamModel = r.FormValue("model")
					}
				} else {
					body, _ := io.ReadAll(r.Body)
					upstreamModel = gjson.GetBytes(body, "model").String()
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"model":%q,"data":[{"model":%q,"b64_json":"AA=="}]}`, yhshuGPTImage2UpstreamModel, yhshuGPTImage2UpstreamModel)
			}))
			defer server.Close()

			accountID := int64(700 + index)
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = "example.com"
			targetAddress := server.Listener.Addr().String()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, targetAddress)
			}
			pool := NewTransportPool()
			pool.transports[poolKey(accountID, "")] = transport
			defer pool.CloseIdle()

			headers := http.Header{}
			headers.Set("X-Forwarded-Path", pathCase.path)
			var body []byte
			if pathCase.multipart {
				var buf bytes.Buffer
				writer := multipart.NewWriter(&buf)
				_ = writer.WriteField("model", "gpt-image-2")
				_ = writer.WriteField("prompt", "a product hero")
				_ = writer.WriteField("size", "2048x1152")
				part, createErr := writer.CreateFormFile("image", "input.png")
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, _ = part.Write(imageBytes)
				if closeErr := writer.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				body = buf.Bytes()
				headers.Set("Content-Type", writer.FormDataContentType())
			} else {
				body = []byte(`{"model":"gpt-image-2","prompt":"a product hero","size":"2048x1152"}`)
				if pathCase.path == "/v1/images/edits" {
					body = []byte(fmt.Sprintf(`{"model":"gpt-image-2","prompt":"a product hero","size":"2048x1152","image":%q}`, tinyPNGDataURL))
				}
				headers.Set("Content-Type", "application/json")
			}

			g := &OpenAIGateway{
				logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				transportPool: pool,
			}
			outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
				Account: &sdk.Account{ID: accountID, Credentials: map[string]string{
					"base_url": "https://www.yhshu.ai",
					"api_key":  "sk-test",
				}},
				Model:   "gpt-image-2",
				Body:    body,
				Headers: headers,
			}, "")
			if err != nil || outcome.Kind != sdk.OutcomeSuccess {
				t.Fatalf("forward outcome=%v err=%v body=%s", outcome.Kind, err, outcome.Upstream.Body)
			}
			if upstreamPath != pathCase.path {
				t.Fatalf("upstream path = %q, want %q", upstreamPath, pathCase.path)
			}
			if upstreamModel != yhshuGPTImage2UpstreamModel {
				t.Fatalf("upstream model = %q, want %q", upstreamModel, yhshuGPTImage2UpstreamModel)
			}
			if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-2" {
				t.Fatalf("billing model = %v, want public gpt-image-2", outcome.Usage)
			}
			if got := gjson.GetBytes(outcome.Upstream.Body, "model").String(); got != "gpt-image-2" {
				t.Fatalf("response root model = %q, want public alias; body=%s", got, outcome.Upstream.Body)
			}
			if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.model").String(); got != "gpt-image-2" {
				t.Fatalf("response item model = %q, want public alias; body=%s", got, outcome.Upstream.Body)
			}
		})
	}
}

func TestForwardAPIKeyKeepsResolutionSpecificGPTImage2Model(t *testing.T) {
	var upstreamModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-image-2-4k","data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	headers.Set("Content-Type", "application/json")
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"base_url": server.URL,
			"api_key":  "sk-test",
		}},
		Model:   "gpt-image-2-4k",
		Body:    []byte(`{"model":"gpt-image-2-4k","prompt":"a product hero","size":"1024x1024"}`),
		Headers: headers,
	}, "")
	if err != nil || outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("forward outcome=%v err=%v body=%s", outcome.Kind, err, outcome.Upstream.Body)
	}
	if upstreamModel != "gpt-image-2-4k" {
		t.Fatalf("specific upstream model = %q, want unchanged", upstreamModel)
	}
}

func TestForwardAPIKeyRoutesGeminiImageThroughOpenAICompatibleChat(t *testing.T) {
	upstreamCalls := 0
	var gotPath string
	var gotAuth string
	var gotModel string
	var gotPrompt string
	var gotModalities []string
	var gotResponseFormat string
	var gotSize string
	imageB64 := testPNGBase64(1, 1, func(x, y int) color.RGBA {
		return color.RGBA{R: 10, G: 20, B: 30, A: 255}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		gotPrompt = gjson.GetBytes(body, "messages.0.content").String()
		for _, item := range gjson.GetBytes(body, "modalities").Array() {
			gotModalities = append(gotModalities, item.String())
		}
		gotResponseFormat = gjson.GetBytes(body, "response_format.type").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_1","model":"gemini-3.1-flash-image","choices":[{"message":{"role":"assistant","content":"![image](data:image/png;base64,%s)"}}],"usage":{"prompt_tokens":7,"completion_tokens":11}}`, imageB64)
	}))
	defer server.Close()

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	headers.Set("Content-Type", "application/json")
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"base_url": server.URL + "/v1",
			"api_key":  "sk-test",
		}},
		Model:   "gemini-3.1-flash-image",
		Body:    []byte(`{"model":"gemini-3.1-flash-image","prompt":"a product hero","size":"2048x1152","n":1}`),
		Headers: headers,
	}, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want success; body=%s", outcome.Kind, outcome.Upstream.Body)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotModel != "gemini-3.1-flash-image" {
		t.Fatalf("model = %q", gotModel)
	}
	if !strings.Contains(gotPrompt, "a product hero") {
		t.Fatalf("prompt missing original text: %q", gotPrompt)
	}
	if len(gotModalities) != 2 || gotModalities[0] != "text" || gotModalities[1] != "image" {
		t.Fatalf("modalities = %#v", gotModalities)
	}
	if gotResponseFormat != "image" {
		t.Fatalf("response_format.type = %q", gotResponseFormat)
	}
	if strings.Contains(gotPrompt, "2048x1152") {
		gotSize = "2048x1152"
	}
	if gotSize != "2048x1152" {
		t.Fatalf("prompt missing size hint: %q", gotPrompt)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "model").String(); got != "gemini-3.1-flash-image" {
		t.Fatalf("response model = %q", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.b64_json").String(); got != imageB64 {
		t.Fatalf("response b64 mismatch")
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "usage.input_tokens").Int(); got != 7 {
		t.Fatalf("usage.input_tokens = %d, want 7", got)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "usage.output_tokens").Int(); got != 11 {
		t.Fatalf("usage.output_tokens = %d, want 11", got)
	}
}

func TestForwardAPIKeyRoutesGeminiImageThroughNativeImagesAPIWhenConfigured(t *testing.T) {
	var gotPath string
	var gotModel string
	var gotSize string
	imageB64 := testPNGBase64(1, 1, func(x, y int) color.RGBA {
		return color.RGBA{R: 10, G: 20, B: 30, A: 255}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotModel = gjson.GetBytes(body, "model").String()
		gotSize = gjson.GetBytes(body, "size").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"model":"gemini-3.1-flash-image","data":[{"b64_json":%q}],"usage":{"input_tokens":7,"output_tokens":11}}`, imageB64)
	}))
	defer server.Close()

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	headers.Set("Content-Type", "application/json")
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"base_url":                    server.URL + "/v1",
			"api_key":                     "sk-test",
			geminiImageProtocolCredential: geminiImageProtocolImagesAPI,
		}},
		Model:   "gemini-3.1-flash-image",
		Body:    []byte(`{"model":"gemini-3.1-flash-image","prompt":"a product hero","size":"2048x1152","n":1}`),
		Headers: headers,
	}, "")
	if err != nil || outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("forward outcome=%v err=%v body=%s", outcome.Kind, err, outcome.Upstream.Body)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("path = %q, want native Images API", gotPath)
	}
	if gotModel != "gemini-3.1-flash-image" || gotSize != "2048x1152" {
		t.Fatalf("model=%q size=%q", gotModel, gotSize)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.b64_json").String(); got != imageB64 {
		t.Fatalf("response b64 mismatch")
	}
}

// TestForwardAPIKeyGeminiImageEditViaChatBridge 图生图不再 400，而是转成
// chat completions 桥接请求：参考图以 image_url 分段携带、响应重组为 Images REST。
func TestForwardAPIKeyGeminiImageEditViaChatBridge(t *testing.T) {
	upstreamCalls := 0
	var upstreamPath string
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"![img](` + tinyPNGDataURL + `)"}}],"usage":{"prompt_tokens":7,"completion_tokens":11}}`))
	}))
	defer server.Close()

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/edits")
	headers.Set("Content-Type", "application/json")
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"base_url": server.URL,
			"api_key":  "sk-test",
		}},
		Model:   "gemini-3.1-flash-image",
		Body:    []byte(`{"model":"gemini-3.1-flash-image","prompt":"make it yellow","image":"` + tinyPNGDataURL + `"}`),
		Headers: headers,
	}, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned err: %v", err)
	}
	if upstreamCalls != 1 || !strings.HasSuffix(upstreamPath, "/v1/chat/completions") {
		t.Fatalf("upstream calls=%d path=%s, want 1 次 chat completions", upstreamCalls, upstreamPath)
	}
	parts := gjson.GetBytes(upstreamBody, "messages.0.content")
	if !parts.IsArray() || len(parts.Array()) != 2 {
		t.Fatalf("桥接请求应带 text+image 分段: %s", parts.Raw)
	}
	if url := parts.Get("1.image_url.url").String(); !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("参考图应以 data URL image_url 携带: %.60s", url)
	}
	if outcome.Upstream.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", outcome.Upstream.StatusCode, outcome.Upstream.Body)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "data.0.b64_json").String(); got == "" {
		t.Fatalf("响应应重组为 Images REST: %s", outcome.Upstream.Body)
	}
}

// TestBuildImagesToolCreateMsg 翻译 Images REST 请求体为 Codex HTTP SSE
// Responses body，tool 配置保持 Codex 对齐的极简 schema。
func TestBuildImagesToolCreateMsg(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1.5","prompt":"a shiba","n":1,"size":"1024x1024","quality":"low","background":"transparent","output_format":"png"}`)
	msg, n, promptTokens, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	// "a shiba" = 7 runes → (7+2)/3 = 3 tokens
	if promptTokens != 3 {
		t.Errorf("promptTokens = %d, want 3", promptTokens)
	}

	if gjson.GetBytes(msg, "type").Exists() {
		t.Errorf("top-level type should not be present for HTTP SSE body: %s", msg)
	}
	if gjson.GetBytes(msg, "model").String() != imagesOAuthChatModel {
		t.Errorf("model = %q, want %q", gjson.GetBytes(msg, "model").String(), imagesOAuthChatModel)
	}
	if gjson.GetBytes(msg, "tool_choice").String() != "auto" {
		t.Errorf("tool_choice = %q, want auto", gjson.GetBytes(msg, "tool_choice").String())
	}
	inputItem := gjson.GetBytes(msg, "input.0")
	if inputItem.Get("type").String() != "message" || inputItem.Get("role").String() != "user" {
		t.Errorf("input[0] type/role wrong: %s", inputItem.Raw)
	}
	prompt := inputItem.Get("content.0.text").String()
	if !strings.HasPrefix(prompt, "a shiba\n\nImage API constraints:\n") {
		t.Errorf("prompt prefix wrong: %q", prompt)
	}
	for _, want := range []string{
		"Generate the image at 1024x1024 pixels.",
		"Use image quality setting low.",
		"Use background setting transparent.",
		"Use requested image model gpt-image-1.5.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing constraint %q: %q", want, prompt)
		}
		if !strings.Contains(gjson.GetBytes(msg, "instructions").String(), want) {
			t.Errorf("instructions missing constraint %q", want)
		}
	}
	tool := gjson.GetBytes(msg, "tools.0")
	if tool.Get("type").String() != "image_generation" {
		t.Errorf("tools[0].type = %q, want image_generation", tool.Get("type").String())
	}
	if tool.Get("output_format").String() != "png" {
		t.Errorf("tools[0].output_format = %q, want png", tool.Get("output_format").String())
	}
	// size / quality / background 必须直接落到 tool 字段，
	// 否则上游 image_generation 收不到这些参数会按默认（1024×1024）出图。
	if got := tool.Get("size").String(); got != "1024x1024" {
		t.Errorf("tools[0].size = %q, want 1024x1024", got)
	}
	if got := tool.Get("quality").String(); got != "low" {
		t.Errorf("tools[0].quality = %q, want low", got)
	}
	if got := tool.Get("background").String(); got != "transparent" {
		t.Errorf("tools[0].background = %q, want transparent", got)
	}
	// 这些字段不应出现：
	//   - n：image_generation 一次只产 1 张，n>1 需要多轮工具调用编排，不通过 tool 字段
	//   - model：image_generation tool 不接受 model 字段，image 模型由工具自己定（默认 gpt-image-2），
	//     写了上游会 502；顶层 payload.model 给 chat 模型用是另一回事
	for _, forbidden := range []string{"n", "model"} {
		if tool.Get(forbidden).Exists() {
			t.Errorf("tools[0].%s should not be present", forbidden)
		}
	}
}

func TestBuildImagesToolCreateMsg_ClampsOversizedSize(t *testing.T) {
	body := []byte(`{"model":"gpt-image-1.5","prompt":"a shiba","n":1,"size":"4096x2304"}`)
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	prompt := gjson.GetBytes(msg, "input.0.content.0.text").String()
	if !strings.Contains(prompt, "Generate the image at 3840x2160 pixels.") {
		t.Errorf("prompt missing clamped size constraint: %q", prompt)
	}
	// clamp 后的 size 也要落到 tool 字段（之前期望"不存在"是按 prompt-only 策略写的，
	// 现在改双轨：tool 字段是权威来源）。
	if got := gjson.GetBytes(msg, "tools.0.size").String(); got != "3840x2160" {
		t.Errorf("tools[0].size = %q, want clamped 3840x2160", got)
	}
}

func TestBuildImagesToolCreateMsg_KeepsSessionFieldsWithoutEventWrapper(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","prompt":"a shiba","n":1,"size":"1024x1024"}`)
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{
		PromptCacheKey: "cache-key-1",
	})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	if gjson.GetBytes(msg, "type").Exists() {
		t.Fatalf("HTTP SSE body must not include response.create event wrapper: %s", msg)
	}
	if got := gjson.GetBytes(msg, "prompt_cache_key").String(); got != "cache-key-1" {
		t.Fatalf("prompt_cache_key = %q, want cache-key-1", got)
	}
	if got := gjson.GetBytes(msg, "tools.0.type").String(); got != "image_generation" {
		t.Fatalf("tools[0].type = %q, want image_generation", got)
	}
}

// TestBuildImagesToolCreateMsg_NGreaterThanOne V1 不支持 n>1，应直接返错。
func TestBuildImagesToolCreateMsg_NGreaterThanOne(t *testing.T) {
	body := []byte(`{"prompt":"x","n":3}`)
	_, _, _, err := buildImagesToolCreateMsg(body, "application/json", false, openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for n>1, got nil")
	}
}

// TestBuildImagesToolCreateMsg_EmptyPrompt prompt 空串应报错。
func TestBuildImagesToolCreateMsg_EmptyPrompt(t *testing.T) {
	_, _, _, err := buildImagesToolCreateMsg([]byte(`{"n":1}`), "application/json", false, openAISessionResolution{})
	if err == nil {
		t.Fatal("expected err for empty prompt, got nil")
	}
}

// TestBuildImagesToolCreateMsg_Edit_JSON 验证 /images/edits 走 JSON 路径时：
// 参考图以 input_image 注入，mask 转成 Codex built-in image_gen 可理解的区域标注图。
func TestBuildImagesToolCreateMsg_Edit_JSON(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 80, G: 90, B: 100, A: 255}
	})
	maskBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		if x == 1 && y == 1 {
			return color.RGBA{A: 0}
		}
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	})
	maskSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(maskBytes)
	}))
	defer maskSrv.Close()
	maskRef := maskSrv.URL + "/mask.png"
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-1.5",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"input_fidelity":"high",
		"output_format":"jpeg",
		"image":%q,
		"mask":%q
	}`, imageRef, maskRef))
	msg, n, inputTokens, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gjson.GetBytes(msg, "type").Exists() {
		t.Errorf("top-level type should not be present for HTTP SSE body: %s", msg)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	// text prompt "make it cyberpunk" = 17 runes → 6；reference image + region annotation = 2 * 272 → 550
	if inputTokens != 6+272*2 {
		t.Errorf("inputTokens = %d, want %d", inputTokens, 6+272*2)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 3 {
		t.Fatalf("content len = %d, want 3 (text + image + region annotation)", len(content.Array()))
	}
	if content.Get("0.type").String() != "input_text" || content.Get("1.type").String() != "input_image" || content.Get("2.type").String() != "input_image" {
		t.Errorf("content types wrong: %s", content.Raw)
	}
	if content.Get("1.image_url").String() != imageRef {
		t.Errorf("image_url not propagated: %s", content.Raw)
	}
	annotationRef := content.Get("2.image_url").String()
	if annotationRef == "" || annotationRef == maskRef || !strings.HasPrefix(annotationRef, "data:image/png;base64,") {
		t.Errorf("region annotation not generated: %s", content.Raw)
	}
	prompt := content.Get("0.text").String()
	for _, want := range []string{
		"Image 1 is the edit target; preserve its framing, identity, geometry, lighting, and all unrequested details.",
		"Preserve the input image with high fidelity.",
		"Image 2 is a region annotation derived from the edit mask; change only the red marked area in Image 1 and keep everything outside that region unchanged.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing edit constraint %q: %q", want, prompt)
		}
		if !strings.Contains(gjson.GetBytes(msg, "instructions").String(), want) {
			t.Errorf("instructions missing edit constraint %q", want)
		}
	}
	tool := gjson.GetBytes(msg, "tools.0")
	if tool.Get("output_format").String() != "png" {
		t.Errorf("tools[0].output_format = %q, want png", tool.Get("output_format").String())
	}
	// /edits 模式下 size 同样要落到 tool 字段（双轨：tool 字段权威 + prompt 兜底）。
	if got := tool.Get("size").String(); got != "1024x1024" {
		t.Errorf("tools[0].size = %q, want 1024x1024", got)
	}
	// 这些字段仍不应出现：
	//   - action / input_image_mask：codex CLI 字段，不是 Responses API image_generation tool 的合法字段
	//   - input_fidelity：当前未作为 tool 字段透传（在 prompt constraints 里走兜底，gpt-image-2 上跳过）
	//   - model：image_generation tool 不接受 model 字段，写了会 502
	for _, forbidden := range []string{"action", "input_image_mask", "input_fidelity", "model"} {
		if tool.Get(forbidden).Exists() {
			t.Errorf("tools[0].%s should not be present", forbidden)
		}
	}
}

func TestBuildAPIKeyImagesEditMultipartBody_RemoteRefs(t *testing.T) {
	imageBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 90, G: 100, B: 110, A: 255}
	})
	maskBytes := testPNGBytes(2, 2, func(x, y int) color.RGBA {
		if x == 0 && y == 0 {
			return color.RGBA{A: 0}
		}
		return color.RGBA{A: 255}
	})
	imageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageBytes)
	}))
	defer imageSrv.Close()
	maskSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(maskBytes)
	}))
	defer maskSrv.Close()

	body := []byte(fmt.Sprintf(`{
		"prompt":"relight the scene",
		"model":"gpt-image-2",
		"size":"1024x1024",
		"quality":"auto",
		"n":1,
		"image":%q,
		"mask":%q
	}`, imageSrv.URL+"/image.png", maskSrv.URL+"/mask.png"))

	multipartBody, contentType, err := buildAPIKeyImagesEditMultipartBody(body, "application/json")
	if err != nil {
		t.Fatalf("buildAPIKeyImagesEditMultipartBody err: %v", err)
	}
	req, err := parseImagesRequest(multipartBody, contentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if len(req.Images) != 1 || !strings.HasPrefix(req.Images[0], "data:image/png;base64,") {
		t.Fatalf("remote image not normalized into multipart data URL: %+v", req.Images)
	}
	if !strings.HasPrefix(req.Mask, "data:image/png;base64,") {
		t.Fatalf("remote mask not normalized into multipart data URL: %q", req.Mask)
	}
}

// TestBuildImagesToolCreateMsg_Edit_MissingImage /edits 模式下缺 image 字段应报错。
func TestBuildImagesToolCreateMsg_Edit_MissingImage(t *testing.T) {
	_, _, _, err := buildImagesToolCreateMsg(
		[]byte(`{"prompt":"x"}`), "application/json", true, openAISessionResolution{},
	)
	if err == nil {
		t.Fatal("expected err for missing image, got nil")
	}
}

// TestBuildImagesToolCreateMsg_Edit_Img2ImgNoMask 钉死 OAuth Responses-tool 路径
// 上"纯图生图"（无 mask）的载荷形状：必须 1 张 input_image 而且不生成 region
// annotation。这是 studio ComposerBar 图生图入口最常见的请求形态，是 OAuth
// 路径上历史最容易"看上去通了但参考图没生效"的退化点。
func TestBuildImagesToolCreateMsg_Edit_Img2ImgNoMask(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 120, G: 30, B: 30, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"image":%q
	}`, imageRef))

	msg, n, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 2 {
		t.Fatalf("content len = %d, want 2 (text + 1 image, no region annotation); raw=%s",
			len(content.Array()), content.Raw)
	}
	if content.Get("0.type").String() != "input_text" {
		t.Errorf("content[0].type = %q, want input_text", content.Get("0.type").String())
	}
	if content.Get("1.type").String() != "input_image" {
		t.Errorf("content[1].type = %q, want input_image", content.Get("1.type").String())
	}
	if got := content.Get("1.image_url").String(); got != imageRef {
		t.Errorf("content[1].image_url not propagated verbatim; got len=%d", len(got))
	}
	// Region annotation 只在 mask 存在时生成。img2img 不应有第三个 input_image。
	if content.Get("2").Exists() {
		t.Errorf("img2img without mask must not append region annotation; raw=%s", content.Raw)
	}
}

// TestBuildImagesToolCreateMsg_Edit_MultiImage 钉死多参考图场景：studio 在
// ComposerBar 上传多张图时，buildImageRequestBody 会把 image 输出成数组。
// OAuth Responses-tool 路径必须把每张都附为 input_image，且顺序与请求一致。
func TestBuildImagesToolCreateMsg_Edit_MultiImage(t *testing.T) {
	imageA := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 10, G: 20, B: 30, A: 255}
	})
	imageB := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 200, G: 210, B: 220, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"merge the two scenes",
		"size":"1024x1024",
		"image":[%q,%q]
	}`, imageA, imageB))

	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) != 3 {
		t.Fatalf("content len = %d, want 3 (text + 2 images); raw=%s", len(content.Array()), content.Raw)
	}
	if got := content.Get("1.image_url").String(); got != imageA {
		t.Errorf("content[1].image_url should be image A (verbatim); len=%d", len(got))
	}
	if got := content.Get("2.image_url").String(); got != imageB {
		t.Errorf("content[2].image_url should be image B (verbatim); len=%d", len(got))
	}
}

// TestBuildImagesToolCreateMsg_Edit_StudioShape 验证 gateway-openai 的 task
// 执行链路在 OAuth 账号上的端到端形状：执行 buildImageRequestBody 后得到的
// JSON 字节，直接喂给 buildImagesToolCreateMsg，必须解析出 input_image。
// 守住"studio 任务路径 + OAuth 账号"这一组合，不再被静默降级成纯文生图。
func TestBuildImagesToolCreateMsg_Edit_StudioShape(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 60, G: 60, B: 200, A: 255}
	})
	// 复刻 executeImageTask → buildImageRequestBody 在 studio img2img 任务上
	// 真实写出的 input map（image 单元素时取字符串、size=auto 是 studio 默认）。
	taskInput := map[string]any{
		"prompt":             "replace the red ball with a blue cube",
		"model":              "gpt-image-2",
		"size":               "auto",
		"images":             []string{imageRef},
		"preserve_reference": true,
	}
	jsonBody, err := buildImageRequestBody(taskInput)
	if err != nil {
		t.Fatalf("buildImageRequestBody: %v", err)
	}
	// 必要前置：emitter 一定要在体内放上 image 字段。
	if !gjson.GetBytes(jsonBody, "image").Exists() {
		t.Fatalf("buildImageRequestBody dropped image field: %s", jsonBody)
	}

	msg, n, _, err := buildImagesToolCreateMsg(jsonBody, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg: %v", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	content := gjson.GetBytes(msg, "input.0.content")
	if !content.IsArray() || len(content.Array()) < 2 {
		t.Fatalf("studio shape must produce ≥2 content items (text + image); got %s", content.Raw)
	}
	if content.Get("1.type").String() != "input_image" {
		t.Errorf("studio shape must surface input_image as second content item; raw=%s", content.Raw)
	}
	if got := content.Get("1.image_url").String(); got != imageRef {
		t.Errorf("studio source image not propagated through Responses-tool path")
	}
}

func TestShrinkTaskInputImages_ResizesMaskWithShrunkImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	seed := uint32(1)
	for y := 0; y < 3000; y++ {
		for x := 0; x < 3000; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	mask := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	draw.Draw(mask, mask.Bounds(), image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	draw.Draw(mask, image.Rect(750, 900, 1600, 1800), image.Transparent, image.Point{}, draw.Src)

	input := map[string]any{
		"images": []string{testPNGDataURLFromImage(img)},
		"mask":   testPNGDataURLFromImage(mask),
	}
	if err := shrinkTaskInputImages(input); err != nil {
		t.Fatalf("shrinkTaskInputImages returned err: %v", err)
	}
	images := input["images"].([]string)
	if len(images) != 1 {
		t.Fatalf("images len = %d", len(images))
	}
	imageMime, imageBytes, err := decodeDataImageURL(images[0])
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	maskMime, maskBytes, err := decodeDataImageURL(input["mask"].(string))
	if err != nil {
		t.Fatalf("decode mask: %v", err)
	}
	if imageMime != "image/jpeg" {
		t.Fatalf("image mime = %q, want compressed jpeg", imageMime)
	}
	if maskMime != "image/png" {
		t.Fatalf("mask mime = %q, want png", maskMime)
	}
	imageCfg, _, err := image.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatalf("decode image config: %v", err)
	}
	maskCfg, _, err := image.DecodeConfig(bytes.NewReader(maskBytes))
	if err != nil {
		t.Fatalf("decode mask config: %v", err)
	}
	if imageCfg.Width >= 3000 || imageCfg.Height >= 3000 {
		t.Fatalf("test image did not shrink: %dx%d", imageCfg.Width, imageCfg.Height)
	}
	if maskCfg.Width != imageCfg.Width || maskCfg.Height != imageCfg.Height {
		t.Fatalf("mask size = %dx%d, want image size %dx%d", maskCfg.Width, maskCfg.Height, imageCfg.Width, imageCfg.Height)
	}
}

type noopWSEventHandler struct{}

func (noopWSEventHandler) OnTextDelta(string)        {}
func (noopWSEventHandler) OnReasoningDelta(string)   {}
func (noopWSEventHandler) OnRawEvent(string, []byte) {}
func (noopWSEventHandler) OnRateLimits(float64)      {}

func TestReceiveWSResponseTreatsMessageTooBigAsStreamError(t *testing.T) {
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
		_ = conn.Close()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	result := ReceiveWSResponse(context.Background(), conn, noopWSEventHandler{})
	var failure *responsesFailureError
	if errors.As(result.Err, &failure) {
		t.Fatalf("Err = %#v, want plain stream error, not responsesFailureError", result.Err)
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want websocket close error")
	}
	if !strings.Contains(result.Err.Error(), "close 1009") {
		t.Fatalf("Err = %v, want websocket close 1009", result.Err)
	}
}

func TestBuildImagesToolCreateMsg_Edit_ShrinksLargeInputImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	seed := uint32(1)
	for y := 0; y < 1600; y++ {
		for x := 0; x < 1600; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	imageRef := testPNGDataURLFromImage(img)
	body := []byte(fmt.Sprintf(`{"prompt":"edit this","image":%q}`, imageRef))
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	got := gjson.GetBytes(msg, "input.0.content.1.image_url").String()
	if !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("image_url prefix = %.32q, want jpeg data URL", got)
	}
	comma := strings.IndexByte(got, ',')
	data, err := base64.StdEncoding.DecodeString(got[comma+1:])
	if err != nil {
		t.Fatalf("decoded shrunk image: %v", err)
	}
	if len(data) > maxResponsesInputImageBytes {
		t.Fatalf("shrunk image bytes = %d, want <= %d", len(data), maxResponsesInputImageBytes)
	}
}

func TestBuildImagesToolCreateMsg_Edit_AlignsRegionAnnotationWithShrunkTarget(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	seed := uint32(1)
	for y := 0; y < 3000; y++ {
		for x := 0; x < 3000; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	mask := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	draw.Draw(mask, mask.Bounds(), image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	draw.Draw(mask, image.Rect(750, 900, 1600, 1800), image.Transparent, image.Point{}, draw.Src)

	body := []byte(fmt.Sprintf(`{
		"prompt":"edit marked region",
		"model":"gpt-image-2",
		"image":%q,
		"mask":%q
	}`, testPNGDataURLFromImage(img), testPNGDataURLFromImage(mask)))
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("buildImagesToolCreateMsg returned err: %v", err)
	}
	targetRef := gjson.GetBytes(msg, "input.0.content.1.image_url").String()
	annotationRef := gjson.GetBytes(msg, "input.0.content.2.image_url").String()
	if targetRef == "" || annotationRef == "" {
		t.Fatalf("missing target or annotation: %s", msg)
	}
	_, targetBytes, err := decodeDataImageURL(targetRef)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	_, annotationBytes, err := decodeDataImageURL(annotationRef)
	if err != nil {
		t.Fatalf("decode annotation: %v", err)
	}
	if len(targetBytes) > maxResponsesInputImageBytes {
		t.Fatalf("target bytes = %d, want <= %d", len(targetBytes), maxResponsesInputImageBytes)
	}
	if len(annotationBytes) > maxResponsesInputImageBytes {
		t.Fatalf("annotation bytes = %d, want <= %d", len(annotationBytes), maxResponsesInputImageBytes)
	}
	targetCfg, _, err := image.DecodeConfig(bytes.NewReader(targetBytes))
	if err != nil {
		t.Fatalf("decode target config: %v", err)
	}
	annotationCfg, _, err := image.DecodeConfig(bytes.NewReader(annotationBytes))
	if err != nil {
		t.Fatalf("decode annotation config: %v", err)
	}
	if targetCfg.Width >= 3000 || targetCfg.Height >= 3000 {
		t.Fatalf("test target did not shrink: %dx%d", targetCfg.Width, targetCfg.Height)
	}
	if annotationCfg.Width != targetCfg.Width || annotationCfg.Height != targetCfg.Height {
		t.Fatalf("annotation size = %dx%d, want target size %dx%d", annotationCfg.Width, annotationCfg.Height, targetCfg.Width, targetCfg.Height)
	}
}

// TestParseImagesEditMultipart 覆盖 OpenAI SDK 标准的 multipart/form-data 请求：
// image 文件 + prompt 文本 + mask 文件 → 规范化后 images / mask 都应是 data URL。
func TestBuildAPIKeyImagesEditMultipartBody(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}
	body := []byte(fmt.Sprintf(`{
		"prompt":"relight the scene",
		"model":"gpt-image-2",
		"size":"1024x1024",
		"quality":"auto",
		"n":1,
		"image":"data:image/png;base64,%s"
	}`, base64.StdEncoding.EncodeToString(pngBytes)))

	multipartBody, contentType, err := buildAPIKeyImagesEditMultipartBody(body, "application/json")
	if err != nil {
		t.Fatalf("buildAPIKeyImagesEditMultipartBody err: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("contentType = %q", contentType)
	}

	req, err := parseImagesRequest(multipartBody, contentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if req.Prompt != "relight the scene" || req.Model != "gpt-image-2" || req.Size != "1024x1024" || req.Quality != "auto" || req.N != 1 {
		t.Fatalf("fields wrong: %+v", req)
	}
	if len(req.Images) != 1 || req.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("image wrong: %+v", req.Images)
	}
}

func TestBuildAPIKeyImagesEditMultipartBody_ResizesMaskWithShrunkImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	seed := uint32(1)
	for y := 0; y < 3000; y++ {
		for x := 0; x < 3000; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	mask := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	draw.Draw(mask, mask.Bounds(), image.NewUniform(color.RGBA{R: 255, G: 255, B: 255, A: 255}), image.Point{}, draw.Src)
	draw.Draw(mask, image.Rect(750, 900, 1600, 1800), image.Transparent, image.Point{}, draw.Src)

	body := []byte(fmt.Sprintf(`{
		"prompt":"edit marked region",
		"model":"gpt-image-2",
		"image":%q,
		"mask":%q
	}`, testPNGDataURLFromImage(img), testPNGDataURLFromImage(mask)))

	multipartBody, contentType, err := buildAPIKeyImagesEditMultipartBody(body, "application/json")
	if err != nil {
		t.Fatalf("buildAPIKeyImagesEditMultipartBody err: %v", err)
	}
	req, err := parseImagesRequest(multipartBody, contentType, true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if len(req.Images) != 1 || req.Mask == "" {
		t.Fatalf("missing image/mask: images=%d mask=%v", len(req.Images), req.Mask != "")
	}

	imageMime, imageBytes, err := decodeDataImageURL(req.Images[0])
	if err != nil {
		t.Fatalf("decode image: %v", err)
	}
	maskMime, maskBytes, err := decodeDataImageURL(req.Mask)
	if err != nil {
		t.Fatalf("decode mask: %v", err)
	}
	if imageMime != "image/jpeg" {
		t.Fatalf("image mime = %q, want compressed jpeg", imageMime)
	}
	if maskMime != "image/png" {
		t.Fatalf("mask mime = %q, want png", maskMime)
	}
	imageCfg, _, err := image.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatalf("decode image config: %v", err)
	}
	maskCfg, _, err := image.DecodeConfig(bytes.NewReader(maskBytes))
	if err != nil {
		t.Fatalf("decode mask config: %v", err)
	}
	if imageCfg.Width >= 3000 || imageCfg.Height >= 3000 {
		t.Fatalf("test image did not shrink: %dx%d", imageCfg.Width, imageCfg.Height)
	}
	if maskCfg.Width != imageCfg.Width || maskCfg.Height != imageCfg.Height {
		t.Fatalf("mask size = %dx%d, want image size %dx%d", maskCfg.Width, maskCfg.Height, imageCfg.Width, imageCfg.Height)
	}
}

func TestParseImagesEditMultipart(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}
	maskBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0xFF}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "relight the scene")
	_ = mw.WriteField("model", "gpt-image-1.5")
	_ = mw.WriteField("size", "1024x1024")
	_ = mw.WriteField("quality", "high")
	_ = mw.WriteField("input_fidelity", "high")

	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="image"; filename="in.png"`)
	h.Set("Content-Type", "image/png")
	w, _ := mw.CreatePart(h)
	_, _ = w.Write(pngBytes)

	hm := textproto.MIMEHeader{}
	hm.Set("Content-Disposition", `form-data; name="mask"; filename="mask.png"`)
	hm.Set("Content-Type", "image/png")
	wm, _ := mw.CreatePart(hm)
	_, _ = wm.Write(maskBytes)

	_ = mw.Close()

	req, err := parseImagesRequest(buf.Bytes(), mw.FormDataContentType(), true)
	if err != nil {
		t.Fatalf("parseImagesRequest err: %v", err)
	}
	if !req.IsEdit || req.Prompt != "relight the scene" {
		t.Errorf("prompt / edit flag wrong: %+v", req)
	}
	if req.Model != "gpt-image-1.5" || req.Size != "1024x1024" ||
		req.Quality != "high" || req.InputFidelity != "high" {
		t.Errorf("fields mis-parsed: %+v", req)
	}
	if len(req.Images) != 1 ||
		req.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("image not encoded as data URL: %+v", req.Images)
	}
	if req.Mask != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(maskBytes) {
		t.Errorf("mask not encoded as data URL: %q", req.Mask)
	}
}

func TestNormalizeImageRef(t *testing.T) {
	cases := map[string]string{
		"data:image/png;base64,AAA=":     "data:image/png;base64,AAA=",
		"data:image/png;base64,QUJD\nRA": "data:image/png;base64,QUJDRA==",
		"https://example.com/a.png":      "https://example.com/a.png",
	}
	for in, want := range cases {
		got, err := normalizeImageRef(in)
		if err != nil {
			t.Fatalf("normalizeImageRef(%q) err: %v", in, err)
		}
		if got != want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeImageRefRejectsUnsupportedFormat(t *testing.T) {
	if got, err := normalizeImageRef("QUJD\nRA"); err == nil {
		t.Fatalf("normalizeImageRef bare base64 = %q, want err", got)
	}
}

// TestEstimatePromptTokens 覆盖常见输入。粗略 / 3 上取整，够用即可。
func TestEstimatePromptTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},           // 1 rune → 1
		{"abc", 1},         // 3 runes → 1
		{"abcd", 2},        // 4 runes → 2
		{"a shiba", 3},     // 7 runes → 3
		{"可爱柴犬", 2},        // 4 runes → 2
		{"一只可爱的柴犬在草地上", 4}, // 10 runes → 4
	}
	for _, tc := range cases {
		if got := estimatePromptTokens(tc.in); got != tc.want {
			t.Errorf("estimatePromptTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestBuildImagesRESTResponse 把 WSResult 打包回 OpenAI Images REST 响应格式。
// 计费口径对齐 OpenAI 官方：usage.input_tokens = prompt tokens、output_tokens = 图像 tokens、
// root 级 model 使用实际响应的图像模型。instructions / 工具包装的 chat tokens 不暴露。
func TestBuildImagesRESTResponse(t *testing.T) {
	ws := WSResult{
		InputTokens:           4808, // chat text tokens (内层吸收，不对外)
		OutputTokens:          40,   // chat output tokens (内层吸收，不对外)
		ToolImageInputTokens:  0,
		ToolImageOutputTokens: 4160,
		ToolImageModel:        "gpt-image-2",
		ImageGenCalls: []ImageGenCall{
			{Result: "PNG_BASE64_A", RevisedPrompt: "revised a"},
			{Result: "PNG_BASE64_B"},
		},
	}
	promptTokens := 12
	imageOut := 4160
	body := buildImagesRESTResponse(ws, promptTokens, imageOut, imageGenerationBillingModel(ws.ToolImageModel, "dall-e-3"))

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "gpt-image-2" {
		t.Errorf("root model = %v, want gpt-image-2", got["model"])
	}
	data, _ := got["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data len = %d, want 2", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "PNG_BASE64_A" || first["revised_prompt"] != "revised a" {
		t.Errorf("data[0] fields wrong: %+v", first)
	}
	second, _ := data[1].(map[string]any)
	if second["b64_json"] != "PNG_BASE64_B" {
		t.Errorf("data[1].b64_json = %v, want PNG_BASE64_B", second["b64_json"])
	}
	if _, ok := second["revised_prompt"]; ok {
		t.Errorf("empty revised_prompt should be omitted")
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing")
	}
	if int(usage["input_tokens"].(float64)) != promptTokens {
		t.Errorf("usage.input_tokens = %v, want %d", usage["input_tokens"], promptTokens)
	}
	if int(usage["output_tokens"].(float64)) != imageOut {
		t.Errorf("usage.output_tokens = %v, want %d", usage["output_tokens"], imageOut)
	}
	if int(usage["total_tokens"].(float64)) != promptTokens+imageOut {
		t.Errorf("usage.total_tokens wrong")
	}
}

// TestBuildImagesRESTResponse_ChainedCostParity 验证 AirGate 套 AirGate 时两级
// 金额一致：下一级拿到 body 按 root model 单价重算，应等于本级结果。
func TestBuildImagesRESTResponse_ChainedCostParity(t *testing.T) {
	promptTokens := 12
	imageOut := 1056
	ws := WSResult{ImageGenCalls: []ImageGenCall{{Result: "X"}}}
	body := buildImagesRESTResponse(ws, promptTokens, imageOut, "gpt-image-2")

	inner := newTokenUsage("gpt-image-2", "", promptTokens, imageOut, 0, 0, 0)
	fillUsageCost(inner)
	innerCost := inner.AccountCost

	var got map[string]any
	_ = json.Unmarshal(body, &got)
	u := got["usage"].(map[string]any)
	outer := newTokenUsage(got["model"].(string), "", int(u["input_tokens"].(float64)), int(u["output_tokens"].(float64)), 0, 0, 0)
	fillUsageCost(outer)
	outerCost := outer.AccountCost

	if innerCost != outerCost {
		t.Errorf("cost mismatch: inner=%.6f outer=%.6f", innerCost, outerCost)
	}
}

func TestAccountRequiresResponsesImageTool(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "responses only", header: "responses_tool", want: true},
		{name: "both protocols", header: "images_api,responses_tool", want: false},
		{name: "images only", header: "images_api", want: false},
		{name: "empty", header: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("X-Airgate-Account-Image-Protocols", tc.header)
			if got := accountRequiresResponsesImageTool(headers); got != tc.want {
				t.Fatalf("accountRequiresResponsesImageTool(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestImageGenCallsFromResponsesBody(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"image_generation","model":"gpt-image-2"}],
		"output":[
			{"type":"message","content":[]},
			{"type":"image_generation_call","id":"ig_1","status":"completed","result":"AAA","size":"1024x1024","quality":"medium","output_format":"png","revised_prompt":"rp"},
			{"type":"image_generation_call","id":"ig_2","status":"generating","result":"BBB"}
		]
	}`)

	got := imageGenCallsFromResponsesBody(body)
	if got.ToolImageModel != "gpt-image-2" {
		t.Fatalf("ToolImageModel = %q, want gpt-image-2", got.ToolImageModel)
	}
	if len(got.ImageGenCalls) != 1 {
		t.Fatalf("ImageGenCalls len = %d, want 1", len(got.ImageGenCalls))
	}
	call := got.ImageGenCalls[0]
	if call.ID != "ig_1" || call.Result != "AAA" || call.Size != "1024x1024" || call.RevisedPrompt != "rp" {
		t.Fatalf("call parsed incorrectly: %+v", call)
	}
}

// TestLookupImageGenOutputTokens 按 OpenAI 官方表验证 size×quality→token 估算。
func TestLookupImageGenOutputTokens(t *testing.T) {
	cases := []struct {
		size    string
		quality string
		want    int
	}{
		{"1024x1024", "low", 272},
		{"1024x1024", "medium", 1056},
		{"1024x1024", "high", 4160},
		{"1024x1536", "low", 408},
		{"1536x1024", "high", 6240},
		// quality="auto" → medium
		{"1024x1024", "auto", 1056},
		{"1024x1024", "", 1056},
		// 未注册但可解析的 size 按像素面积缩放；无法解析才保底 1024×1024 medium。
		{"9999x9999", "high", 396650},
		{"garbage", "high", 4160},
		{"1024x1024", "unknown", 1056}, // unknown quality → medium
		// 大小写归一
		{"1024X1024", "HIGH", 4160},
	}
	for _, tc := range cases {
		if got := lookupImageGenOutputTokens(tc.size, tc.quality); got != tc.want {
			t.Errorf("lookup(%q,%q) = %d, want %d", tc.size, tc.quality, got, tc.want)
		}
	}
}

// TestEstimateImageGenOutputTokens 多张图总 token 数 = 每张相加。
func TestEstimateImageGenOutputTokens(t *testing.T) {
	calls := []ImageGenCall{
		{Size: "1024x1024", Quality: "low"},  // 272
		{Size: "1024x1536", Quality: "high"}, // 6240
		{Size: "1024x1024", Quality: ""},     // auto → medium → 1056
	}
	got := estimateImageGenOutputTokens(calls)
	want := 272 + 6240 + 1056
	if got != want {
		t.Errorf("estimateImageGenOutputTokens = %d, want %d", got, want)
	}
}

// TestForwardImagesViaResponsesTool_EmptyPrompt 客户端传空 prompt 时，
// 翻译层应在未建立 WS 连接的情况下返回 ClientError + 400，不伤账号状态。
func TestForwardImagesViaResponsesTool_EmptyPrompt(t *testing.T) {
	g := &OpenAIGateway{}
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "tok"}},
		Body:    []byte(`{"prompt":"","n":1}`),
		Headers: http.Header{},
		Writer:  w,
	}
	outcome, err := g.forwardImagesViaResponsesTool(t.Context(), req)
	if err != nil {
		t.Fatalf("expected nil err for client-side issue, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Errorf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Errorf("Upstream.StatusCode = %d, want 400", outcome.Upstream.StatusCode)
	}
	if w.Body.Len() != 0 {
		t.Errorf("Core 处理 outcome 前不应提交 writer，实际写入 %q", w.Body.String())
	}
}

// TestValidateImageSize 覆盖 gpt-image-2 size 硬约束的全部分支。
// 包括：空/auto 透传、显式 1.5 跳过、16 倍数对齐、3:1 比例、
// 总像素 [655360, 8294400] 范围、3840 边长上限。
func TestValidateImageSize(t *testing.T) {
	cases := []struct {
		name    string
		size    string
		model   string
		wantErr bool
	}{
		// 透传：空 / auto 不校验
		{"empty size", "", "gpt-image-2", false},
		{"auto size", "auto", "gpt-image-2", false},
		{"AUTO uppercase", "AUTO", "", false},

		// 跳过：显式 gpt-image-1 / 1.5 不应用 SKILL 严格约束
		{"gpt-image-1.5 skips strict check", "1000x1000", "gpt-image-1.5", false},
		{"gpt-image-1 skips strict check", "999x999", "gpt-image-1", false},

		// gpt-image-2 / 空 model：合规
		{"square 1024 ok", "1024x1024", "gpt-image-2", false},
		{"landscape 1536x1024 ok", "1536x1024", "gpt-image-2", false},
		{"4K landscape ok", "3840x2160", "gpt-image-2", false},
		{"empty model treats as gpt-image-2 ok", "1024x1024", "", false},

		// 格式错
		{"malformed size", "1024", "gpt-image-2", true},
		{"non-numeric", "abcxdef", "gpt-image-2", true},

		// 边长超过 3840
		{"oversize edge", "4096x2304", "gpt-image-2", true},

		// 不是 16 的倍数
		{"width not multiple of 16", "1000x1024", "gpt-image-2", true},
		{"height not multiple of 16", "1024x1000", "gpt-image-2", true},

		// 比例 > 3:1
		{"ratio over 3 to 1", "3840x1024", "gpt-image-2", true}, // 3.75:1

		// 总像素低于 655360
		{"too few pixels", "512x512", "gpt-image-2", true}, // 262144

		// 总像素高于 8294400 —— 但同时也会触发"边长超 3840"或"比例超"
		// 单独构造一个仅超总像素而不超其它约束的 case 不可能（3840×3840=14745600，
		// 但比例 1:1 ✓、边长 3840 ✓、16 倍数 ✓，仅超总像素），用这个验证总像素分支
		{"too many pixels", "3840x3840", "gpt-image-2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateImageSize(tc.size, tc.model)
			if tc.wantErr && err == nil {
				t.Errorf("validateImageSize(%q, %q) = nil, want err", tc.size, tc.model)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateImageSize(%q, %q) = %v, want nil", tc.size, tc.model, err)
			}
		})
	}
}

// TestForwardImagesViaResponsesTool_InvalidSize 客户端在 gpt-image-2 上传非法 size，
// 应在 OAuth/PoW 链路启动前 400 返回，省一次配额。
func TestForwardImagesViaResponsesTool_InvalidSize(t *testing.T) {
	g := &OpenAIGateway{}
	w := httptest.NewRecorder()
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{"access_token": "tok"}},
		// size 1000 不是 16 倍数
		Body:    []byte(`{"prompt":"hi","n":1,"model":"gpt-image-2","size":"1000x1000"}`),
		Headers: http.Header{},
		Writer:  w,
	}
	outcome, err := g.forwardImagesViaResponsesTool(t.Context(), req)
	if err != nil {
		t.Fatalf("expected nil err for client-side issue, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Errorf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Errorf("Upstream.StatusCode = %d, want 400", outcome.Upstream.StatusCode)
	}
	if w.Body.Len() != 0 {
		t.Errorf("Core 处理 outcome 前不应提交 writer，实际写入 %q", w.Body.String())
	}
	if !strings.Contains(string(outcome.Upstream.Body), "16") {
		t.Errorf("error body should mention the 16-multiple constraint: %s", outcome.Upstream.Body)
	}
}

func TestForwardImagesViaResponsesTool_KeepAliveThenOverloadStillFailsOver(t *testing.T) {
	w := newSignalingResponseWriter()
	serverErr := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("upgrade websocket: %w", err)
			return
		}
		defer func() { _ = conn.Close() }()

		var msg json.RawMessage
		if err := conn.ReadJSON(&msg); err != nil {
			serverErr <- fmt.Errorf("read websocket request: %w", err)
			return
		}
		select {
		case heartbeat := <-w.writes:
			if got := string(heartbeat); got != responseStreamKeepAliveComment {
				serverErr <- fmt.Errorf("heartbeat = %q, want %q", got, responseStreamKeepAliveComment)
				return
			}
		case <-time.After(time.Second):
			serverErr <- errors.New("timed out waiting for image keepalive")
			return
		}

		failure := `{"type":"response.failed","response":{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(failure)); err != nil {
			serverErr <- fmt.Errorf("write websocket failure: %w", err)
			return
		}
		serverErr <- nil
	}))
	defer server.Close()

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"access_token":       "tok",
			"chatgpt_account_id": "acct-123",
		}},
		Model:  "gpt-image-2",
		Body:   []byte(`{"prompt":"draw a stable gateway","model":"gpt-image-2","size":"1024x1024"}`),
		Stream: true,
		Headers: http.Header{
			"X-Forwarded-Path":   []string{"/v1/images/generations"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"Content-Type":       []string{"application/json"},
		},
		Writer: w,
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	outcome, err := g.forwardImagesViaResponsesToolWithURL(t.Context(), req, wsURL, 5*time.Millisecond)
	if err == nil {
		t.Fatal("overload error = nil, want Core-facing error for failover")
	}
	if outcome.Kind != sdk.OutcomeUpstreamTransient || !outcome.Kind.ShouldFailover() {
		t.Fatalf("Kind = %v, want failover-eligible UpstreamTransient", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want 503", outcome.Upstream.StatusCode)
	}
	if outcome.RetryAfter != 5*time.Second {
		t.Fatalf("RetryAfter = %v, want 5s", outcome.RetryAfter)
	}
	if serverFailure := <-serverErr; serverFailure != nil {
		t.Fatal(serverFailure)
	}
	body := w.BodyString()
	if body == "" {
		t.Fatal("expected at least one keepalive comment")
	}
	if remainder := strings.ReplaceAll(body, responseStreamKeepAliveComment, ""); remainder != "" {
		t.Fatalf("failed attempt wrote business/error SSE after keepalive: %q", body)
	}
}

func TestForwardImagesViaResponsesTool_UsesWSForLargeEditImage(t *testing.T) {
	tinyResult := strings.TrimPrefix(testPNGDataURL(1, 1, func(x, y int) color.RGBA {
		return color.RGBA{R: 220, G: 40, B: 80, A: 255}
	}), "data:image/png;base64,")

	var gotAuth string
	var gotAccountID string
	var gotOpenAIBeta string
	var gotOriginator string
	var gotBody string

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("ChatGPT-Account-ID")
		gotOpenAIBeta = r.Header.Get("OpenAI-Beta")
		gotOriginator = r.Header.Get("originator")

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		var msg json.RawMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}
		gotBody = string(msg)

		write := func(payload string) {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Errorf("write websocket response: %v", err)
			}
		}

		write(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-1.5"}]}}`)
		write(fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"image_generation_call","id":"call_1","status":"completed","result":"%s","size":"1024x1024","quality":"medium","output_format":"png","model":"gpt-image-1.5"}}`, tinyResult))
		write(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.4","usage":{"input_tokens":12,"output_tokens":34},"tool_usage":{"image_gen":{"input_tokens":7,"output_tokens":9}},"output":[{"type":"image_generation_call","id":"call_1"}]}}`)
	}))
	defer server.Close()

	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	seed := uint32(1)
	for y := 0; y < 1600; y++ {
		for x := 0; x < 1600; x++ {
			seed = seed*1664525 + 1013904223
			img.SetRGBA(x, y, color.RGBA{R: uint8(seed >> 16), G: uint8(seed >> 8), B: uint8(seed), A: 255})
		}
	}
	imageRef := testPNGDataURLFromImage(img)
	body := []byte(fmt.Sprintf(`{"prompt":"edit this","image":%q}`, imageRef))

	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 1, Credentials: map[string]string{
			"access_token":       "tok",
			"chatgpt_account_id": "acct-123",
		}},
		Body: body,
		Headers: http.Header{
			"X-Forwarded-Path":   []string{"/v1/images/edits"},
			"X-Forwarded-Method": []string{http.MethodPost},
			"Content-Type":       []string{"application/json"},
			"originator":         []string{"codex_cli_rs"},
		},
		Writer: httptest.NewRecorder(),
	}

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	outcome, err := g.forwardImagesViaResponsesToolWithURL(t.Context(), req, wsURL)
	if err != nil {
		t.Fatalf("forwardImagesViaResponsesToolWithURL returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want Success", outcome.Kind)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if gotAccountID != "acct-123" {
		t.Fatalf("ChatGPT-Account-ID = %q, want acct-123", gotAccountID)
	}
	if gotOpenAIBeta != WSBetaHeader {
		t.Fatalf("OpenAI-Beta = %q, want %q", gotOpenAIBeta, WSBetaHeader)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Fatalf("originator = %q, want codex_cli_rs", gotOriginator)
	}
	if !strings.Contains(gotBody, `"input_image"`) {
		t.Fatalf("request body missing input_image: %s", gotBody)
	}
	if !strings.Contains(gotBody, "data:image/jpeg;base64,") {
		preview := gotBody
		if len(preview) > 256 {
			preview = preview[:256]
		}
		t.Fatalf("request body should contain compressed jpeg data URL: %s", preview)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage = nil, want non-nil")
	}
	if outcome.Usage.Model != "gpt-image-1.5" {
		t.Fatalf("Usage.Model = %q, want gpt-image-1.5", outcome.Usage.Model)
	}
}

// TestBuildImagesToolCreateMsg_Edit_GPTImage2_SkipsInputFidelity gpt-image-2 始终
// 用 high fidelity 处理输入图，input_fidelity 是 no-op，constraints 不应再追加。
// SKILL.md: "do not set input_fidelity with this model".
func TestBuildImagesToolCreateMsg_Edit_GPTImage2_SkipsInputFidelity(t *testing.T) {
	imageRef := testPNGDataURL(2, 2, func(x, y int) color.RGBA {
		return color.RGBA{R: 80, G: 90, B: 100, A: 255}
	})
	body := []byte(fmt.Sprintf(`{
		"model":"gpt-image-2",
		"prompt":"make it cyberpunk",
		"size":"1024x1024",
		"input_fidelity":"high",
		"image":%q
	}`, imageRef))
	msg, _, _, err := buildImagesToolCreateMsg(body, "application/json", true, openAISessionResolution{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	prompt := gjson.GetBytes(msg, "input.0.content.0.text").String()
	if strings.Contains(prompt, "fidelity") {
		t.Errorf("gpt-image-2 prompt should not mention fidelity, got: %q", prompt)
	}
	instructions := gjson.GetBytes(msg, "instructions").String()
	if strings.Contains(instructions, "fidelity") {
		t.Errorf("gpt-image-2 instructions should not mention fidelity, got: %q", instructions)
	}
	// 其它编辑约束不受影响，校验它们仍然存在
	if !strings.Contains(prompt, "Image 1 is the edit target") {
		t.Errorf("edit-target constraint missing: %q", prompt)
	}
}
