package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

var markdownDataImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((data:image/[^)]+)\)`)

const (
	geminiImageProtocolCredential = "gemini_image_protocol"
	geminiImageProtocolImagesAPI  = "images_api"
)

func usesGeminiImagesAPI(account *sdk.Account) bool {
	return strings.EqualFold(
		accountCredential(account, geminiImageProtocolCredential),
		geminiImageProtocolImagesAPI,
	)
}

func isGeminiImageChatCompletionsRequest(req *sdk.ForwardRequest, reqPath string) bool {
	if req == nil || !strings.HasSuffix(reqPath, "/chat/completions") {
		return false
	}
	return isGeminiImageModel(req.Model) || isGeminiImageModel(gjson.GetBytes(req.Body, "model").String())
}

func (g *OpenAIGateway) forwardAPIKeyGeminiImageViaChat(ctx context.Context, req *sdk.ForwardRequest, imgReq *imagesRequest, start time.Time) (sdk.ForwardOutcome, error) {
	if imgReq == nil {
		_, reqPath := resolveAPIKeyRoute(req)
		parsed, err := parseImagesRequest(req.Body, req.Headers.Get("Content-Type"), isImagesEditRequest(reqPath))
		if err != nil {
			body := jsonError(err.Error())
			return sdk.ForwardOutcome{
				Kind: sdk.OutcomeClientError,
				Upstream: sdk.UpstreamResponse{
					StatusCode: http.StatusBadRequest,
					Headers:    http.Header{"Content-Type": []string{"application/json"}},
					Body:       body,
				},
				Reason:   err.Error(),
				Duration: time.Since(start),
			}, nil
		}
		imgReq = parsed
	}
	modelName := firstNonEmptyString(imgReq.Model, req.Model)
	chatBody, err := buildGeminiImageChatRequestBody(modelName, imgReq)
	if err != nil {
		body := jsonError(err.Error())
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeClientError,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       body,
			},
			Reason:   err.Error(),
			Duration: time.Since(start),
		}, nil
	}

	account := req.Account
	targetURL := buildAPIKeyURL(account, "/v1/chat/completions")
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(chatBody))
	if err != nil {
		reason := fmt.Sprintf("构建上游请求失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	setAuthHeaders(upstreamReq, account)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")
	passHeadersForAccount(req.Headers, upstreamReq.Header, account)

	logger := sdk.LoggerFromContext(ctx)
	logger.Debug("gemini_image_chat_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, modelName,
		"url", redactURL(targetURL),
	)
	resp, cancel, err := g.doStreamableUpstream(ctx, upstreamReq, account, false)
	if err != nil {
		logger.Warn("gemini_image_chat_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, modelName,
			sdk.LogFieldError, err,
		)
		return upstreamTransportOutcome(ctx, err), fmt.Errorf("请求上游失败: %w", err)
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		reason := fmt.Sprintf("读取 Gemini 图片响应失败: %v", readErr)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	if resp.StatusCode >= 400 {
		errDetail := gjson.GetBytes(body, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(body), 200)
		}
		logger.Warn("gemini_image_chat_non_2xx",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, modelName,
			sdk.LogFieldStatus, resp.StatusCode,
			sdk.LogFieldReason, errDetail,
		)
		outcome := failureOutcome(resp.StatusCode, body, resp.Header.Clone(), errDetail, extractRetryAfterHeader(resp.Header))
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	imagesBody, convErr := convertGeminiImageChatResponseToImages(body, modelName, imgReq)
	if convErr != nil {
		reason := "上游响应中未包含可用图片: " + convErr.Error()
		logger.Warn("gemini_image_chat_convert_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, modelName,
			"body_len", len(body),
			sdk.LogFieldError, convErr,
		)
		errBody := buildImagesErrorBody(http.StatusBadGateway, reason)
		return sdk.ForwardOutcome{
			Kind: sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{
				StatusCode: http.StatusBadGateway,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       errBody,
			},
			Reason:   reason,
			Duration: time.Since(start),
		}, fmt.Errorf("%s", reason)
	}

	mockResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(imagesBody)),
	}
	return g.handleImagesResponse(mockResp, req.Writer, nil, start, modelName, imgReq)
}

func buildGeminiImageChatRequestBody(modelName string, req *imagesRequest) ([]byte, error) {
	if strings.TrimSpace(modelName) == "" {
		return nil, fmt.Errorf("model 不能为空")
	}
	if req != nil && strings.TrimSpace(req.Mask) != "" {
		return nil, fmt.Errorf("暂不支持 mask 蒙版（Gemini 图片模型），请去掉 mask，用文字描述要修改的区域")
	}
	prompt := buildGeminiImageChatPrompt(req)
	var content any = prompt
	if req != nil && len(req.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": prompt}}
		for _, ref := range req.Images {
			dataURL, err := imageRefToDataURL(ref)
			if err != nil {
				return nil, err
			}
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": dataURL},
			})
		}
		content = parts
	}
	payload := map[string]any{
		"model": modelName,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": content,
			},
		},
		"modalities": []string{"text", "image"},
		"response_format": map[string]any{
			"type": "image",
		},
	}
	if imageConfig := geminiImageChatConfig(req); imageConfig != nil {
		payload["extra_body"] = map[string]any{
			"google": map[string]any{
				"image_config": imageConfig,
			},
		}
	}
	return json.Marshal(payload)
}

func geminiImageChatConfig(req *imagesRequest) map[string]string {
	if req == nil {
		return nil
	}
	size := strings.ToLower(strings.TrimSpace(req.Size))
	var aspectRatio, imageSize string
	switch size {
	case "1024x1024":
		aspectRatio, imageSize = "1:1", "1K"
	case "1536x1024":
		aspectRatio, imageSize = "3:2", "1K"
	case "1024x1536":
		aspectRatio, imageSize = "2:3", "1K"
	case "2048x2048":
		aspectRatio, imageSize = "1:1", "2K"
	case "2048x1152":
		aspectRatio, imageSize = "16:9", "2K"
	case "1152x2048":
		aspectRatio, imageSize = "9:16", "2K"
	case "3840x2160":
		aspectRatio, imageSize = "16:9", "4K"
	case "2160x3840":
		aspectRatio, imageSize = "9:16", "4K"
	default:
		return nil
	}
	return map[string]string{
		"aspect_ratio": aspectRatio,
		"image_size":   imageSize,
	}
}

// imageRefToDataURL 把参考图（data URL 或 http(s) URL）统一读成压缩后的 data URL，
// 供 chat 桥接以 image_url 分段携带；上游按 Gemini inline 图片消费。
func imageRefToDataURL(ref string) (string, error) {
	mimeType, data, err := readImageRefBytes(ref, maxEditInputImageBytes)
	if err != nil {
		return "", err
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func buildGeminiImageChatPrompt(req *imagesRequest) string {
	if req == nil {
		return ""
	}
	lines := []string{req.Prompt}
	if size := strings.TrimSpace(req.Size); size != "" && !strings.EqualFold(size, "auto") {
		lines = append(lines, "Generate the image at "+size+" pixels.")
	}
	return strings.Join(lines, "\n\n")
}

func convertGeminiImageChatResponseToImages(body []byte, modelName string, req *imagesRequest) ([]byte, error) {
	dataURLs := extractGeminiChatDataImageURLs(body)
	if len(dataURLs) == 0 {
		return nil, fmt.Errorf("chat completion response has no data:image result")
	}
	data := make([]map[string]any, 0, len(dataURLs))
	for _, ref := range dataURLs {
		mimeType, dataBytes, err := decodeDataImageURL(ref)
		if err != nil {
			return nil, err
		}
		item := map[string]any{
			"b64_json": base64.StdEncoding.EncodeToString(dataBytes),
		}
		if mimeType != "" {
			item["mime_type"] = mimeType
		}
		if sz, ok := imageActualSizeFromBase64(item["b64_json"].(string)); ok {
			item["size"] = sz
		}
		data = append(data, item)
	}
	promptTokens := 0
	outputTokens := 0
	usage := gjson.GetBytes(body, "usage")
	if usage.Exists() {
		promptTokens = int(firstNonZeroInt64(
			usage.Get("prompt_tokens").Int(),
			usage.Get("input_tokens").Int(),
		))
		outputTokens = int(firstNonZeroInt64(
			usage.Get("completion_tokens").Int(),
			usage.Get("output_tokens").Int(),
		))
	}
	if promptTokens <= 0 && req != nil {
		promptTokens = estimatePromptTokens(req.Prompt)
	}
	if outputTokens <= 0 && req != nil {
		outputTokens = len(data) * lookupImageGenOutputTokens(req.Size, req.Quality)
	}
	payload := map[string]any{
		"created": time.Now().Unix(),
		"model":   modelName,
		"data":    data,
	}
	if promptTokens+outputTokens > 0 {
		payload["usage"] = map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": outputTokens,
			"total_tokens":  promptTokens + outputTokens,
			"input_tokens_details": map[string]any{
				"text_tokens":  promptTokens,
				"image_tokens": 0,
			},
		}
	}
	return json.Marshal(payload)
}

func extractGeminiChatDataImageURLs(body []byte) []string {
	var refs []string
	content := gjson.GetBytes(body, "choices.0.message.content")
	switch {
	case content.IsArray():
		for _, part := range content.Array() {
			if ref := dataImageURLFromChatPart(part); ref != "" {
				refs = append(refs, ref)
			}
		}
	case content.Type == gjson.String:
		refs = append(refs, extractMarkdownDataImageURLs(content.String())...)
	}
	return dedupeStrings(refs)
}

func dataImageURLFromChatPart(part gjson.Result) string {
	for _, path := range []string{
		"image_url.url",
		"input_image.image_url",
		"data",
		"url",
	} {
		if ref := strings.TrimSpace(part.Get(path).String()); strings.HasPrefix(ref, "data:image/") {
			return ref
		}
	}
	text := strings.TrimSpace(firstNonEmptyString(part.Get("text").String(), part.Get("content").String()))
	if text != "" {
		urls := extractMarkdownDataImageURLs(text)
		if len(urls) > 0 {
			return urls[0]
		}
	}
	return ""
}

func extractMarkdownDataImageURLs(text string) []string {
	var refs []string
	for _, match := range markdownDataImagePattern.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 && strings.HasPrefix(match[1], "data:image/") {
			refs = append(refs, match[1])
		}
	}
	return refs
}

func dedupeStrings(values []string) []string {
	if len(values) <= 1 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func firstNonZeroInt64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
