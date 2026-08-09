package model

import "testing"

// TestDeepSeekV4Flash TokenHub 基础价与元数据。
// TokenHub 价为 ¥1/¥0.2/¥2（每 1M tokens），按 ¥6.8/$ 换算为下列美元值。
func TestDeepSeekV4Flash(t *testing.T) {
	for _, id := range []string{"deepseek-v4-flash", "deepseek-v4-flash-202605"} {
		spec, registered := registry[id]
		if !registered {
			t.Fatalf("%s 未显式注册", id)
		}
		if spec.InputPrice != 0.14705882352941177 ||
			spec.CachedPrice != 0.02941176470588235 ||
			spec.OutputPrice != 0.29411764705882354 {
			t.Fatalf("%s 价格 = %v/%v/%v", id, spec.InputPrice, spec.CachedPrice, spec.OutputPrice)
		}
		if spec.ContextWindow != 1_000_000 || spec.MaxOutputTokens != 384_000 {
			t.Fatalf("%s 上下文 = %d/%d", id, spec.ContextWindow, spec.MaxOutputTokens)
		}
		// DeepSeek 没有 priority/flex 档,不得凭空造档价
		if spec.InputPricePriority != 0 || spec.InputPriceFlex != 0 {
			t.Fatalf("%s 不应有 priority/flex 档价: %+v", id, spec)
		}
		if v := vendorForModel(id); v != "deepseek" {
			t.Fatalf("%s vendor = %q", id, v)
		}
		if s := seriesForModel(id); s != "deepseek-v4" {
			t.Fatalf("%s series = %q", id, s)
		}
	}
}

// TestDeepSeekVariantFallback deepseek 变体不得掉进 GPT 系兜底价（18 倍多收）。
func TestDeepSeekVariantFallback(t *testing.T) {
	for _, id := range []string{"deepseek-v4-flash-0731", "deepseek-chat", "deepseek-v4-mini"} {
		spec := Lookup(id)
		if spec.InputPrice != 0.14705882352941177 ||
			spec.CachedPrice != 0.02941176470588235 ||
			spec.OutputPrice != 0.29411764705882354 {
			t.Fatalf("%s 应按 deepseek-v4-flash TokenHub 价推断, got input/cached/output=%v/%v/%v (%s)",
				id, spec.InputPrice, spec.CachedPrice, spec.OutputPrice, spec.Name)
		}
	}
}

// TestSeriesForModelExisting 既有系列折叠标识。
func TestSeriesForModelExisting(t *testing.T) {
	cases := map[string]string{
		"gpt-5.6-sol":            "gpt-5.6",
		"gpt-5.6-luna":           "gpt-5.6",
		"gpt-image-2":            "gpt-image",
		"gemini-3-pro-image":     "gemini-image",
		"gemini-3.1-flash-image": "gemini-image",
		"gpt-5.5":                "", // 单模型不折叠
		"glm-5.2":                "",
	}
	for id, want := range cases {
		if got := seriesForModel(id); got != want {
			t.Fatalf("seriesForModel(%q) = %q, want %q", id, got, want)
		}
	}
}
