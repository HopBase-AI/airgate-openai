package model

import (
	"strings"
	"testing"
)

func withCatalogOverlay(t *testing.T, raw string) {
	t.Helper()
	if _, err := SetCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("SetCatalogOverlayJSON(%q): %v", raw, err)
	}
	t.Cleanup(ResetCatalogOverlay)
}

// TestLookup_UnknownModelFallsBackToBillablePrice 回归测试：未注册模型必须走兜底定价，
// 绝不能返回 0 价（那会导致免费流量 / 计费全 0）。
func TestLookup_UnknownModelFallsBackToBillablePrice(t *testing.T) {
	spec := Lookup("some-brand-new-unknown-model-xyz")
	if spec.InputPrice <= 0 || spec.OutputPrice <= 0 {
		t.Fatalf("未知模型必须有兜底价格；InputPrice=%v OutputPrice=%v", spec.InputPrice, spec.OutputPrice)
	}
}

// TestLookup_ByKeyword 按关键词推断到对应系列。
func TestLookup_ByKeyword(t *testing.T) {
	cases := []struct {
		name      string
		modelID   string
		wantMatch string // 期望命中的注册键（按 InputPrice 识别）
	}{
		{"未知 codex 系列 → gpt-5.4", "gpt-5.9-codex-preview", "gpt-5.4"},
		{"未知 image 系列 → gpt-image-1.5", "gpt-image-3", "gpt-image-1.5"},
		{"未知 mini 系列 → gpt-5.4-mini", "gpt-5.9-mini", "gpt-5.4-mini"},
		{"未知 nano 系列 → gpt-5.4-mini", "gpt-5.9-nano", "gpt-5.4-mini"},
		{"未知 gpt-5 系列 → gpt-5.4", "gpt-5.9", "gpt-5.4"},
		{"o1 推理模型 → gpt-5.4", "o1-preview", "gpt-5.4"},
		{"o3 推理模型 → gpt-5.4", "o3-mini", "gpt-5.4-mini"}, // "mini" 优先
		{"gpt-4 系列 → gpt-5.4（偏保守）", "gpt-4o", "gpt-5.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Lookup(tc.modelID)
			want := registry[tc.wantMatch]
			if got.InputPrice != want.InputPrice || got.OutputPrice != want.OutputPrice {
				t.Errorf("Lookup(%q): got InputPrice=%v OutputPrice=%v, want match %q (In=%v Out=%v)",
					tc.modelID, got.InputPrice, got.OutputPrice,
					tc.wantMatch, want.InputPrice, want.OutputPrice)
			}
		})
	}
}

// TestLookup_KnownModelUnchanged 已注册模型不受关键词兜底影响。
func TestLookup_KnownModelUnchanged(t *testing.T) {
	ResetCatalogOverlay()
	t.Run("gpt-5.5", func(t *testing.T) {
		spec := Lookup("gpt-5.5")
		if spec.InputPrice != 5.0 || spec.OutputPrice != 30.0 || spec.CachedPrice != 0.5 {
			t.Errorf("gpt-5.5 定价变化: In=%v Out=%v Cached=%v", spec.InputPrice, spec.OutputPrice, spec.CachedPrice)
		}
		if spec.ContextWindow != 400000 {
			t.Errorf("gpt-5.5 ContextWindow = %v, want 400000", spec.ContextWindow)
		}
		if spec.InputPricePriority != 12.5 || spec.OutputPricePriority != 75.0 || spec.CachedPricePriority != 1.25 {
			t.Errorf("gpt-5.5 priority 定价变化: In=%v Out=%v Cached=%v", spec.InputPricePriority, spec.OutputPricePriority, spec.CachedPricePriority)
		}
		if spec.InputPriceFast != 0 || spec.OutputPriceFast != 0 || spec.CachedPriceFast != 0 {
			t.Errorf("gpt-5.5 不应配置 fast 定价: In=%v Out=%v Cached=%v", spec.InputPriceFast, spec.OutputPriceFast, spec.CachedPriceFast)
		}
	})

	t.Run("gpt-5.4", func(t *testing.T) {
		spec := Lookup("gpt-5.4")
		if spec.InputPrice != 2.5 || spec.OutputPrice != 15.0 {
			t.Errorf("gpt-5.4 定价变化: In=%v Out=%v", spec.InputPrice, spec.OutputPrice)
		}
	})
}

