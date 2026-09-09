package gateway

import (
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// imageModelMapCredential 账号级「公开图像模型名 → 上游模型 ID」映射，与
// chat_model_map 同形（JSON 对象），对任意图像模型生效：
//
//	{"gpt-image-2.5-flare":"MM-H3-sft-Mlogic-High-25-image",
//	 "gpt-image-2.5-sunburst":"MM-H3-sft-Mlogic-Max-25-image"}
//
// 背景：gpt_image_2_upstream_model 是单值凭证，一个账号只能给 gpt-image-2 起别名；
// MiniMax 一把 key 同时供 canvas-20 与两款 H3 2.5 图像模型，且上游只认 URL 里的
// 模型名（body.model 被忽略），必须按模型分别映射并配合 images_path_prefix 的
// {model} 占位符拼路径。
//
// 解析优先级（imageUpstreamModelIDForAccount）：image_model_map 命中 >
// gpt_image_2_upstream_model（仅 gpt-image-2 系，向后兼容）> yhshu 自动别名 > 原样。
//
// 仅重写发往上游的模型 ID（body.model 与路径占位）；计费、用量与客户可见的 model
// 字段一律还原为公开名（imagePublicModelID），否则上游 ID 泄漏给客户端，且
// model.Lookup 查不到价走关键字兜底静默错价。
const imageModelMapCredential = "image_model_map"

// imageModelMapUpstreamForAccount 返回该账号 image_model_map 中 publicModel 对应的
// 上游 ID；未配置 / JSON 非法 / 未命中 / 映射到自身时返回空串。
func imageModelMapUpstreamForAccount(account *sdk.Account, publicModel string) string {
	return upstreamModelFromMapCredential(account, imageModelMapCredential, publicModel)
}

// imageModelWasMapped 判断本次图像请求是否做过「公开名 → 上游 ID」改写：
// 上游 ID 非空且与公开名不同。做过映射的响应必须把模型名还原为公开名。
func imageModelWasMapped(publicModel, upstreamModel string) bool {
	publicModel = strings.TrimSpace(publicModel)
	upstreamModel = strings.TrimSpace(upstreamModel)
	return publicModel != "" && upstreamModel != "" && !strings.EqualFold(publicModel, upstreamModel)
}
