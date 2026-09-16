package model

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"sync/atomic"
)

// CatalogStats 描述一次模型目录覆盖层应用后的有效规模。
type CatalogStats struct {
	RegistrySize int
	HiddenSize   int
}

type catalogOverlay struct {
	registry map[string]Spec
	hidden   map[string]bool
}

var overlayStore atomic.Pointer[catalogOverlay]

// IsRetired reports model IDs that must not be restored by a stale Core catalog.
// These IDs remain recognizable to the gateway so requests can fail closed before
// reaching an unintended upstream route.
func IsRetired(modelID string) bool {
	switch normalizeID(modelID) {
	case "deepseek-v4-flash":
		return true
	default:
		return false
	}
}

func activeRegistry() map[string]Spec {
	if ov := overlayStore.Load(); ov != nil {
		return ov.registry
	}
	return registry
}

func activeHiddenModels() map[string]bool {
	if ov := overlayStore.Load(); ov != nil {
		return ov.hidden
	}
	return nil
}

// ResetCatalogOverlay 清空运行时覆盖层，恢复纯内置注册表。
func ResetCatalogOverlay() {
	overlayStore.Store(nil)
}

// SetCatalogOverlayJSON 解析并应用后台配置的模型目录覆盖层。
//
// 空字符串表示空覆盖层。非法 JSON 返回错误且不修改当前快照；调用方可安全保留旧配置。
func SetCatalogOverlayJSON(raw string) (CatalogStats, error) {
	ov, err := parseCatalogOverlay(raw)
	if err != nil {
		return CatalogStats{}, err
	}
	overlayStore.Store(ov)
	return CatalogStats{RegistrySize: len(ov.registry), HiddenSize: len(ov.hidden)}, nil
}

type overlayPricing struct {
	Input               float64 `json:"input"`
	CachedInput         float64 `json:"cached_input"`
	Output              float64 `json:"output"`
	PriorityInput       float64 `json:"priority_input"`
	PriorityCachedInput float64 `json:"priority_cached_input"`
	PriorityOutput      float64 `json:"priority_output"`
	FlexInput           float64 `json:"flex_input"`
	FlexCachedInput     float64 `json:"flex_cached_input"`
	FlexOutput          float64 `json:"flex_output"`
}

type overlayLongContext struct {
	Threshold        int     `json:"threshold"`
	InputMultiplier  float64 `json:"input_multiplier"`
	CachedMultiplier float64 `json:"cached_multiplier"`
	OutputMultiplier float64 `json:"output_multiplier"`
}

// overlayListPrice 覆盖层条目的厂商官方牌价（原币）。
//
//	"list_price":{"currency":"CNY","fx":6.8,"input":12,"cached_input":2.4,"output":36}
//
// 恒等式 <k> ÷ fx ≈ pricing.<k>（USD 基准价）。core 写入侧会按此拒写；插件侧
// 只兜底：不一致记 WARN 仍加载（牌价纯展示，不参与计费，不值得为它拒掉整条价格）。
type overlayListPrice struct {
	Currency    string  `json:"currency"`
	FX          float64 `json:"fx"`
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	Output      float64 `json:"output"`
}

type overlayEntry struct {
	ID            string              `json:"id"`
	Name          string              `json:"name,omitempty"`
	ContextWindow int                 `json:"context_window,omitempty"`
	MaxOutput     int                 `json:"max_output_tokens,omitempty"`
	Enabled       *bool               `json:"enabled,omitempty"`
	ImageOnly     *bool               `json:"image_only,omitempty"`
	Pricing       *overlayPricing     `json:"pricing,omitempty"`
	LongContext   *overlayLongContext `json:"long_context,omitempty"`
	ListPrice     *overlayListPrice   `json:"list_price,omitempty"`
	// Vendor 厂商标识（metadata 约定键 "vendor"）；零插件模型（qwen / kimi 等）
	// 关键字推断不到，靠这里补正。空 = 沿用推断。
	Vendor string `json:"vendor,omitempty"`
}

