package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

// MiniMax canvas-20 异步图片任务契约（对方提供的未公开文档，2026-08-21）：
// 提交请求与同步 Images API 完全一致，仅追加 X-Async: true 请求头；服务同步完成
// 鉴权/限流/参数校验/Prompt 安检后返回顶层 task_id，实际生成在后台执行。
// 查询 GET {base}/v1/content/images/tasks/{task_id}：
//   - queued / in_progress：HTTP 200 + status 字段
//   - completed：直接返回与同步接口一致的响应体（含 data[].b64_json 与 usage），
//     不再携带 status
//   - failed：直接返回同步错误体与对应 HTTP 状态码，不携带 status；失败不计费
// 终态判断必须按契约顺序（不能只看 status）：非 2xx 或 base_resp.status_code!=0
// 为失败；有 status 继续轮询；有 data 且 base_resp.status_code==0 为完成。
// 提交接口没有幂等键：查询失败只能重试查询，绝不能重新提交（会创建并计费多个任务）。
// 终态保留 24 小时。

const (
	// imagesAsyncCredential 账号级开关：填 true / 1 时图像请求走 X-Async 提交 +
	// 任务轮询（MiniMax canvas-20 契约）。
	imagesAsyncCredential = "images_async"
	// miniMaxImagesTasksPath 任务查询路径（相对 base_url，不含末尾斜杠）。
	miniMaxImagesTasksPath = "/v1/content/images/tasks"
	// miniMaxAsyncTimeoutSeconds 提交时携带的 X-Timeout。服务端允许 300~900，
	// 与我们生图转发 300s 硬超时对齐：我们放弃时上游也尽快转 failed（不计费）。
	miniMaxAsyncTimeoutSeconds = 300

	miniMaxPollMaxTransportRetry = 8
	miniMaxPollMaxUnknown        = 3
)

// 轮询节奏（契约建议 2~5 秒 + 退避）。var 以便测试缩短。
var (
	miniMaxPollInitialDelay = 2 * time.Second
	miniMaxPollMaxInterval  = 5 * time.Second
)

// imagesAsyncEnabled 账号是否启用图像异步任务模式。
func imagesAsyncEnabled(account *sdk.Account) bool {
	switch strings.ToLower(accountCredential(account, imagesAsyncCredential)) {
	case "true", "1":
		return true
	}
	return false
}

// miniMaxAsyncTaskID 识别 X-Async 提交响应：顶层 task_id 且不含图片数据。
// 仅在 imagesAsyncEnabled 的账号上调用，避免把别家同步响应误判成任务。
func miniMaxAsyncTaskID(body []byte) (string, bool) {
	if gjson.GetBytes(body, "data").Exists() {
		return "", false
	}
	tid := strings.TrimSpace(gjson.GetBytes(body, "task_id").String())
	if tid == "" {
		return "", false
	}
	return tid, true
}

// asyncImageTaskFailedError 任务终态失败：携带上游错误体与 HTTP 状态码，
// 由 forward 层按同步失败同一套 failureOutcome 分类（400 判客户端错等），
// 避免被笼统当 transient 重试——重新提交会创建并计费新任务。
type asyncImageTaskFailedError struct {
	StatusCode int
	Body       []byte
}

func (e *asyncImageTaskFailedError) Error() string {
	return fmt.Sprintf("异步图片任务失败: HTTP %d: %s", e.StatusCode, truncate(string(e.Body), 300))
}

