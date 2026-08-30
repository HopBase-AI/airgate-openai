package gateway

// chat completions 的协议预校验:messages 缺失/为空是客户端请求错误,在网关层
// 直接 400,不转发上游。
//
// 背景(2026-08-30 三渠道协议保真度矩阵):官方口径(腾讯线实证)对空 messages 回
// 400002;但张总自部署回 500——会被判 upstream transient 烧整条 failover 链并
// 污染账号健康度;inference.ai 这类参数错误则一律伪装 503。与其逐家纠偏,
// 不如在自己门口把这类确定性客户端错误掐掉。
//
// 只校验「messages 键缺失或为空数组」这一无争议事实,不做更深的形态校验——
// 消息内容的合法性仍由上游权威判定(契约可选性红线:别替官方收紧)。

import (
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func rejectInvalidChatMessages(req *sdk.ForwardRequest, method, path string, start time.Time) (sdk.ForwardOutcome, bool) {
	if req == nil || method != http.MethodPost || !isChatCompletionsPath(path) {
		return sdk.ForwardOutcome{}, false
	}
	if len(req.Body) == 0 || !gjson.ValidBytes(req.Body) {
		// 空体/非 JSON 保持原行为交上游判定,不在本守卫扩权。
		return sdk.ForwardOutcome{}, false
	}
	messages := gjson.GetBytes(req.Body, "messages")
	if messages.IsArray() && len(messages.Array()) > 0 {
		return sdk.ForwardOutcome{}, false
	}
	reason := "messages 不能为空数组"
	if !messages.Exists() {
		reason = "缺少 messages 字段"
	}
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeClientError,
		Upstream: sdk.UpstreamResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       jsonError(reason),
		},
		Reason:   reason,
		Duration: time.Since(start),
	}, true
}
