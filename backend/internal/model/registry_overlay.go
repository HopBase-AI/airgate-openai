package model

import (
	"encoding/json"
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

type overlayEntry struct {
	ID            string              `json:"id"`
	Name          string              `json:"name,omitempty"`
	ContextWindow int                 `json:"context_window,omitempty"`
	MaxOutput     int                 `json:"max_output_tokens,omitempty"`
	Enabled       *bool               `json:"enabled,omitempty"`
	ImageOnly     *bool               `json:"image_only,omitempty"`
	Pricing       *overlayPricing     `json:"pricing,omitempty"`
	LongContext   *overlayLongContext `json:"long_context,omitempty"`
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
		eff[id] = applyOverlay(base, e)
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

func applyOverlay(base Spec, e overlayEntry) Spec {
	if e.Name != "" {
		base.Name = e.Name
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
	return base
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
