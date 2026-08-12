package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func accountRequiresResponsesImageTool(headers http.Header) bool {
	protocols := strings.Split(headers.Get("X-Airgate-Account-Image-Protocols"), ",")
	hasResponsesTool := false
	hasImagesAPI := false
	for _, protocol := range protocols {
		switch strings.ToLower(strings.TrimSpace(protocol)) {
		case "responses_tool":
			hasResponsesTool = true
		case "images_api":
			hasImagesAPI = true
		}
	}
	return hasResponsesTool && !hasImagesAPI
}

func (g *OpenAIGateway) forwardAPIKeyImagesViaResponsesTool(ctx context.Context, req *sdk.ForwardRequest, reqServiceTier string) (sdk.ForwardOutcome, error) {
	start := time.Now()
	account := req.Account
	logger := sdk.LoggerFromContext(ctx)
	_, reqPath := resolveAPIKeyRoute(req)
	isEdit := isImagesEditRequest(reqPath)
	contentType := req.Headers.Get("Content-Type")

	imgReq, err := parseImagesRequest(req.Body, contentType, isEdit)
	if err == nil {
		err = validateImageSize(imgReq.Size, imgReq.Model)
	}
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

	responsesBody, _, promptTokens, err := buildImagesToolResponsesBody(req.Body, contentType, isEdit, apiKeyImageToolChatModel(req.Model), openAISessionResolution{})
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

	if tier := firstNonEmptyTier(gjson.GetBytes(responsesBody, "service_tier").String(), reqServiceTier); tier != "" {
		responsesBody, _ = sjsonSetBytes(responsesBody, "service_tier", tier)
	}
	responsesBody, _ = sjsonSetBytes(responsesBody, "stream", false)

	targetURL := buildAPIKeyURL(account, "/v1/responses")
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(responsesBody))
	if err != nil {
		reason := fmt.Sprintf("构建上游 Responses 生图请求失败: %v", err)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	setAuthHeaders(upstreamReq, account)
	upstreamReq.Header.Set("Content-Type", "application/json")
	passHeadersForAccount(req.Headers, upstreamReq.Header, account)

	logger.Debug("upstream_request_start",
		sdk.LogFieldAccountID, account.ID,
		sdk.LogFieldModel, req.Model,
		"url", redactURL(targetURL),
		sdk.LogFieldMethod, http.MethodPost,
		"stream", false,
		"account_type", "apikey",
		"image_protocol", "responses_tool",
	)

	resp, err := g.buildHTTPClient(account).Do(upstreamReq)
	if err != nil {
		logger.Warn("upstream_request_failed",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			sdk.LogFieldError, err,
		)
		return upstreamTransportOutcome(ctx, err), fmt.Errorf("请求上游失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		reason := fmt.Sprintf("读取 Responses 生图响应失败: %v", readErr)
		return transientOutcome(reason), fmt.Errorf("%s", reason)
	}
	if resp.StatusCode >= 400 {
		errDetail := gjson.GetBytes(body, "error.message").String()
		if errDetail == "" {
			errDetail = truncate(string(body), 200)
		}
		outcome := failureOutcome(resp.StatusCode, body, resp.Header.Clone(), errDetail, extractRetryAfterHeader(resp.Header))
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	wsResult := imageGenCallsFromResponsesBody(body)
	if len(wsResult.ImageGenCalls) == 0 {
		reason := "Responses 生图响应中没有 completed image_generation_call.result"
		logger.Warn("responses_tool_image_result_empty",
			sdk.LogFieldAccountID, account.ID,
			sdk.LogFieldModel, req.Model,
			"body_len", len(body),
		)
		return sdk.ForwardOutcome{
			Kind:     sdk.OutcomeUpstreamTransient,
			Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
			Reason:   reason,
			Duration: time.Since(start),
		}, fmt.Errorf("%s", reason)
	}

	outputTokens := int(gjson.GetBytes(body, "tool_usage.image_gen.output_tokens").Int())
	if outputTokens == 0 {
		outputTokens = int(gjson.GetBytes(body, "response.tool_usage.image_gen.output_tokens").Int())
	}
	if outputTokens == 0 {
		outputTokens = estimateImageGenOutputTokens(wsResult.ImageGenCalls)
	}
	if wsResult.ToolImageModel == "" {
		wsResult.ToolImageModel = imageToolCostModel
	}
	restBody := buildImagesRESTResponse(wsResult, promptTokens, outputTokens, imageGenerationBillingModel(wsResult.ToolImageModel, req.Model))
	mockResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(restBody)),
	}
	return g.handleImagesResponse(mockResp, req.Writer, nil, start, req.Model, imgReq.Size)
}

func apiKeyImageToolChatModel(requestModel string) string {
	model := strings.TrimSpace(requestModel)
	if model == "" || strings.Contains(strings.ToLower(model), "image") {
		return imagesOAuthChatModel
	}
	return model
}

func imageGenCallsFromResponsesBody(body []byte) WSResult {
	var out WSResult
	for _, path := range []string{"tools", "response.tools"} {
		for _, tool := range gjson.GetBytes(body, path).Array() {
			if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
				out.ToolImageModel = tool.Get("model").String()
				break
			}
		}
		if out.ToolImageModel != "" {
			break
		}
	}
	for _, path := range []string{"output", "response.output"} {
		for _, item := range gjson.GetBytes(body, path).Array() {
			if call, ok := imageGenCallFromJSON(item); ok {
				out.ImageGenCalls = append(out.ImageGenCalls, call)
			}
		}
	}
	return out
}

func imageGenCallFromJSON(item gjson.Result) (ImageGenCall, bool) {
	if !strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "image_generation_call") {
		return ImageGenCall{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(item.Get("status").String()), "completed") {
		return ImageGenCall{}, false
	}
	result := item.Get("result").String()
	if result == "" {
		return ImageGenCall{}, false
	}
	return ImageGenCall{
		ID:            item.Get("id").String(),
		Status:        "completed",
		Result:        result,
		Size:          item.Get("size").String(),
		Quality:       item.Get("quality").String(),
		OutputFormat:  item.Get("output_format").String(),
		Model:         item.Get("model").String(),
		RevisedPrompt: item.Get("revised_prompt").String(),
	}, true
}

func sjsonSetBytes(body []byte, path string, value any) ([]byte, error) {
	return sjson.SetBytes(body, path, value)
}
