package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────────────────────────────
// Responses API 有状态会话（previous_response_id / store）
//
// 产品决定（2026-09-18）：**上游支持的能力，我们就支持，不在网关里替客户阉割。**
//
// 改动前这里是两步无条件处理：剔掉客户端的 `previous_response_id`、强制 `store=false`。
// 后果是客户发了 previous_response_id 拿到 200，却**完全没有记忆**，没有任何提示——
// 这是最难自查的一类故障（付费客户走 /v1/responses 正在受影响）。
//
// 当初剔除的理由成立：多账号负载均衡时 response id 属于某个具体账号，换号就 not found。
// 但正确的解法是**把会话钉回原账号**，而不是把能力一刀切掉。钉的机制在 core：
// 插件拿到上游 response id 后经 Host `scheduler.bind_response_account` 登记，
// core 下一轮按 previous_response_id 把请求钉回同一账号，钉不住就明确报错。
//
// ## 为什么按账号开，而不是一把全放开
//
// 不是所有 OpenAI 协议上游都真的支持这两项：真正的 OpenAI 官方支持，火山方舟支持
// （官方参数表含 previous_response_id，`store` 默认 true、expire_at 默认 3 天最长 7 天），
// 但各类中继行为不一——有的把未知字段直接 400，有的收下但不存。全放开等于拿其它通道的
// 稳定性去赌（2026-09-17 火山字段白名单那次的教训就是「一个字段不兼容 = 持续负毛利」）。
//
// 所以作用域与 `responses_field_filter` 同构：默认 auto = base_url 命中火山方舟域名时
// 启用，其余账号**保持改动前的行为**（继续剥 previous_response_id、继续强制 store=false），
// 明确支持的上游由运维用凭证逐个放行。
//
// ## store 的取舍
//
// 放行后不再强制 `store=false`：客户端传什么就是什么，没传就尊重上游默认（火山为 true）。
// 代价是响应会落在上游存储里（火山默认 3 天）——这是已知并被接受的产品取舍，
// 换回来的是有状态会话与显式前缀缓存（火山的 `caching` 要求 store=true，
// 而缓存命中价只有输入价的 1/50）。
// ──────────────────────────────────────────────────────────────────────────────

// responsesSessionPassthroughCredential 账号凭证键：是否放行 Responses 有状态会话。
//
//	""/"auto" → 自动：base_url 指向火山方舟时放行（默认）
//	"on"      → 强制放行（其它已验证支持的上游，如 OpenAI 官方）
//	"off"     → 强制关闭（保持剔除 previous_response_id + store=false）
const responsesSessionPassthroughCredential = "responses_session_passthrough"

// responsesSessionPassthroughEnabled 判断本账号是否放行有状态会话字段。
//
// auto 复用 isVolcengineArkAccount（按 base_url 主机名判定，与字段白名单同一套口径），
// 不另起一份域名表——两者要么同时认一个上游是方舟，要么同时不认。
func responsesSessionPassthroughEnabled(account *sdk.Account) bool {
	switch strings.ToLower(strings.TrimSpace(accountCredential(account, responsesSessionPassthroughCredential))) {
	case "off", "false", "0", "none":
		return false
	case "on", "true", "1", "ark", "volcengine":
		return true
	default: // "" / "auto"
		return isVolcengineArkAccount(account)
	}
}

// responsesSessionFieldsForAccount 描述本次请求对两个有状态字段的处理方式。
type responsesSessionFields struct {
	// passthrough 为 true 时透传客户端的 previous_response_id / store。
	passthrough bool
	// storeDisabled 为 true 表示本轮响应不会被上游保留（客户端显式 store=false，
	// 或我们仍在强制 store=false），因此不值得登记会话绑定。
	storeDisabled bool
}

func responsesSessionFieldsForAccount(account *sdk.Account, body []byte, reqPath string) responsesSessionFields {
	if !isResponsesRequestPath(reqPath) {
		return responsesSessionFields{storeDisabled: true}
	}
	if !responsesSessionPassthroughEnabled(account) {
		return responsesSessionFields{storeDisabled: true}
	}
	fields := responsesSessionFields{passthrough: true}
	if store := gjson.GetBytes(body, "store"); store.Exists() && !store.Bool() {
		fields.storeDisabled = true
	}
	return fields
}

// responseIDFromResponsesJSON 从 Responses API 的非流式响应体里取 response id。
func responseIDFromResponsesJSON(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
		return id
	}
	return strings.TrimSpace(gjson.GetBytes(body, "response.id").String())
}

// responseIDFromSSEData 从单个 SSE 事件里取 response id（response.created 就已经带）。
func responseIDFromSSEData(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(data, "response.id").String()); id != "" {
		return id
	}
	return ""
}

// bindResponseAffinityTimeout 登记绑定的独立超时。
// 绑定要在客户端断开之后仍然写得进去（流式请求结束时 ctx 往往已经取消），
// 但它只是一次本地 gRPC，卡住不该拖累请求收尾。
const bindResponseAffinityTimeout = 3 * time.Second

// bindResponseAffinity 把「这条 response 由本账号产出」登记给 core。
//
// 失败只记日志、不影响本次响应：绑定丢了最坏是下一轮钉不住 → core 明确报错让客户重开会话，
// 而不是静默丢上下文。core 版本尚未支持该方法时同理（Unimplemented 会走到这里）。
func (g *OpenAIGateway) bindResponseAffinity(ctx context.Context, req *sdk.ForwardRequest, responseID string) {
	if g == nil || g.host == nil || req == nil || req.Account == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	userID := airgateUserIDFromHeaders(req.Headers)
	if userID <= 0 {
		return
	}
	logger := sdk.LoggerFromContext(ctx)
	// 客户端断开不该让绑定写不进去：响应已经产出，下一轮仍可能续聊。
	bindCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindResponseAffinityTimeout)
	defer cancel()
	if _, err := g.hostInvoke(bindCtx, hostMethodSchedulerBindResponse, map[string]interface{}{
		"user_id":     userID,
		"account_id":  req.Account.ID,
		"response_id": responseID,
	}); err != nil {
		logger.Warn("responses_session_bind_failed",
			sdk.LogFieldAccountID, req.Account.ID,
			sdk.LogFieldError, err,
		)
		return
	}
	logger.Debug("responses_session_bound",
		sdk.LogFieldAccountID, req.Account.ID,
	)
}

// airgateUserIDFromHeaders 读 core 注入的付费身份。
// core 的 buildHeaders 用 Set 覆盖同名头，客户端伪造不进来。
func airgateUserIDFromHeaders(headers http.Header) int64 {
	if headers == nil {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-Airgate-User-ID")), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
