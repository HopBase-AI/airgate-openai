package model

import "testing"

// TestGrokChatSpecs xAI 官方牌价（2026-08-24 docs.x.ai 核实）与长上下文阶梯。
// 中继实报价 = 官方 × 0.27，逐档验证吻合；注册表只写官方基准价。
func TestGrokChatSpecs(t *testing.T) {
	cases := map[string]struct {
		ctx                   int
		input, cached, output float64
	}{
		"grok-4.20-0309-reasoning":   {2_000_000, 1.25, 0.20, 2.50},
		"grok-4.20-multi-agent-0309": {2_000_000, 1.25, 0.20, 2.50},
		"grok-4.3":                   {1_000_000, 1.25, 0.20, 2.50},
		"grok-4.5":                   {500_000, 2.0, 0.30, 6.0},
		"grok-4.6":                   {500_000, 2.0, 0.50, 6.0},
	}
	for id, want := range cases {
		spec, registered := registry[id]
		if !registered {
			t.Fatalf("%s 未显式注册", id)
		}
		if spec.InputPrice != want.input || spec.CachedPrice != want.cached || spec.OutputPrice != want.output {
			t.Fatalf("%s 价格 = %v/%v/%v, want %v/%v/%v",
				id, spec.InputPrice, spec.CachedPrice, spec.OutputPrice, want.input, want.cached, want.output)
		}
		if spec.ContextWindow != want.ctx {
			t.Fatalf("%s 上下文 = %d, want %d", id, spec.ContextWindow, want.ctx)
		}
		// xAI 官方：提示 ≥200K 时整笔 in/cached/out 全部 ×2。
		if spec.LongContextThreshold != 200_000 ||
			spec.LongContextInputMultiplier != 2.0 ||
			spec.LongContextOutputMultiplier != 2.0 ||
			spec.LongContextCachedMultiplier != 2.0 {
			t.Fatalf("%s 长上下文阶梯 = %+v", id, spec)
		}
		// xAI 上游没有 priority/flex 档，不得凭空造档价。
		if spec.InputPricePriority != 0 || spec.InputPriceFlex != 0 {
			t.Fatalf("%s 不应有 priority/flex 档价: %+v", id, spec)
		}
		// 上游 completion_tokens 不含 reasoning，漏了这个标记推理输出几乎免费。
		if !spec.OutputExcludesReasoning {
			t.Fatalf("%s 缺 OutputExcludesReasoning 标记", id)
		}
		if v := vendorForModel(id); v != "xai" {
			t.Fatalf("%s vendor = %q", id, v)
		}
		if s := seriesForModel(id); s != "grok-4" {
			t.Fatalf("%s series = %q", id, s)
		}
	}
}

// TestGrokImageSpecs 按张计费图像模型：官方单张价与输入参考图价。
func TestGrokImageSpecs(t *testing.T) {
	cases := map[string]struct {
		oneK, twoK, input float64
	}{
		"grok-imagine-image":         {0.02, 0, 0.002},
		"grok-imagine-image-2.0":     {0.06, 0.08, 0.01},
		"grok-imagine-image-quality": {0.05, 0.07, 0.01},
	}
	for id, want := range cases {
		spec, registered := registry[id]
		if !registered {
			t.Fatalf("%s 未显式注册", id)
		}
		if !spec.ImageOnly || !spec.ImagePerUnitBilling {
			t.Fatalf("%s 应为按张计费的纯图像模型: %+v", id, spec)
		}
		if spec.ImageUnit.OneK != want.oneK || spec.ImageUnit.TwoK != want.twoK {
			t.Fatalf("%s 单张价 = %v/%v, want %v/%v", id, spec.ImageUnit.OneK, spec.ImageUnit.TwoK, want.oneK, want.twoK)
		}
		if spec.ImageInputUnitPrice != want.input {
			t.Fatalf("%s 输入图价 = %v, want %v", id, spec.ImageInputUnitPrice, want.input)
		}
		// token 价必须留兜底非零，防异常路径变免费流量。
		if spec.InputPrice <= 0 || spec.OutputPrice <= 0 {
			t.Fatalf("%s token 兜底价不能为 0: %+v", id, spec)
		}
		if v := vendorForModel(id); v != "xai" {
			t.Fatalf("%s vendor = %q", id, v)
		}
		if s := seriesForModel(id); s != "grok-imagine-image" {
			t.Fatalf("%s series = %q", id, s)
		}
	}
}

// TestGrokVariantFallback 未注册 grok 变体的兜底：
// 对话变体必须落 grok-4.6（保住 OutputExcludesReasoning，掉进 GPT 兜底会漏推理输出费），
// imagine 变体必须落 grok-imagine-image-2.0（掉进 gpt-image token 价会因响应无 token 变免费）。
func TestGrokVariantFallback(t *testing.T) {
	for _, id := range []string{"grok-4.9", "grok-5", "grok-code-fast"} {
		spec := Lookup(id)
		if !spec.OutputExcludesReasoning {
			t.Fatalf("%s 兜底 Spec 丢了 OutputExcludesReasoning: %+v", id, spec)
		}
		if spec.InputPrice != 2.0 || spec.OutputPrice != 6.0 {
			t.Fatalf("%s 应按 grok-4.6 兜底, got %v/%v (%s)", id, spec.InputPrice, spec.OutputPrice, spec.Name)
		}
	}
	for _, id := range []string{"grok-imagine-image-3.0", "grok-imagine-image-pro"} {
		spec := Lookup(id)
		if !spec.ImagePerUnitBilling || spec.ImageUnit.OneK != 0.06 {
			t.Fatalf("%s 应按 grok-imagine-image-2.0 兜底, got %+v", id, spec)
		}
	}
}