func parseCatalogOverlay(raw string) (*catalogOverlay, error) {
	eff := cloneRegistry(registry)
	hidden := map[string]bool{}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return &catalogOverlay{registry: eff, hidden: hidden}, nil
	}

	var entries []overlayEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, err
	}
	for _, e := range entries {
		id := normalizeID(e.ID)
		if id == "" || IsRetired(id) {
			continue
		}
		base, ok := eff[id]
		if !ok {
			base = inferNewModelBase(id, e, eff)
		}
		eff[id] = applyOverlay(id, base, e)
		if e.Enabled != nil && !*e.Enabled {
			hidden[id] = true
		} else {
			delete(hidden, id)
		}
	}
	return &catalogOverlay{registry: eff, hidden: hidden}, nil
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// inferNewModelBase 为覆盖层"新增"(非内置)模型构造基底。
//
// 结构性字段(上下文窗口、长上下文阶梯、图像标记)沿用关键字推断的最接近系列;
// 价格字段若条目给出了标准档 input+output,则按官方惯例从标准价推导缺省档:
// 缓存读=输入×0.1、priority=标准×2、flex=标准×0.5(与 std() 构造惯例一致)。
// 不能直接继承推断系列的"绝对价"——那会把 gpt-5.4 的 priority/flex/缓存价
// 错按到价位完全不同的新模型头上(gpt-5.6 三档卖错价事故的同源坑)。
// 条目未给标准价时维持既有行为:整体沿用推断系列,兜底 DefaultSpec。
func inferNewModelBase(id string, e overlayEntry, reg map[string]Spec) Spec {
	base, matched := fallbackByKeyword(id, reg)
	if !matched {
		base = DefaultSpec
		// 长上下文阶梯(272K 后 ×2 in/×1.5 out)是 OpenAI GPT 家族的计费特性。
		// 未按关键字匹配到任何内置 GPT 系列的"新增"模型(如 GLM 等第三方模型),
		// 不应从 DefaultSpec 继承该阶梯——否则会给它们错误地套用 GPT 的长上下文倍率计费。
		// 确需阶梯的新模型可在 overlay 条目里用 long_context 显式声明(applyOverlay 会应用)。
		base.LongContextThreshold = 0
		base.LongContextInputMultiplier = 0
		base.LongContextOutputMultiplier = 0
		base.LongContextCachedMultiplier = 0
	}
	if e.Pricing == nil || e.Pricing.Input <= 0 || e.Pricing.Output <= 0 {
		return base
	}
	cached := e.Pricing.CachedInput
	if cached <= 0 {
		cached = e.Pricing.Input * 0.1
	}
	derived := std(base.Name, base.ContextWindow, base.MaxOutputTokens, e.Pricing.Input, cached, e.Pricing.Output)
	derived.ImageOnly = base.ImageOnly
	derived.LongContextThreshold = base.LongContextThreshold
	derived.LongContextInputMultiplier = base.LongContextInputMultiplier
	derived.LongContextOutputMultiplier = base.LongContextOutputMultiplier
	derived.LongContextCachedMultiplier = base.LongContextCachedMultiplier
	return derived
}

func cloneRegistry(src map[string]Spec) map[string]Spec {
	out := make(map[string]Spec, len(src))
	for id, spec := range src {
		out[id] = spec
	}
	return out
}

func applyOverlay(id string, base Spec, e overlayEntry) Spec {
	if e.Name != "" {
		base.Name = e.Name
	}
	if vendor := strings.TrimSpace(e.Vendor); vendor != "" {
		base.Vendor = vendor
	}
	if e.ContextWindow > 0 {
		base.ContextWindow = e.ContextWindow
	}
	if e.MaxOutput > 0 {
		base.MaxOutputTokens = e.MaxOutput
	}
	if e.ImageOnly != nil {
		base.ImageOnly = *e.ImageOnly
	}
	if e.Pricing != nil {
		applyPricingOverlay(&base, *e.Pricing)
	}
	if e.LongContext != nil {
		applyLongContextOverlay(&base, *e.LongContext)
	}
	if e.ListPrice != nil {
		applyListPriceOverlay(&base, id, *e.ListPrice)
	}
	return base
}

