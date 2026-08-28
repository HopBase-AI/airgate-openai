package gateway

import (
	"encoding/json"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/tidwall/sjson"
)

// chatModelMapCredential 账号级「公开模型名 → 上游模型 ID」映射。
//
// 背景：同一个模型在不同上游的 ID 未必一致。例如 DeepSeek V4 在腾讯 TokenHub 上
// 就叫 deepseek-v4-pro-202606（与我方公开名相同），在火山方舟上却叫
// deepseek-v4-pro-ga-260813。若不做映射，换上游就得改公开名，会让客户端全部 404。
//
// 取值为 JSON 对象，键=我方公开模型名，值=该账号上游的真实模型 ID：
//
//	{"deepseek-v4-pro-202606":"deepseek-v4-pro-ga-260813"}
//
// 仅重写发往上游的请求体；计费、用量与对外展示一律仍用公开名，
// 与 gpt_image_2_upstream_model 的处理方式一致。
const chatModelMapCredential = "chat_model_map"

// chatUpstreamModelForAccount 返回该账号下公开模型对应的上游模型 ID。
// 未配置映射、JSON 非法或未命中时返回空串，调用方据此跳过重写。
func chatUpstreamModelForAccount(account *sdk.Account, publicModel string) string {
	raw := accountCredential(account, chatModelMapCredential)
	if raw == "" {
		return ""
	}
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return ""
	}
	var mapping map[string]string
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		// 配错不阻断请求：保持公开名直连上游，由上游给出可诊断的错误。
		return ""
	}
	if upstream, ok := mapping[publicModel]; ok {
		if upstream = strings.TrimSpace(upstream); upstream != "" && !strings.EqualFold(upstream, publicModel) {
			return upstream
		}
	}
	// 大小写不敏感兜底，避免后台填写时大小写不一致导致静默不生效。
	for k, v := range mapping {
		if strings.EqualFold(strings.TrimSpace(k), publicModel) {
			if v = strings.TrimSpace(v); v != "" && !strings.EqualFold(v, publicModel) {
				return v
			}
		}
	}
	return ""
}

// rewriteChatRequestModel 把请求体里的 model 字段替换为上游模型 ID。
func rewriteChatRequestModel(body []byte, upstreamModel string) ([]byte, error) {
	if len(body) == 0 || upstreamModel == "" {
		return body, nil
	}
	return sjson.SetBytes(body, "model", upstreamModel)
}
