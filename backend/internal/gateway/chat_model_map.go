package gateway

import (
	"encoding/json"
	"log/slog"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
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

// restoreChatResponseModelData 把回给客户端的响应字节里的上游模型 ID 还原为公开名。
//
// chat_model_map 只应影响「发往上游的字节」，但上游会在响应里回显自己的模型 ID
// （chat 的顶层 model、Responses API 的 response.model），不还原就把上游身份泄露
// 给客户端（如 tokenforge/kimi-k3），且部分客户端会校验响应 model 与请求一致。
// 计费侧的还原由 restoreMappedUsageModel 负责，此处只管客户端可见字节。
//
// 仅当字段值与上游 ID 完全一致时才改写，其余情况原样透传。
func restoreChatResponseModelData(data, upstreamModel, publicModel string) string {
	if data == "" || upstreamModel == "" || publicModel == "" || upstreamModel == publicModel {
		return data
	}
	for _, key := range [...]string{"model", "response.model"} {
		if gjson.Get(data, key).String() != upstreamModel {
			continue
		}
		if patched, err := sjson.Set(data, key, publicModel); err == nil {
			data = patched
		}
	}
	return data
}

// restoreMappedUsageModel 把 Usage.Model 从上游 ID 还原为公开模型名，并按公开名重算成本。
//
// 做过 chat_model_map 映射时，上游按自己的 ID 回包，而 Usage.Model 取自响应体。
// 上游 ID 不在我方价格表里，model.Lookup 会走关键字兜底匹配到别的型号——实测
// deepseek-v4-pro-ga-260813 被按 DeepSeek Flash 计价，Pro 少收 3 倍，且 usage_logs
// 的 model 列记成上游 ID 导致对账与用量统计一并失真。
//
// 未发生映射（mappedPublicModel 为空）时完全不介入，保持既有行为。
func restoreMappedUsageModel(logger *slog.Logger, outcome *sdk.ForwardOutcome, mappedPublicModel string) {
	if mappedPublicModel == "" || outcome == nil || outcome.Usage == nil {
		return
	}
	upstreamModel := outcome.Usage.Model
	if upstreamModel == mappedPublicModel {
		return
	}
	outcome.Usage.Model = mappedPublicModel
	setUsageModelAttribute(outcome.Usage, mappedPublicModel)
	// 成本此前是按上游 ID 查表算出来的，必须按公开名整体重算覆盖。
	fillUsageCost(outcome.Usage)
	if logger != nil {
		logger.Info("chat_usage_model_restored",
			"upstream_model", upstreamModel,
			"public_model", mappedPublicModel,
		)
	}
}
