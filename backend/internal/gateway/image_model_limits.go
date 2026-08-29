package gateway

import (
	"fmt"
	"strings"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// Gemini 生图模型的尺寸口径：上游根本不收 WIDTHxHEIGHT，只收 aspect_ratio
// （10 个官方比例）+ image_size（1K/2K/4K 档位）。客户的 size 是我们**翻译**
// 出去的，不是透传参数——所以任意可解析的 WxH 一律就近吸附到该模型支持的
// （比例, 档位），永不因比例拒绝；只有客户显式点名 1K/2K/4K 且超出模型能力
// 档位时才 400（明确报错优于静默降档）。
//
// 档位上限与 model 包的 ImageUnit 牌价档位**同源**（flash/flash-lite 只有 1K、
// 3.1-flash 最高 2K、只有 3-pro 有 4K），广场标价与网关放行由构造保证一致。
//
// 历史教训（两次同型事故）：
//   - 2026-08-24 gpt-image-2：19 条枚举白名单把六个官方合法尺寸拒成 400，
//     直连上游逐个实测全部可出图——改走 validateImageSize() 官方规则校验。
//   - 2026-08-29 gemini-2.5-flash-image：白名单只登记 1:1/3:2/2:3，9:16 竖图
//     被拒，而上游 chat 桥接实测 9:16/16:9 均正常出图（768x1344）。
//
// ⚠️ 改本文件必须跑真实链路验证：forward.go 的这道闸门在 images.go 规则校验
// 之前执行，单测证明不了闸门顺序（见仓库 CLAUDE.md 红线）。
func validateImageModelSize(modelID, size string) error {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	size = strings.ToLower(strings.TrimSpace(size))
	if modelID == "" || size == "" || size == "auto" {
		return nil
	}
	// gpt-image-2 官方口径是任意分辨率，按官方规则校验而不是查表。
	if isGPTImage2Model(modelID) {
		return validateImageSize(size, modelID)
	}
	tiers := geminiImageModelDeclaredTiers(modelID)
	if len(tiers) == 0 {
		return nil
	}
	if literal, ok := geminiLiteralTier(size); ok {
		for _, tier := range tiers {
			if tier == literal {
				return nil
			}
		}
		return fmt.Errorf("模型 %s 不支持 %s 档，支持: %s",
			modelID, strings.ToUpper(literal), strings.ToUpper(strings.Join(tiers, ", ")))
	}
	if _, _, ok := parseImageSize(size); ok {
		return nil
	}
	return fmt.Errorf("模型 %s 的 size %q 无法解析，应为 WIDTHxHEIGHT（任意比例，就近映射官方档位）或 1K/2K/4K", modelID, size)
}

// geminiImageModelDeclaredTiers 返回模型声明的牌价档位（小写 1k/2k/4k，升序）。
// 空切片表示该模型不按固定档位计价，不参与档位校验。
func geminiImageModelDeclaredTiers(modelID string) []string {
	if !isGeminiImageModel(modelID) {
		return nil
	}
	spec := model.Lookup(modelID)
	declared := spec.ImageUnit.Tiers()
	tiers := make([]string, 0, len(declared))
	for _, tier := range declared {
		tiers = append(tiers, strings.ToLower(tier.Tier))
	}
	return tiers
}

// geminiLiteralTier 识别显式档位字面量（大小写不敏感）。
func geminiLiteralTier(size string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1k":
		return "1k", true
	case "2k":
		return "2k", true
	case "4k":
		return "4k", true
	}
	return "", false
}

func geminiTierRank(tier string) int {
	switch strings.ToLower(tier) {
	case "1k":
		return 1
	case "2k":
		return 2
	case "4k":
		return 3
	}
	return 0
}

// geminiMaxDeclaredTier 返回模型能力上限档位；无声明时回落 1k（最保守）。
func geminiMaxDeclaredTier(modelID string) string {
	maxTier := ""
	for _, tier := range geminiImageModelDeclaredTiers(modelID) {
		if geminiTierRank(tier) > geminiTierRank(maxTier) {
			maxTier = tier
		}
	}
	if maxTier == "" {
		return "1k"
	}
	return maxTier
}

// geminiAspectRatios 是 Gemini 官方支持的宽高比全集。
// 与 airgate-gemini 插件 images.go 的同名表保持一致（两插件分仓无法共享代码）。
var geminiAspectRatios = []struct {
	name  string
	value float64
}{
	{name: "1:1", value: 1},
	{name: "2:3", value: 2.0 / 3},
	{name: "3:2", value: 3.0 / 2},
	{name: "3:4", value: 3.0 / 4},
	{name: "4:3", value: 4.0 / 3},
	{name: "4:5", value: 4.0 / 5},
	{name: "5:4", value: 5.0 / 4},
	{name: "9:16", value: 9.0 / 16},
	{name: "16:9", value: 16.0 / 9},
	{name: "21:9", value: 21.0 / 9},
}

func closestGeminiAspectRatio(width, height int) string {
	target := float64(width) / float64(height)
	best := geminiAspectRatios[0]
	bestDistance := target - best.value
	if bestDistance < 0 {
		bestDistance = -bestDistance
	}
	for _, candidate := range geminiAspectRatios[1:] {
		distance := target - candidate.value
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			best = candidate
			bestDistance = distance
		}
	}
	return best.name
}
