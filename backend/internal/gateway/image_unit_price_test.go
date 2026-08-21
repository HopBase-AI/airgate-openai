package gateway

import (
	"sort"
	"testing"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// TestImageUnitPriceTiersMatchSupportedSizes 把「广场标了什么档位」和「网关放行什么尺寸」
// 焊死在一起。两者分居 model / gateway 两个包，靠人眼同步过一次就漂了：模型广场曾给
// 只出 1K 的 flash-lite 标 2K/4K 价，客户照着下单必然吃 validateImageModelSize 的 400。
func TestImageUnitPriceTiersMatchSupportedSizes(t *testing.T) {
	for id, sizes := range imageModelSupportedSizes {
		spec := model.Lookup(id)
		declared := make([]string, 0, 3)
		for _, tier := range spec.ImageUnit.Tiers() {
			declared = append(declared, tier.Tier)
		}
		// 未声明单张牌价的型号（gpt-image 系）回落 token 展示，不参与本守卫。
		if len(declared) == 0 {
			continue
		}
		supported := map[string]bool{}
		for size := range sizes {
			if size == "auto" {
				continue
			}
			supported[imageTierForSize(size)] = true
		}
		want := make([]string, 0, len(supported))
		for _, tier := range []string{"1k", "2k", "4k"} {
			if supported[tier] {
				want = append(want, tier)
			}
		}
		sort.Strings(declared)
		sort.Strings(want)
		if len(declared) != len(want) {
			t.Fatalf("%s 声明档位 %v，尺寸白名单支持 %v", id, declared, want)
		}
		for i := range declared {
			if declared[i] != want[i] {
				t.Fatalf("%s 声明档位 %v，尺寸白名单支持 %v", id, declared, want)
			}
		}
	}
}