func TestCatalogOverlay_OverrideExistingPrice(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.4","pricing":{"input":2,"cached_input":0.2,"output":10}}
	]`)

	spec := Lookup("gpt-5.4")
	if spec.InputPrice != 2 || spec.CachedPrice != 0.2 || spec.OutputPrice != 10 {
		t.Fatalf("gpt-5.4 overlay pricing = %+v, want input 2 cached 0.2 output 10", spec)
	}
	if spec.ContextWindow != registry["gpt-5.4"].ContextWindow {
		t.Fatalf("未覆盖字段不应改变, ContextWindow = %d", spec.ContextWindow)
	}
}

func TestCatalogOverlay_PartialPricingKeepsSafeDefaults(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.5","pricing":{"input":4,"cached_input":0,"output":0}}
	]`)

	spec := Lookup("gpt-5.5")
	base := registry["gpt-5.5"]
	if spec.InputPrice != 4 {
		t.Fatalf("显式 input 价格未覆盖: %+v", spec)
	}
	if spec.CachedPrice != base.CachedPrice || spec.OutputPrice != base.OutputPrice {
		t.Fatalf("缺省/0 价格不应覆盖成免费价: got %+v want cached %.2f output %.2f", spec, base.CachedPrice, base.OutputPrice)
	}
}

func TestCatalogOverlay_AddNewModel(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-6-preview","name":"GPT 6 Preview","context_window":500000,"max_output_tokens":128000,
	   "pricing":{"input":8,"cached_input":0.8,"output":40}}
	]`)

	if !IsKnown("gpt-6-preview") {
		t.Fatal("新增模型应被 IsKnown 识别")
	}
	spec := Lookup("gpt-6-preview")
	if spec.Name != "GPT 6 Preview" || spec.ContextWindow != 500000 ||
		spec.InputPrice != 8 || spec.CachedPrice != 0.8 || spec.OutputPrice != 40 {
		t.Fatalf("新增模型 spec = %+v", spec)
	}
	found := false
	for _, m := range AllModels() {
		if m.ID == "gpt-6-preview" {
			found = true
			if m.Name != "GPT 6 Preview" || m.ContextWindow != 500000 {
				t.Fatalf("AllModels 新增模型元信息错误: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("AllModels 应包含新增模型")
	}
}

// TestCatalogOverlay_NonGPTNewModelHasNoLongContextTier 非 GPT 系列的覆盖层新增模型(如 GLM)
// 不应继承 DefaultSpec 的 272K 长上下文阶梯——否则 >272K 请求会按 GPT 的 ×2 in/×1.5 out 错误计费。
func TestCatalogOverlay_NonGPTNewModelHasNoLongContextTier(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"glm-5.2","name":"GLM-5.2","context_window":1000000,"max_output_tokens":128000,
	   "pricing":{"input":1.4,"cached_input":0.26,"output":4.4}}
	]`)

	spec := Lookup("glm-5.2")
	if spec.LongContextThreshold != 0 {
		t.Fatalf("非 GPT 新模型不应有长上下文阶梯, got threshold=%d", spec.LongContextThreshold)
	}
	if spec.LongContextInputMultiplier != 0 || spec.LongContextOutputMultiplier != 0 || spec.LongContextCachedMultiplier != 0 {
		t.Fatalf("非 GPT 新模型不应有长上下文倍率, got in=%v out=%v cached=%v",
			spec.LongContextInputMultiplier, spec.LongContextOutputMultiplier, spec.LongContextCachedMultiplier)
	}
	if spec.InputPrice != 1.4 || spec.OutputPrice != 4.4 {
		t.Fatalf("标准价应正常生效: %+v", spec)
	}
}