// pollMiniMaxImageTask 轮询 MiniMax 图片任务直到终态或 ctx 结束。
// 完成时返回与同步接口一致的响应体（直接交 handleImagesResponse 计费与回包）。
func (g *OpenAIGateway) pollMiniMaxImageTask(
	ctx context.Context,
	account *sdk.Account,
	taskID string,
	logger *slog.Logger,
) ([]byte, error) {
	baseURL := strings.TrimRight(account.Credentials["base_url"], "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	pollURL := baseURL + miniMaxImagesTasksPath + "/" + taskID
	client := g.buildHTTPClient(account)

	logger.Debug("images_minimax_task_poll_start", "task_id", taskID)

	interval := miniMaxPollInitialDelay
	transportFailures := 0
	unknownResponses := 0

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(miniMaxPollInitialDelay):
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pollReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, fmt.Errorf("构建轮询请求失败: %w", err)
		}
		setAuthHeaders(pollReq, account)

		pollResp, err := client.Do(pollReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			transportFailures++
			logger.Warn("images_minimax_task_poll_error",
				"task_id", taskID, "failures", transportFailures, sdk.LogFieldError, err)
			if transportFailures > miniMaxPollMaxTransportRetry {
				return nil, fmt.Errorf("异步任务查询连续失败: %w", err)
			}
			if !sleepCtx(ctx, interval) {
				return nil, ctx.Err()
			}
			interval = min(interval+time.Second, miniMaxPollMaxInterval)
			continue
		}
		body, _ := io.ReadAll(pollResp.Body)
		_ = pollResp.Body.Close()
		transportFailures = 0

		baseRespCode := gjson.GetBytes(body, "base_resp.status_code")
		traceID := gjson.GetBytes(body, "trace_id").String()

		// 契约顺序 1：非 2xx，或 base_resp.status_code != 0 → 任务失败/查询失败。
		if pollResp.StatusCode >= 400 || (baseRespCode.Exists() && baseRespCode.Int() != 0) {
			// 查询端点自身的 5xx 抖动（无 base_resp 结构）按瞬态重试，不当任务终态。
			if pollResp.StatusCode >= 500 && !baseRespCode.Exists() {
				transportFailures++
				logger.Warn("images_minimax_task_poll_http_error",
					"task_id", taskID, "status", pollResp.StatusCode, "failures", transportFailures)
				if transportFailures > miniMaxPollMaxTransportRetry {
					return nil, fmt.Errorf("异步任务查询连续 HTTP %d", pollResp.StatusCode)
				}
				if !sleepCtx(ctx, interval) {
					return nil, ctx.Err()
				}
				interval = min(interval+time.Second, miniMaxPollMaxInterval)
				continue
			}
			statusCode := pollResp.StatusCode
			if statusCode < 400 {
				if c := int(baseRespCode.Int()); c >= 400 && c < 600 {
					statusCode = c
				} else {
					statusCode = http.StatusBadGateway
				}
			}
			logger.Warn("images_minimax_task_failed",
				"task_id", taskID, "status", statusCode, "trace_id", traceID,
				sdk.LogFieldReason, truncate(string(body), 300))
			return nil, &asyncImageTaskFailedError{StatusCode: statusCode, Body: body}
		}

		// 契约顺序 2：status=queued/in_progress → 继续轮询。
		switch gjson.GetBytes(body, "status").String() {
		case "queued", "in_progress":
			unknownResponses = 0
			logger.Debug("images_minimax_task_poll_status",
				"task_id", taskID, "status", gjson.GetBytes(body, "status").String())
			if !sleepCtx(ctx, interval) {
				return nil, ctx.Err()
			}
			interval = min(interval+time.Second, miniMaxPollMaxInterval)
			continue
		}

		// 契约顺序 3：有 data 且 base_resp.status_code==0 → 完成。
		if gjson.GetBytes(body, "data").Exists() {
			logger.Info("images_minimax_task_completed", "task_id", taskID, "trace_id", traceID)
			return body, nil
		}

		// 契约顺序 4：未知响应 → 记录 trace_id，连续多次后放弃。
		unknownResponses++
		logger.Warn("images_minimax_task_poll_unknown",
			"task_id", taskID, "trace_id", traceID, "count", unknownResponses,
			sdk.LogFieldReason, truncate(string(body), 300))
		if unknownResponses >= miniMaxPollMaxUnknown {
			return nil, fmt.Errorf("异步任务返回未知响应(trace_id=%s)", traceID)
		}
		if !sleepCtx(ctx, interval) {
			return nil, ctx.Err()
		}
		interval = min(interval+time.Second, miniMaxPollMaxInterval)
	}
}

// sleepCtx 可被 ctx 打断的等待；被打断时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
