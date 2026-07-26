package model

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────
// 集中模型注册表
// 新增模型只需在 registry 中加一行，所有引用点自动生效
// ──────────────────────────────────────────────────────

// Spec 单个模型的完整元数据
//
// 定价对齐 OpenAI 官方规则：
//   - 标准档：Input / Cached / Output
//   - Priority 档：*Priority 字段（通常标准 × 2；gpt-5.5 为 × 2.5），缺省时 SDK 以 × 2 兜底
//   - Flex / Batch 档：*Flex 字段（= 标准 × 0.5），缺省时 SDK 以 × 0.5 兜底
//   - 长上下文档（仅 gpt-5.6 家族）：完整 input_tokens 超过 LongContextThreshold
//     时，整次请求全量按倍率计费
type Spec struct {
	Name            string
	ContextWindow   int
	MaxOutputTokens int

	// 标准档单价（$/1M tokens）
	InputPrice  float64
	CachedPrice float64
	OutputPrice float64

	// ImageOnly 标记纯图像生成模型。Responses image_generation 产生的图像 token
	// 默认按对话模型价格计费；纯图像接口没有对话模型时回退到 gpt-5.5 价格。
	ImageOnly bool

	// Priority 档单价（$/1M tokens）。零值表示未配置，由 SDK 以标准 × 2 兜底。
	InputPricePriority  float64
	CachedPricePriority float64
	OutputPricePriority float64

	// Fast 档单价（$/1M tokens）。当前未使用，保持零值。
	InputPriceFast  float64
	CachedPriceFast float64
	OutputPriceFast float64

	// Flex / Batch 档单价（$/1M tokens）。零值表示未配置，由 SDK 以标准 × 0.5 兜底。
	InputPriceFlex  float64
	CachedPriceFlex float64
	OutputPriceFlex float64

	// 长上下文阶梯（只对 gpt-5.6 家族填非零值）。
	LongContextThreshold        int
	LongContextInputMultiplier  float64
	LongContextOutputMultiplier float64
	LongContextCachedMultiplier float64
}

// std 快捷构造 standard / priority / flex 价格齐全的 Spec，
// 倍率按 OpenAI 官方：priority = 2×standard，flex = 0.5×standard。
func std(name string, ctx, maxOut int, input, cached, output float64) Spec {
	return Spec{
		Name:                name,
		ContextWindow:       ctx,
		MaxOutputTokens:     maxOut,
		InputPrice:          input,
		CachedPrice:         cached,
		OutputPrice:         output,
		InputPricePriority:  input * 2,
		CachedPricePriority: cached * 2,
		OutputPricePriority: output * 2,
		InputPriceFlex:      input * 0.5,
		CachedPriceFlex:     cached * 0.5,
		OutputPriceFlex:     output * 0.5,
	}
}

func withPriorityMultiplier(s Spec, multiplier float64) Spec {
	s.InputPricePriority = s.InputPrice * multiplier
	s.CachedPricePriority = s.CachedPrice * multiplier
	s.OutputPricePriority = s.OutputPrice * multiplier
	return s
}

// pricedImageSpec 构造带独立 token 价格的纯图像模型 Spec。图像输出成本在
// gateway 层会归入 image cost，便于 Core 配置固定图价时覆盖。
func pricedImageSpec(name string, input, cached, output float64) Spec {
	s := std(name, 32000, 0, input, cached, output)
	s.ImageOnly = true
	return s
}

// imgSpec 保留 GPT Image 的默认 token fallback 口径。
func imgSpec(name string) Spec {
	return pricedImageSpec(name, 5.0, 0.5, 30.0)
}

// withLongCtx 在已构造的 Spec 基础上附加 GPT-5.6 长上下文阶梯。
// OpenAI 官方：input ×2、cached ×2、output ×1.5，阈值 272k input_tokens。
func withLongCtx(s Spec) Spec {
	s.LongContextThreshold = 272_000
	s.LongContextInputMultiplier = 2.0
	s.LongContextOutputMultiplier = 1.5
	s.LongContextCachedMultiplier = 2.0
	return s
}

