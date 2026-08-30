package gateway

// Gemini 生图模型的 chat completions「无图重试」守卫。
//
// 背景(2026-08-29/30 生产实测):chat 透传路径上游概率性只回文本不出图
// (「好的,这是咖啡杯产品图。」然后什么都没有,3 次里 2 次复现)。客户端点名
// 生图模型就是要图,无图的 200 是一次失败的生成——Images API 桥接路径早有
// 「上游响应中未包含可用图片」守卫,chat 透传此前没有。
//
// 做法:强制上游非流式,最多 geminiImageChatMaxAttempts 次;拿到图立即交付,
// 全部只回文本时把最后一次响应原样交付(客户至少看到模型说了什么)。客户端
// 要流式时交付期合成 SSE(整段单 delta)。计费与透传同源:handleNonStreamResponse。
// 上游 HTTP 错误不在此重试——那是 core failover 的职责,原样归类返回。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const geminiImageChatMaxAttempts = 3

func isGeminiImageChatRequest(req *sdk.ForwardRequest, reqMethod, reqPath string) bool {
	if req == nil || reqMethod != http.MethodPost || !isChatCompletionsPath(reqPath) {
		return false
	}
	if usesGeminiImagesAPI(req.Account) {
		return false
	}
	return isGeminiImageModel(firstNonEmptyString(req.Model, gjson.GetBytes(req.Body, "model").String()))
}

func (g *OpenAIGateway) forwardAPIKeyGeminiImageChat(ctx context.Context, req *sdk.ForwardRequest, reqServiceTier string, start time.Time) (sdk.ForwardOutcome, error) {
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	model := firstNonEmptyString(req.Model, gjson.GetBytes(req.Body, "model").String())

	clientStream := req.Stream || gjson.GetBytes(req.Body, "stream").Bool()
	upstreamBody := normalizeGeminiVideoParts(req.Body)
	if clientStream {
		// 上游强制非流式才能在无图时重试;客户端的流式在交付期合成。
		upstreamBody, _ = sjson.SetBytes(upstreamBody, "stream", false)
		upstreamBody, _ = sjson.DeleteBytes(upstreamBody, "stream_options")
	}
	targetURL := buildAPIKeyURL(account, "/v1/chat/completions")

	var body []byte
	var respHeader http.Header
	for attempt := 1; attempt <= geminiImageChatMaxAttempts; attempt++ {
		upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
		if err != nil {
			reason := fmt.Sprintf("构建上游请求失败: %v", err)
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}
		setAuthHeaders(upstreamReq, account)
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Accept", "application/json")
		passHeadersForAccount(req.Headers, upstreamReq.Header, account)

		resp, cancel, err := g.doStreamableUpstream(ctx, upstreamReq, account, false)
		if err != nil {
			return upstreamTransportOutcome(ctx, err), fmt.Errorf("请求上游失败: %w", err)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		respHeader = resp.Header.Clone()
		statusCode := resp.StatusCode
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			reason := fmt.Sprintf("读取上游响应失败: %v", readErr)
			return transientOutcome(reason), fmt.Errorf("%s", reason)
		}
		if statusCode >= 400 {
			errDetail := gjson.GetBytes(respBody, "error.message").String()
			if errDetail == "" {
				errDetail = truncate(string(respBody), 200)
			}
			outcome := failureOutcome(statusCode, respBody, respHeader, errDetail, extractRetryAfterHeader(respHeader))
			outcome.Duration = time.Since(start)
			return outcome, nil
		}
		body = respBody
		if chatBodyHasImage(body) {
			break
		}
		logger.Warn("gemini_image_chat_text_only",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, model,
			"attempt", attempt,
			"max_attempts", geminiImageChatMaxAttempts,
		)
	}

	mockResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     respHeader,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	if !clientStream {
		return handleNonStreamResponse(mockResp, req.Writer, start, reqServiceTier)
	}
	outcome, err := handleNonStreamResponse(mockResp, nil, start, reqServiceTier)
	if err != nil {
		return outcome, err
	}
	if req.Writer != nil {
		writeGeminiImageChatSSE(req.Writer, body)
	}
	outcome.Upstream = sdk.UpstreamResponse{StatusCode: http.StatusOK}
	return outcome, nil
}

// chatBodyHasImage 判定 chat 响应里是否真的带了图:内嵌 data URL,或
// message.images 数组(部分中继把图放独立字段)。
func chatBodyHasImage(body []byte) bool {
	if bytes.Contains(body, []byte("data:image/")) {
		return true
	}
	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		if len(choice.Get("message.images").Array()) > 0 {
			return true
		}
	}
	return false
}

// writeGeminiImageChatSSE 把完整 chat 响应合成为 OpenAI SSE。content 通常是
// 携带兆级 base64 data URL 的 markdown,整段一个 delta,不切片。
func writeGeminiImageChatSSE(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	id := gjson.GetBytes(body, "id").String()
	if id == "" {
		id = "chatcmpl-gemini-image"
	}
	model := gjson.GetBytes(body, "model").String()
	created := gjson.GetBytes(body, "created").Int()
	if created == 0 {
		created = time.Now().Unix()
	}
	writeChunk := func(deltaJSON string, finish string, usageRaw string) {
		chunk, _ := sjson.Set(`{}`, "id", id)
		chunk, _ = sjson.Set(chunk, "object", "chat.completion.chunk")
		chunk, _ = sjson.Set(chunk, "created", created)
		chunk, _ = sjson.Set(chunk, "model", model)
		chunk, _ = sjson.SetRaw(chunk, "choices.0.delta", deltaJSON)
		chunk, _ = sjson.Set(chunk, "choices.0.index", 0)
		if finish != "" {
			chunk, _ = sjson.Set(chunk, "choices.0.finish_reason", finish)
		} else {
			chunk, _ = sjson.SetRaw(chunk, "choices.0.finish_reason", "null")
		}
		if usageRaw != "" {
			chunk, _ = sjson.SetRaw(chunk, "usage", usageRaw)
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		flush()
	}

	writeChunk(`{"role":"assistant"}`, "", "")

	message := gjson.GetBytes(body, "choices.0.message")
	delta := `{}`
	if content := message.Get("content"); content.Exists() {
		delta, _ = sjson.SetRaw(delta, "content", content.Raw)
	}
	if images := message.Get("images"); images.IsArray() && len(images.Array()) > 0 {
		delta, _ = sjson.SetRaw(delta, "images", images.Raw)
	}
	writeChunk(delta, "", "")

	finish := gjson.GetBytes(body, "choices.0.finish_reason").String()
	if finish == "" {
		finish = "stop"
	}
	usageRaw := ""
	if usage := gjson.GetBytes(body, "usage"); usage.Exists() {
		usageRaw = usage.Raw
	}
	writeChunk(`{}`, finish, usageRaw)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}
