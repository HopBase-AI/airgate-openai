package gateway

import (
	"encoding/json"
	"os"
	"strings"
)

// anthropicRouteMetadata 构建 Anthropic 协议翻译路由的 Metadata 声明：
//   - error_format：对外错误体按 Anthropic 格式输出；
//   - scheduling_model_map：claude-* 请求模型 → OpenAI 平台调度模型的前缀映射表，
//     Core 据此选号（声明式，取代 Core 侧的平台/路径硬编码，见 core 的
//     docs/architecture/current/plugin-contract.md 约定表）。
func anthropicRouteMetadata() map[string]string {
	return map[string]string{
		"error_format":              "anthropic",
		"scheduling_model_map":      anthropicSchedulingModelMapJSON(),
		"subscription_output_bound": anthropicOutputBoundJSON,
	}
}

// subscription_output_bound 告诉 core 这条协议用哪个字段限制输出长度。
// 订阅制分组按「本次请求最多花多少」准入：模型的输出上限值钱远超套餐给单条消息的
// 额度,声明了这个字段,core 就把输出封到额度买得起的长度并改写请求,而不是把超额
// 请求直接拒掉。每条路由按自己的协议声明,不能相互套用。
const (
	anthropicOutputBoundJSON = `{"fields":["max_tokens"]}`
	// Chat Completions：推理模型只认 max_completion_tokens,老字段仍兼容,
	// 因此优先写新字段;调用方已经传了哪个就收窄哪个。
	chatCompletionsOutputBoundJSON = `{"fields":["max_completion_tokens","max_tokens"]}`
	responsesOutputBoundJSON       = `{"fields":["max_output_tokens"]}`
)

// outputBoundMetadata 只声明输出上限字段的路由元数据。
func outputBoundMetadata(contract string) map[string]string {
	return map[string]string{"subscription_output_bound": contract}
}

// anthropicSchedulingModelMapJSON 生成前缀映射表 JSON。
// 键为模型 ID 前缀（Core 按最长前缀匹配），值为调度候选模型（按优先级）。
// 部署可经环境变量覆盖（与历史 Core 侧变量同名，平滑迁移）。
func anthropicSchedulingModelMapJSON() string {
	defaultTarget := envModel("gpt-5.5", "AIRGATE_DEFAULT_CLAUDE_MODEL")
	m := map[string][]string{
		"claude-haiku-": {
			envModel("gpt-5.3-codex-spark", "AIRGATE_MODEL_HAIKU", "ANTHROPIC_DEFAULT_HAIKU_MODEL"),
			envModel("gpt-5.4-mini", "AIRGATE_MODEL_HAIKU_FALLBACK"),
		},
		"claude-sonnet-": {
			envModel(defaultTarget, "AIRGATE_MODEL_SONNET", "ANTHROPIC_DEFAULT_SONNET_MODEL"),
			envModel("gpt-5.4", "AIRGATE_MODEL_SONNET_FALLBACK"),
		},
		"claude-opus-": {
			envModel(defaultTarget, "AIRGATE_MODEL_OPUS", "ANTHROPIC_DEFAULT_OPUS_MODEL"),
			envModel("gpt-5.4", "AIRGATE_MODEL_OPUS_FALLBACK"),
		},
		"claude-": {
			defaultTarget,
			envModel("gpt-5.4", "AIRGATE_MODEL_DEFAULT_FALLBACK"),
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// envModel 依次读取环境变量，取首个非空值，否则用默认值。
func envModel(fallback string, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return fallback
}