// registry 内置模型注册表（按模型 ID 索引），运行时可被后台模型目录覆盖层叠加。
// ─── 新增模型只需在此处加一行 ───
//
// 注意：Claude 系列模型（claude-opus-*、claude-sonnet-*、claude-haiku-*）不在此注册。
// 它们由客户端经 /v1/messages Anthropic 协议翻译入口传入，插件内部映射为 GPT 模型
// 后再调用上游。Core 调度层通过 scheduling_model.go 的硬编码回退处理映射。
// 若将来需要插件声明此映射，可在 toModelInfo 中为对应模型设置
// Metadata["scheduling_model"]，Core 会优先读取该元数据。
var registry = map[string]Spec{
	// ── GPT-5.6 家族(2026-07-09 GA):三档同为 1.05M 上下文,>272K 输入整笔 ×2 in / ×1.5 out ──
	// 官方价 2026-07-11 核实:Sol $5/$30、Terra $2.5/$15、Luna $1/$6,缓存读=输入×10%。
	"gpt-5.6-sol":   withLongCtx(std("GPT 5.6 Sol", 1050000, 128000, 5.0, 0.5, 30.0)),
	"gpt-5.6-terra": withLongCtx(std("GPT 5.6 Terra", 1050000, 128000, 2.5, 0.25, 15.0)),
	"gpt-5.6-luna":  withLongCtx(std("GPT 5.6 Luna", 1050000, 128000, 1.0, 0.1, 6.0)),

	"gpt-5.5": withPriorityMultiplier(std("GPT 5.5", 400000, 128000, 5.0, 0.5, 30.0), 2.5),

	// ── GPT-5.4 ──
	"gpt-5.4": std("GPT 5.4", 272000, 128000, 2.5, 0.25, 15.0),

	// ── Codex / GPT 轻量系列 ──
	"gpt-5.3-codex-spark": std("GPT 5.3 Codex Spark", 128000, 128000, 1.75, 0.175, 14.0),
	"gpt-5.4-mini":        std("GPT 5.4 Mini", 128000, 128000, 0.75, 0.075, 4.5),

	// ── 图像生成（默认按对话模型 token 价格计费；固定价由 Core 配置覆盖）──
	"gpt-image-1":   imgSpec("GPT Image 1"),
	"gpt-image-1.5": imgSpec("GPT Image 1.5"),
	"gpt-image-2":   imgSpec("GPT Image 2"),

	// ── OpenAI-compatible Gemini image relays（Nano Banana 系列）──
	// Azure Gemini 分组使用同一组官方模型基准价，再由 Core 套分组倍率。
	// -c 是协议变体，与对应非 -c 型号同价。
	// ⚠️ output 必须填 Google 的**图像输出价**($120/$60/$30),不是文本输出价
	// ($12/$3/$1.5)——生图请求的输出 tokens 走 prices.output 计费(见 outcome.go
	// addUsageCostForModel)。2026-07-26 前误填文本价,生产少收 10–20 倍。
	// 官方依据(直读):https://ai.google.dev/gemini-api/docs/pricing
	//   3 Pro Image  $120/1M(1120 tok/张 = $0.134/张)
	//   3.1 Flash    $60/1M (1120 tok/张 = $0.067/张)
	//   3.1 Lite     $30/1M (1120 tok/张 = $0.0336/张)
	//   2.5 Flash    $30/1M (1290 tok/张 = $0.039/张)
	// cached 价 Google 未发布(这些模型无 context caching 行),沿用输入价 10% 惯例。
	"gemini-2.5-flash-image":           pricedImageSpec("Gemini 2.5 Flash Image", 0.3, 0.03, 30.0),
	"gemini-3-pro-image":               pricedImageSpec("Gemini 3 Pro Image", 2.0, 0.2, 120.0),
	"gemini-3-pro-image-c":             pricedImageSpec("Gemini 3 Pro Image C", 2.0, 0.2, 120.0),
	"gemini-3-pro-image-preview":       pricedImageSpec("Gemini 3 Pro Image Preview", 2.0, 0.2, 120.0),
	"gemini-3-pro-image-preview-c":     pricedImageSpec("Gemini 3 Pro Image Preview C", 2.0, 0.2, 120.0),
	"gemini-3.1-flash-image":           pricedImageSpec("Gemini 3.1 Flash Image", 0.5, 0.05, 60.0),
	"gemini-3.1-flash-image-c":         pricedImageSpec("Gemini 3.1 Flash Image C", 0.5, 0.05, 60.0),
	"gemini-3.1-flash-image-preview":   pricedImageSpec("Gemini 3.1 Flash Image Preview", 0.5, 0.05, 60.0),
	"gemini-3.1-flash-image-preview-c": pricedImageSpec("Gemini 3.1 Flash Image Preview C", 0.5, 0.05, 60.0),
	"gemini-3.1-flash-lite-image":      pricedImageSpec("Gemini 3.1 Flash Lite Image", 0.25, 0.025, 30.0),
}