// listPriceTolerance 牌价恒等式的相对容差。覆盖层 USD 基准价通常只写 4 位小数
// （12 ÷ 6.8 = 1.76470588… 录成 1.7647），绝对 1e-6 会把合法录入全判成不一致。
const listPriceTolerance = 1e-3

// applyListPriceOverlay 把覆盖层牌价映射进 Spec.ListPrice，并按恒等式
// list.<k> ÷ fx ≈ 基准价 校验：不一致只告警仍加载（core 写入侧才是拒写闸门）。
// 币种或折算率缺失则整体丢弃——没有折算率的原币数字无法验算，写出去只会误导。
func applyListPriceOverlay(spec *Spec, id string, p overlayListPrice) {
	currency := strings.ToUpper(strings.TrimSpace(p.Currency))
	if currency == "" || p.FX <= 0 {
		slog.Warn("model_list_price_invalid",
			"model", id, "currency", p.Currency, "fx", p.FX,
			"reason", "currency and fx are required")
		return
	}
	lp := ListPrice{
		Currency:    currency,
		FX:          p.FX,
		Input:       math.Max(p.Input, 0),
		CachedInput: math.Max(p.CachedInput, 0),
		Output:      math.Max(p.Output, 0),
	}
	for _, check := range []struct {
		field string
		list  float64
		base  float64
	}{
		{"input", lp.Input, spec.InputPrice},
		{"cached_input", lp.CachedInput, spec.CachedPrice},
		{"output", lp.Output, spec.OutputPrice},
	} {
		if check.list <= 0 || check.base <= 0 {
			continue
		}
		implied := check.list / lp.FX
		if math.Abs(implied-check.base) > check.base*listPriceTolerance {
			slog.Warn("model_list_price_mismatch",
				"model", id, "field", check.field, "currency", currency,
				"list", check.list, "fx", lp.FX, "implied_usd", implied, "base_usd", check.base)
		}
	}
	spec.ListPrice = lp
}

func applyPricingOverlay(spec *Spec, p overlayPricing) {
	if p.Input > 0 {
		spec.InputPrice = p.Input
	}
	if p.CachedInput > 0 {
		spec.CachedPrice = p.CachedInput
	}
	if p.Output > 0 {
		spec.OutputPrice = p.Output
	}
	if p.PriorityInput > 0 {
		spec.InputPricePriority = p.PriorityInput
	}
	if p.PriorityCachedInput > 0 {
		spec.CachedPricePriority = p.PriorityCachedInput
	}
	if p.PriorityOutput > 0 {
		spec.OutputPricePriority = p.PriorityOutput
	}
	if p.FlexInput > 0 {
		spec.InputPriceFlex = p.FlexInput
	}
	if p.FlexCachedInput > 0 {
		spec.CachedPriceFlex = p.FlexCachedInput
	}
	if p.FlexOutput > 0 {
		spec.OutputPriceFlex = p.FlexOutput
	}
}

func applyLongContextOverlay(spec *Spec, p overlayLongContext) {
	if p.Threshold > 0 {
		spec.LongContextThreshold = p.Threshold
	}
	if p.InputMultiplier > 0 {
		spec.LongContextInputMultiplier = p.InputMultiplier
	}
	if p.CachedMultiplier > 0 {
		spec.LongContextCachedMultiplier = p.CachedMultiplier
	}
	if p.OutputMultiplier > 0 {
		spec.LongContextOutputMultiplier = p.OutputMultiplier
	}
}