// TestCatalogOverlay_GPTNewModelDoesNotInventLongContextTier 未注册的新 GPT 型号不能继承 5.6 阶梯。
func TestCatalogOverlay_GPTNewModelDoesNotInventLongContextTier(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.7-nova","name":"GPT 5.7 Nova","pricing":{"input":1,"output":6}}
	]`)
	spec := Lookup("gpt-5.7-nova")
	if spec.LongContextThreshold != 0 {
		t.Fatalf("非 5.6 三规格模型不应带 272K 阶梯, got %d", spec.LongContextThreshold)
	}
}

// TestCatalogOverlay_NonGPTModelKeepsExplicitLongContext 非 GPT 新模型若在 overlay 里显式
// 声明 long_context(如 Gemini 3.1 Pro 的 200K 阶梯),修复后仍必须保留——inferNewModelBase
// 清零默认阶梯,但 applyOverlay 随后应用显式声明。此测试锁定该顺序,防止未来重构悄悄少算钱。
func TestCatalogOverlay_NonGPTModelKeepsExplicitLongContext(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gemini-3.1-pro-preview","name":"Gemini 3.1 Pro","pricing":{"input":1.25,"cached_input":0.125,"output":10},
	   "long_context":{"threshold":200000,"input_multiplier":2,"cached_multiplier":2,"output_multiplier":1.5}}
	]`)
	spec := Lookup("gemini-3.1-pro-preview")
	if spec.LongContextThreshold != 200000 {
		t.Fatalf("显式声明的长上下文阶梯应保留, got threshold=%d", spec.LongContextThreshold)
	}
	if spec.LongContextInputMultiplier != 2 || spec.LongContextCachedMultiplier != 2 || spec.LongContextOutputMultiplier != 1.5 {
		t.Fatalf("显式倍率应保留, got in=%v cached=%v out=%v",
			spec.LongContextInputMultiplier, spec.LongContextCachedMultiplier, spec.LongContextOutputMultiplier)
	}
}

// TestCatalogOverlay_NonGPTNoPricingAlsoDropsTier 非 GPT 新模型即使没给 pricing(走早返回路径),
// 也不应携带 DefaultSpec 的阶梯(ToC 的 glm-5.2 无价条目即此情形)。
func TestCatalogOverlay_NonGPTNoPricingAlsoDropsTier(t *testing.T) {
	withCatalogOverlay(t, `[{"id":"glm-nopricing","name":"GLM NoPricing","context_window":1000000}]`)
	spec := Lookup("glm-nopricing")
	if spec.LongContextThreshold != 0 {
		t.Fatalf("无价的非 GPT 新模型也不应有阶梯, got threshold=%d", spec.LongContextThreshold)
	}
}

func TestCatalogOverlay_DisabledModelHiddenButBillable(t *testing.T) {
	withCatalogOverlay(t, `[{"id":"gpt-5.4-mini","enabled":false}]`)

	if !IsKnown("gpt-5.4-mini") {
		t.Fatal("隐藏模型仍应保留在 registry 中用于兼容和计费")
	}
	if spec := Lookup("gpt-5.4-mini"); spec != registry["gpt-5.4-mini"] {
		t.Fatalf("隐藏模型计费规格不应改变: %+v", spec)
	}
	for _, m := range AllModels() {
		if m.ID == "gpt-5.4-mini" {
			t.Fatal("隐藏模型不应出现在 AllModels")
		}
	}
}

func TestCatalogOverlay_MalformedJSONDoesNotReplaceSnapshot(t *testing.T) {
	withCatalogOverlay(t, `[{"id":"gpt-5.4","pricing":{"input":2,"output":10}}]`)
	before := Lookup("gpt-5.4")
	if _, err := SetCatalogOverlayJSON(`{{{`); err == nil {
		t.Fatal("非法 JSON 应返回 error")
	}
	after := Lookup("gpt-5.4")
	if before != after {
		t.Fatalf("解析失败不应替换旧快照: before=%+v after=%+v", before, after)
	}
}

