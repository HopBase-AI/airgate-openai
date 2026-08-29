package gateway

import (
	"strings"
	"testing"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// TestGeminiTierGateDerivesFromModelSpecs 档位闸门与「广场标了什么档位」同源：
// validateImageModelSize 直接读 model 包的 ImageUnit 牌价档位，显式点名超出
// 声明档位的字面量必须 400，声明内的必须放行。历史背景：两者曾分居两张手写表，
// 模型广场给只出 1K 的 flash-lite 标过 2K/4K 价，客户照着下单必然吃 400。
func TestGeminiTierGateDerivesFromModelSpecs(t *testing.T) {
	checked := 0
	for _, info := range model.AllModels() {
		if !isGeminiImageModel(info.ID) {
			continue
		}
		spec := model.Lookup(info.ID)
		declared := spec.ImageUnit.Tiers()
		if len(declared) == 0 {
			continue
		}
		checked++
		allowed := map[string]bool{}
		for _, tier := range declared {
			allowed[strings.ToLower(tier.Tier)] = true
		}
		for _, tier := range []string{"1k", "2k", "4k"} {
			err := validateImageModelSize(info.ID, tier)
			if allowed[tier] && err != nil {
				t.Errorf("%s 声明了 %s 档却被拒: %v", info.ID, tier, err)
			}
			if !allowed[tier] && err == nil {
				t.Errorf("%s 未声明 %s 档，显式点名应 400", info.ID, tier)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有找到任何声明档位的 Gemini 生图模型，守卫失效")
	}
}

// TestGeminiWxHSizesNeverRejected 任意可解析 WxH 永不拒绝——上游只收
// aspect_ratio+image_size，尺寸是我们翻译的，翻译表缺行不该变成客户的 400
// （2026-08-24 gpt-image-2、2026-08-29 gemini-2.5-flash-image 9:16 两次事故同型）。
func TestGeminiWxHSizesNeverRejected(t *testing.T) {
	sizes := []string{
		"1024x1024", "1536x1024", "1024x1536",
		"1024x1792", "1792x1024", // 9:16 / 16:9，2026-08-29 上游实测可出图
		"768x1344", "1344x768",
		"2048x2048", "3840x2160", // 超出小模型能力档位 → 钳制而非拒绝
		"640x480",
	}
	for _, modelID := range []string{
		"gemini-2.5-flash-image",
		"gemini-3-pro-image",
		"gemini-3.1-flash-image",
		"gemini-3.1-flash-lite-image",
	} {
		for _, size := range sizes {
			if err := validateImageModelSize(modelID, size); err != nil {
				t.Errorf("%s 拒绝了 WxH 尺寸 %s: %v", modelID, size, err)
			}
		}
	}
	if err := validateImageModelSize("gemini-2.5-flash-image", "banana"); err == nil {
		t.Error("无法解析的 size 应当 400")
	}
}

// TestGeminiImageChatConfigSnapsToOfficialMenu size → (aspect_ratio, image_size)
// 的吸附翻译：比例就近取官方 10 比例，档位按长边推导后钳到模型声明上限。
func TestGeminiImageChatConfigSnapsToOfficialMenu(t *testing.T) {
	for _, tt := range []struct {
		model, size, wantAspect, wantTier string
	}{
		// 旧枚举表的全部条目行为保持不变
		{"gemini-3-pro-image", "1024x1024", "1:1", "1K"},
		{"gemini-3-pro-image", "1536x1024", "3:2", "1K"},
		{"gemini-3-pro-image", "1024x1536", "2:3", "1K"},
		{"gemini-3-pro-image", "2048x2048", "1:1", "2K"},
		{"gemini-3-pro-image", "2048x1152", "16:9", "2K"},
		{"gemini-3-pro-image", "1152x2048", "9:16", "2K"},
		{"gemini-3-pro-image", "3840x2160", "16:9", "4K"},
		{"gemini-3-pro-image", "2160x3840", "9:16", "4K"},
		// 新增能力：任意比例吸附
		{"gemini-2.5-flash-image", "1024x1792", "9:16", "1K"}, // flash 只有 1K，长边 1792 推 2K 后钳回
		{"gemini-2.5-flash-image", "1792x1024", "16:9", "1K"},
		{"gemini-2.5-flash-image", "768x1344", "9:16", "1K"},
		{"gemini-3.1-flash-image", "3840x2160", "16:9", "2K"}, // 3.1 最高 2K，4K 钳回
		{"gemini-3.1-flash-lite-image", "2048x2048", "1:1", "1K"},
		{"gemini-3-pro-image", "2560x1080", "21:9", "4K"},
		// 显式档位字面量
		{"gemini-3-pro-image", "4K", "1:1", "4K"},
		{"gemini-2.5-flash-image", "1k", "1:1", "1K"},
	} {
		got := geminiImageChatConfig(tt.model, &imagesRequest{Size: tt.size})
		if got == nil {
			t.Errorf("%s size=%s 返回 nil，want %s/%s", tt.model, tt.size, tt.wantAspect, tt.wantTier)
			continue
		}
		if got["aspect_ratio"] != tt.wantAspect || got["image_size"] != tt.wantTier {
			t.Errorf("%s size=%s → %s/%s, want %s/%s",
				tt.model, tt.size, got["aspect_ratio"], got["image_size"], tt.wantAspect, tt.wantTier)
		}
	}
	if got := geminiImageChatConfig("gemini-3-pro-image", &imagesRequest{Size: "auto"}); got != nil {
		t.Errorf("auto 应返回 nil（上游默认），got %v", got)
	}
}