// DefaultSpec 未注册模型的最终兜底值。按 gpt-5.4 标准档计价——宁可略高也不能 0。
// （0 价格会导致免费流量，之前一个 bug 来源。）
var DefaultSpec = std("Unknown (billed as gpt-5.4)", 272000, 128000, 2.5, 0.25, 15.0)

// Lookup 查询模型元数据。未命中注册表时按关键字推断到最接近的系列，仍无法匹配再落 DefaultSpec。
//
// 这避免了"客户端请求未知模型 → Spec 全 0 → cost=0 免费使用"的坑：只要能看出系列
// （mini / codex / image / gpt-5 等），就按对应系列定价；彻底不认识的兜底到 GPT-5.4 标准价。
func Lookup(modelID string) Spec {
	reg := activeRegistry()
	id := strings.ToLower(strings.TrimSpace(modelID))
	if spec, ok := reg[id]; ok {
		return spec
	}
	if spec, ok := fallbackByKeyword(id, reg); ok {
		warnPricingFallbackOnce(id, spec.Name)
		return spec
	}
	warnPricingFallbackOnce(id, DefaultSpec.Name)
	return DefaultSpec
}

// 兜底计费告警去重表。上限防被垃圾模型名撑爆内存;到达上限后不再新增告警(已告警的仍去重)。
const pricingFallbackWarnCap = 512

var (
	pricingFallbackWarnMu sync.Mutex
	pricingFallbackWarned = map[string]struct{}{}
)

// warnPricingFallbackOnce 未注册模型按推断/兜底价计费时告警一次(按模型去重)。
// gpt-5.6 三档按 gpt-5.4 价静默卖了一天才被人工发现——这条日志就是那次事故的探测器:
// 看到它就该去后台「模型目录」给该模型配官方价。
func warnPricingFallbackOnce(modelID, billedAs string) {
	pricingFallbackWarnMu.Lock()
	_, seen := pricingFallbackWarned[modelID]
	full := len(pricingFallbackWarned) >= pricingFallbackWarnCap
	if !seen && !full {
		pricingFallbackWarned[modelID] = struct{}{}
	}
	pricingFallbackWarnMu.Unlock()
	if seen || full {
		return
	}
	slog.Warn("model_pricing_fallback",
		"model", modelID,
		"billed_as", billedAs,
		"hint", "未注册模型正按推断价计费,请到后台「模型目录」为其配置官方价",
	)
}

// fallbackByKeyword 从模型 ID 关键字推断最接近的已注册系列。未命中返回 (_, false)。
func fallbackByKeyword(id string, reg map[string]Spec) (Spec, bool) {
	if id == "" {
		return Spec{}, false
	}
	// 顺序敏感：先细分（codex / mini / image）后粗分（gpt-5 / gpt-4）
	switch {
	case strings.Contains(id, "codex"):
		return reg["gpt-5.4"], true
	case strings.Contains(id, "image"):
		return reg["gpt-image-1.5"], true
	case strings.Contains(id, "mini") || strings.Contains(id, "nano"):
		return reg["gpt-5.4-mini"], true
	case strings.Contains(id, "gpt-5") || strings.HasPrefix(id, "gpt5") ||
		strings.Contains(id, "o1") || strings.Contains(id, "o3") || strings.Contains(id, "o4"):
		return reg["gpt-5.4"], true
	case strings.Contains(id, "gpt-4") || strings.HasPrefix(id, "gpt4"):
		// gpt-4 系列未显式注册，按 gpt-5.4 标准价计（偏保守）
		return reg["gpt-5.4"], true
	}
	return Spec{}, false
}

// IsImageOnly 判断给定 model 是否为纯图像生成模型。
func IsImageOnly(modelID string) bool {
	return Lookup(modelID).ImageOnly
}

// IsKnown 判断给定 model ID 是否在注册表内（大小写不敏感、忽略首尾空白）。
// 用于请求入口的 model 兜底：未注册的 model 会被换成默认值，
// 避免把"不支持的模型"推到上游账号。
func IsKnown(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := activeRegistry()[id]
	return ok
}

