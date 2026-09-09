package model

import "testing"

// GPT Image 2.5 两档必须是注册表精确命中，而不是被 fallbackByKeyword 的 "image"
// 关键字兜到 gpt-image-1.5：两者 cached 价差 2.5 倍，兜底只有一条告警日志可见。
func TestLookup_GPTImage25_RegisteredWithOfficialPrices(t *testing.T) {
	cases := map[string]string{
		"gpt-image-2.5-flare":    "GPT Image 2.5 Flare",
		"gpt-image-2.5-sunburst": "GPT Image 2.5 Sunburst",
	}
	for id, name := range cases {
		t.Run(id, func(t *testing.T) {
			spec := Lookup(id)
			if spec.Name != name {
				t.Fatalf("Lookup(%q).Name = %q, want %q（疑似走了关键字兜底）", id, spec.Name, name)
			}
			if spec.InputPrice != 5.0 || spec.CachedPrice != 1.25 || spec.OutputPrice != 30.0 {
				t.Fatalf("%s 官方价应为 in=5 cached=1.25 out=30，实际 in=%v cached=%v out=%v",
					id, spec.InputPrice, spec.CachedPrice, spec.OutputPrice)
			}
			if !spec.ImageOnly {
				t.Fatalf("%s 应标记 ImageOnly", id)
			}
			if spec.ImagePerUnitBilling {
				t.Fatalf("%s 按 token 计费，不应启用按张计费", id)
			}
			if !IsKnown(id) {
				t.Fatalf("IsKnown(%q) 应为 true，否则入口会把 model 换成默认值", id)
			}
		})
	}
	// 与关键字兜底目标 gpt-image-1.5 的 cached 价不同，证明上面不是兜底命中。
	if Lookup("gpt-image-1.5").CachedPrice == Lookup("gpt-image-2.5-flare").CachedPrice {
		t.Fatalf("测试失去判别力：gpt-image-1.5 与 2.5 的 cached 价相同")
	}
}

// 未注册的 2.5 变体（如 -4k 档位后缀）仍会被关键字兜底到 gpt-image-1.5——
// 这是已知行为，记录在案供 review 判断是否要显式注册档位别名。
func TestLookup_GPTImage25_UnregisteredVariantFallsBackByKeyword(t *testing.T) {
	spec := Lookup("gpt-image-2.5-flare-4k")
	if spec.Name != Lookup("gpt-image-1.5").Name {
		t.Fatalf("未注册变体应兜到 gpt-image-1.5，实际 %q", spec.Name)
	}
}

func TestGPTImage25_ModelInfoMetadata(t *testing.T) {
	for _, id := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		mi := toModelInfo(id, Lookup(id))
		if mi.Metadata["family"] != "gpt-image" {
			t.Errorf("%s family = %q, want gpt-image（账号家族冷却维度）", id, mi.Metadata["family"])
		}
		if mi.Metadata["series"] != "gpt-image" {
			t.Errorf("%s series = %q, want gpt-image（模型广场折叠）", id, mi.Metadata["series"])
		}
		if mi.Metadata["vendor"] != "openai" {
			t.Errorf("%s vendor = %q, want openai", id, mi.Metadata["vendor"])
		}
		if mi.Metadata["price.cached_input"] != "1.25" {
			t.Errorf("%s price.cached_input = %q, want 1.25", id, mi.Metadata["price.cached_input"])
		}
	}
	found := map[string]bool{}
	for _, mi := range AllModels() {
		found[mi.ID] = true
	}
	for _, id := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		if !found[id] {
			t.Errorf("AllModels() 缺少 %s，core 目录拿不到该模型", id)
		}
	}
}