func TestCatalogOverlay_ImageModelCanBeAddedAndHidden(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"new-visual-model","name":"New Visual","image_only":true,
	   "pricing":{"input":6,"cached_input":0.6,"output":30}}
	]`)

	if !IsImageOnly("new-visual-model") {
		t.Fatal("image_only=true 的新增模型应被识别为纯图像模型")
	}
	var caps string
	for _, m := range AllModels() {
		if m.ID == "new-visual-model" {
			caps = strings.Join(m.Capabilities, ",")
		}
	}
	if caps != "image_generation" {
		t.Fatalf("新增图像模型 capabilities = %q, want image_generation", caps)
	}
}

// TestCatalogOverlay_NewModelDerivesTiersFromStandard 覆盖层新增模型只填标准价时,
// priority/flex/缓存读必须从标准价推导(×2 / ×0.5 / ×0.1),
// 而不是继承关键字推断系列(gpt-5.4)的绝对价——gpt-5.6 三档卖错价事故的回归护栏(5.6 已内置,此处用虚构新 ID)。
func TestCatalogOverlay_NewModelDerivesTiersFromStandard(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.7-nova","name":"GPT 5.7 Nova","pricing":{"input":1,"output":6}}
	]`)

	spec := Lookup("gpt-5.7-nova")
	if spec.InputPrice != 1 || spec.OutputPrice != 6 {
		t.Fatalf("标准价未生效: %+v", spec)
	}
	if spec.CachedPrice != 0.1 {
		t.Fatalf("缓存读应推导为输入×0.1=0.1, got %v", spec.CachedPrice)
	}
	if spec.InputPricePriority != 2 || spec.CachedPricePriority != 0.2 || spec.OutputPricePriority != 12 {
		t.Fatalf("priority 档应推导为标准×2, got %v/%v/%v",
			spec.InputPricePriority, spec.CachedPricePriority, spec.OutputPricePriority)
	}
	if spec.InputPriceFlex != 0.5 || spec.CachedPriceFlex != 0.05 || spec.OutputPriceFlex != 3 {
		t.Fatalf("flex 档应推导为标准×0.5, got %v/%v/%v",
			spec.InputPriceFlex, spec.CachedPriceFlex, spec.OutputPriceFlex)
	}
	// 结构性字段沿用推断系列(gpt-5 关键字 → gpt-5.4 家族),长上下文阶梯保留
	base := registry["gpt-5.4"]
	if spec.LongContextThreshold != base.LongContextThreshold ||
		spec.LongContextInputMultiplier != base.LongContextInputMultiplier ||
		spec.LongContextOutputMultiplier != base.LongContextOutputMultiplier {
		t.Fatalf("长上下文阶梯应沿用推断系列: %+v", spec)
	}
}

// TestCatalogOverlay_NewModelExplicitTiersWin 显式给出的档价必须压过推导值。
func TestCatalogOverlay_NewModelExplicitTiersWin(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.7-vega","pricing":{"input":5,"cached_input":0.5,"output":30,
	   "priority_input":12.5,"priority_cached_input":1.25,"priority_output":75}}
	]`)

	spec := Lookup("gpt-5.7-vega")
	if spec.InputPricePriority != 12.5 || spec.CachedPricePriority != 1.25 || spec.OutputPricePriority != 75 {
		t.Fatalf("显式 priority 价应生效: %+v", spec)
	}
	if spec.CachedPrice != 0.5 {
		t.Fatalf("显式缓存读价应生效: %v", spec.CachedPrice)
	}
	if spec.InputPriceFlex != 2.5 || spec.OutputPriceFlex != 15 {
		t.Fatalf("未显式给出的 flex 档仍按标准×0.5 推导: %+v", spec)
	}
}

// TestCatalogOverlay_BuiltinOverrideKeepsBuiltinTiers 覆盖内置模型的单个价格时,
// 未覆盖的档价沿用内置绝对价(如 gpt-5.5 priority=×2.5),不重新推导。
func TestCatalogOverlay_BuiltinOverrideKeepsBuiltinTiers(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.5","pricing":{"output":32}}
	]`)

	spec := Lookup("gpt-5.5")
	base := registry["gpt-5.5"]
	if spec.OutputPrice != 32 {
		t.Fatalf("显式 output 未生效: %+v", spec)
	}
	if spec.InputPricePriority != base.InputPricePriority || spec.OutputPricePriority != base.OutputPricePriority {
		t.Fatalf("内置模型 priority 档不应被推导改写: got %v/%v want %v/%v",
			spec.InputPricePriority, spec.OutputPricePriority, base.InputPricePriority, base.OutputPricePriority)
	}
}

// TestCatalogOverlay_NewModelWithoutStandardKeepsInferred 条目没给标准价时维持既有行为:
// 整体沿用关键字推断系列。
func TestCatalogOverlay_NewModelWithoutStandardKeepsInferred(t *testing.T) {
	withCatalogOverlay(t, `[
	  {"id":"gpt-5.7-rhea","name":"GPT 5.7 Rhea"}
	]`)

	spec := Lookup("gpt-5.7-rhea")
	base := registry["gpt-5.4"]
	if spec.InputPrice != base.InputPrice || spec.OutputPrice != base.OutputPrice ||
		spec.InputPricePriority != base.InputPricePriority {
		t.Fatalf("无标准价条目应沿用推断系列绝对价: %+v", spec)
	}
	if spec.Name != "GPT 5.7 Rhea" {
		t.Fatalf("显示名应生效: %q", spec.Name)
	}
}