// AllSpecs 返回注册模型的 SDK ModelInfo 列表（按 ID 排序）。
// includeImages=true 时返回对话模型和图像模型，false 时只返回对话模型。
func AllSpecs(includeImages bool) []sdk.ModelInfo {
	reg := activeRegistry()
	hidden := activeHiddenModels()
	models := make([]sdk.ModelInfo, 0, len(reg))
	for id, spec := range reg {
		if hidden[id] {
			continue
		}
		isImage := spec.ImageOnly
		if isImage && !includeImages {
			continue
		}
		models = append(models, toModelInfo(id, spec))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models
}

// AllModels 返回当前对外可见模型，用于插件运行时声明与本地 /v1/models。
func AllModels() []sdk.ModelInfo {
	return AllSpecs(true)
}

// AllPricingSpecs 返回所有注册模型的插件私有规格（按 ID 排序）。
//
// SDK 的 ModelInfo 不承载价格；manifest 如需展示标准价格，应从这里读取插件自己的
// 计费规格，而不是把价格重新塞回 SDK 结构。
func AllPricingSpecs() []NamedSpec {
	reg := activeRegistry()
	items := make([]NamedSpec, 0, len(reg))
	for id, spec := range reg {
		items = append(items, NamedSpec{ID: id, Spec: spec})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

// NamedSpec 是带模型 ID 的插件私有规格。
type NamedSpec struct {
	ID   string
	Spec Spec
}

// toModelInfo 将内部 Spec 映射为 SDK ModelInfo。
// 若模型需要 Core 调度层使用不同的模型进行账号选择，可设置
// Metadata["scheduling_model"]，Core 会优先采纳（见 core scheduling_model.go）。
// 图像生成模型声明 Metadata["family"]="gpt-image"，使 Core 按家族维度做限流冷却，
// 避免 gpt-image 撞 4000/min 时误伤同账号上的 chat 模型。
func toModelInfo(id string, spec Spec) sdk.ModelInfo {
	mi := sdk.ModelInfo{
		ID:              id,
		Name:            spec.Name,
		ContextWindow:   spec.ContextWindow,
		MaxOutputTokens: spec.MaxOutputTokens,
		Capabilities:    modelCapabilities(spec),
	}
	if spec.ImageOnly {
		mi.Metadata = map[string]string{"family": "gpt-image"}
	}
	mi.Metadata = priceMetadata(spec, mi.Metadata)
	mi.Metadata["vendor"] = vendorForModel(id)
	return mi
}

// vendorForModel 按模型 ID 推断厂商标识(metadata 约定键 "vendor")。
// openai 平台经 OpenAI 兼容协议中继第三方厂商模型(gemini/glm 等):
// 平台标识表达的是接入协议,vendor 表达模型出品方,供目录展示端区分两者。
func vendorForModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(id, "gemini"), strings.HasPrefix(id, "imagen"):
		return "google"
	case strings.HasPrefix(id, "glm"):
		return "zhipu"
	default:
		return "openai"
	}
}

// priceMetadata 把内置基础价编进 ModelInfo.Metadata 的 price.* / long_context.* 键。
//
// 唯一消费方是 core 后台「模型目录」编辑器（展示各模型的内置地板价与结构默认值，
// 供管理员对照改价）。计费不读这里——仍由 Forward 按插件私有 Spec 计算，manifest
// 展示价也仍走 AllPricingSpecs。字符串值用 FormatFloat -1 精度，无损往返。
func priceMetadata(spec Spec, meta map[string]string) map[string]string {
	if meta == nil {
		meta = make(map[string]string, 8)
	}
	put := func(key string, v float64) {
		if v > 0 {
			meta[key] = strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	put("price.input", spec.InputPrice)
	put("price.cached_input", spec.CachedPrice)
	put("price.output", spec.OutputPrice)
	put("price.priority_input", spec.InputPricePriority)
	put("price.priority_cached_input", spec.CachedPricePriority)
	put("price.priority_output", spec.OutputPricePriority)
	put("price.flex_input", spec.InputPriceFlex)
	put("price.flex_cached_input", spec.CachedPriceFlex)
	put("price.flex_output", spec.OutputPriceFlex)
	if spec.LongContextThreshold > 0 {
		meta["long_context.threshold"] = strconv.Itoa(spec.LongContextThreshold)
		put("long_context.input_multiplier", spec.LongContextInputMultiplier)
		put("long_context.cached_multiplier", spec.LongContextCachedMultiplier)
		put("long_context.output_multiplier", spec.LongContextOutputMultiplier)
	}
	return meta
}

func modelCapabilities(spec Spec) []string {
	if spec.ImageOnly {
		return []string{sdk.ModelCapImageGeneration}
	}
	return []string{sdk.ModelCapChat, sdk.ModelCapReasoning}
}