// TestBuiltinGPT56Family 锁定 5.6 三档的内置价格与长上下文规则。
func TestBuiltinGPT56Family(t *testing.T) {
	ResetCatalogOverlay()
	cases := []struct {
		id                    string
		input, cached, output float64
	}{
		{"gpt-5.6-sol", 5, 0.5, 30},
		{"gpt-5.6-terra", 2.5, 0.25, 15},
		{"gpt-5.6-luna", 1, 0.1, 6},
	}
	for _, c := range cases {
		spec, ok := registry[c.id]
		if !ok {
			t.Fatalf("%s 应在内置注册表", c.id)
		}
		if spec.InputPrice != c.input || spec.CachedPrice != c.cached || spec.OutputPrice != c.output {
			t.Errorf("%s 价格 = %v/%v/%v, want %v/%v/%v", c.id,
				spec.InputPrice, spec.CachedPrice, spec.OutputPrice, c.input, c.cached, c.output)
		}
		if spec.InputPricePriority != c.input*2 || spec.InputPriceFlex != c.input*0.5 {
			t.Errorf("%s priority/flex 档应为标准×2/×0.5", c.id)
		}
		if spec.LongContextThreshold != 272000 || spec.LongContextInputMultiplier != 2 ||
			spec.LongContextOutputMultiplier != 1.5 {
			t.Errorf("%s 长上下文阶梯缺失: %+v", c.id, spec)
		}
		if spec.ContextWindow != 1050000 || spec.MaxOutputTokens != 128000 {
			t.Errorf("%s 上下文规格错误: %d/%d", c.id, spec.ContextWindow, spec.MaxOutputTokens)
		}
	}
	if spec := registry["gpt-5.4"]; spec.LongContextThreshold != 0 {
		t.Fatalf("gpt-5.4 不应启用 272K 长上下文阶梯: %+v", spec)
	}
	if _, ok := registry["gpt-5.6"]; ok {
		t.Fatal("不得注册裸 gpt-5.6；只允许 luna / sol / terra 三个规格")
	}
}

// TestBuiltinGeminiImagePricing 锁定 Azure Gemini 生图线路的逐模型基准价。
// 这些模型曾因共用 imgSpec 被全部误设为 GPT-5.5 的 5/0.5/30。
func TestBuiltinGeminiImagePricing(t *testing.T) {
	ResetCatalogOverlay()
	cases := []struct {
		id                    string
		input, cached, output float64
	}{
		// output = Google 官方**图像输出价**(非文本输出价);见 registry.go 注释。
		{"gemini-2.5-flash-image", 0.3, 0.03, 30},
		{"gemini-3-pro-image", 2, 0.2, 120},
		{"gemini-3-pro-image-c", 2, 0.2, 120},
		{"gemini-3-pro-image-preview", 2, 0.2, 120},
		{"gemini-3-pro-image-preview-c", 2, 0.2, 120},
		{"gemini-3.1-flash-image", 0.5, 0.05, 60},
		{"gemini-3.1-flash-image-c", 0.5, 0.05, 60},
		{"gemini-3.1-flash-image-preview", 0.5, 0.05, 60},
		{"gemini-3.1-flash-image-preview-c", 0.5, 0.05, 60},
		{"gemini-3.1-flash-lite-image", 0.25, 0.025, 30},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			spec := Lookup(c.id)
			if !spec.ImageOnly {
				t.Fatal("Gemini 生图模型必须标记为 ImageOnly")
			}
			if spec.InputPrice != c.input || spec.CachedPrice != c.cached || spec.OutputPrice != c.output {
				t.Fatalf("标准价 = %v/%v/%v, want %v/%v/%v",
					spec.InputPrice, spec.CachedPrice, spec.OutputPrice, c.input, c.cached, c.output)
			}
			if spec.InputPricePriority != c.input*2 || spec.CachedPricePriority != c.cached*2 || spec.OutputPricePriority != c.output*2 {
				t.Fatalf("priority 价未按标准价 x2 派生: %+v", spec)
			}
			if spec.InputPriceFlex != c.input*0.5 || spec.CachedPriceFlex != c.cached*0.5 || spec.OutputPriceFlex != c.output*0.5 {
				t.Fatalf("flex 价未按标准价 x0.5 派生: %+v", spec)
			}
		})
	}
}
