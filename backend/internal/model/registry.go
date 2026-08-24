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

	// ImageUnit 官方单张牌价（$/张），按分辨率档位。
	// 只填该模型真实出得了的档位——档位须与 gateway 的 imageModelSupportedSizes
	// 对齐，否则模型广场会给出不了 4K 的模型标 4K 价。
	// 纯展示锚点：计费仍走 Forward 的 token 口径，不读这里。
	ImageUnit ImageUnitPrice

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

	// OutputExcludesReasoning 标记上游 usage 的 completion_tokens 不含
	// reasoning tokens（xAI Grok 口径，与 OpenAI 相反）。计费时须把
	// completion_tokens_details.reasoning_tokens 并入输出，否则推理型
	// 回复的输出费几乎全漏（实测一条回复 completion=1、reasoning=158）。
	OutputExcludesReasoning bool

	// ImagePerUnitBilling 标记按「张 × 分辨率档」计费的图像模型（xAI Grok
	// Imagine 口径：响应体没有任何 token usage，只能按张计费）。计费读
	// ImageUnit 档位单价与 ImageInputUnitPrice，token 价仅作异常兜底。
	ImagePerUnitBilling bool

	// ImageInputUnitPrice 官方每张输入参考图单价（$/张），仅按张计费模型使用。
	ImageInputUnitPrice float64
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

// ImageUnitPrice 官方单张牌价（$/张）按分辨率档位。0 = 该模型出不了这个档位，
// 模型广场不会为它铺价。刻意用可比较的定长结构而非 map：Spec 要保持可比较。
type ImageUnitPrice struct {
	OneK  float64
	TwoK  float64
	FourK float64
}

// Tiers 按 core 的 price.image.<档位> 约定铺开非零档位（1k / 2k / 4k）。
func (p ImageUnitPrice) Tiers() []struct {
	Tier  string
	Price float64
} {
	all := []struct {
		Tier  string
		Price float64
	}{{"1k", p.OneK}, {"2k", p.TwoK}, {"4k", p.FourK}}
	out := all[:0:0]
	for _, tier := range all {
		if tier.Price > 0 {
			out = append(out, tier)
		}
	}
	return out
}

// withImageUnitPrices 给纯图像模型附上官方单张牌价（$/张）。
// 计算口径：Google 按「每张图固定的图像 output token 数 × output 单价」计价，
// 1K / 2K 同为 1120 token、4K 为 2000 token（gemini-2.5-flash-image 为 1290）。
// 该口径复现了 Google 公布的每张价（3-pro-image 1K/2K $0.134、4K $0.24；
// 2.5-flash-image $0.039），生产 usage_metrics 的 image_cost 也与之逐条吻合。
func withImageUnitPrices(s Spec, oneK, twoK, fourK float64) Spec {
	s.ImageUnit = ImageUnitPrice{OneK: oneK, TwoK: twoK, FourK: fourK}
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

// grokChat 构造 xAI Grok 对话模型的标准价（$/1M tokens，xAI 官方牌价）。
// 该上游没有 priority/flex 档，不能使用 std() 自动派生不存在的价格。
// xAI 官方长上下文阶梯：提示 ≥200K tokens 时整笔请求 input/cached/output 全部 ×2
// （docs.x.ai 2026-08 口径，中继报价与之逐项吻合）。
// completion_tokens 不含 reasoning tokens，须置 OutputExcludesReasoning。
func grokChat(name string, ctx int, input, cached, output float64) Spec {
	return Spec{
		Name:                        name,
		ContextWindow:               ctx,
		InputPrice:                  input,
		CachedPrice:                 cached,
		OutputPrice:                 output,
		LongContextThreshold:        200_000,
		LongContextInputMultiplier:  2.0,
		LongContextOutputMultiplier: 2.0,
		LongContextCachedMultiplier: 2.0,
		OutputExcludesReasoning:     true,
	}
}

// grokImage 构造 xAI Grok Imagine 图像模型（按张计费）。
// oneK / twoK 为官方每张输出单价（$/张，按 resolution 档），inputPerImage 为
// 每张输入参考图单价。上游响应没有 token usage，token 价填 GPT Image 兜底值
// 仅防异常路径漏价，正常计费一律走按张分支。
func grokImage(name string, oneK, twoK, inputPerImage float64) Spec {
	s := pricedImageSpec(name, 5.0, 0.5, 30.0)
	s.ImagePerUnitBilling = true
	s.ImageUnit = ImageUnitPrice{OneK: oneK, TwoK: twoK}
	s.ImageInputUnitPrice = inputPerImage
	return s
}

// deepSeekFlash 构造 TokenHub DeepSeek V4 Flash 的标准价。
// 该上游没有 priority/flex 档，不能使用 std() 自动派生不存在的价格。
func deepSeekFlash(name string) Spec {
	return Spec{
		Name:            name,
		ContextWindow:   1_000_000,
		MaxOutputTokens: 384_000,
		InputPrice:      0.14705882352941177,
		CachedPrice:     0.02941176470588235,
		OutputPrice:     0.29411764705882354,
	}
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
	//
	// ⚠️ output 是「图像输出档」单价，不是同名文本模型的 output 价：Google 对
	// 图像 output token 单独计价（3-pro $120、3.1-flash $60、flash-lite/2.5-flash $30
	// 每 1M）。这里曾误填成文本档的 12 / 3 / 1.5，靠 core 的 models.catalog 覆盖层
	// 兜着才没卖错价；覆盖层一旦清空就会静默 10~20 倍贱卖，故内置值必须自洽。
	// 与 extensions/airgate-gemini 的 imageModelSpecs 保持同一组数字。
	//
	// 单张牌价的档位必须与 gateway/image_model_limits.go 的 imageModelSupportedSizes
	// 对齐：flash-lite 与 2.5-flash 只出 1K，3.1-flash 最高 2K，只有 3-pro 出得了 4K。
	"gemini-2.5-flash-image": withImageUnitPrices(
		pricedImageSpec("Gemini 2.5 Flash Image", 0.3, 0.03, 30.0), 0.0387, 0, 0),
	"gemini-3-pro-image": withImageUnitPrices(
		pricedImageSpec("Gemini 3 Pro Image", 2.0, 0.2, 120.0), 0.1344, 0.1344, 0.24),
	"gemini-3-pro-image-c": withImageUnitPrices(
		pricedImageSpec("Gemini 3 Pro Image C", 2.0, 0.2, 120.0), 0.1344, 0.1344, 0.24),
	"gemini-3-pro-image-preview": withImageUnitPrices(
		pricedImageSpec("Gemini 3 Pro Image Preview", 2.0, 0.2, 120.0), 0.1344, 0.1344, 0.24),
	"gemini-3-pro-image-preview-c": withImageUnitPrices(
		pricedImageSpec("Gemini 3 Pro Image Preview C", 2.0, 0.2, 120.0), 0.1344, 0.1344, 0.24),
	"gemini-3.1-flash-image": withImageUnitPrices(
		pricedImageSpec("Gemini 3.1 Flash Image", 0.5, 0.05, 60.0), 0.0672, 0.0672, 0),
	"gemini-3.1-flash-image-c": withImageUnitPrices(
		pricedImageSpec("Gemini 3.1 Flash Image C", 0.5, 0.05, 60.0), 0.0672, 0.0672, 0),
	"gemini-3.1-flash-image-preview": withImageUnitPrices(
		pricedImageSpec("Gemini 3.1 Flash Image Preview", 0.5, 0.05, 60.0), 0.0672, 0.0672, 0),
	"gemini-3.1-flash-image-preview-c": withImageUnitPrices(
		pricedImageSpec("Gemini 3.1 Flash Image Preview C", 0.5, 0.05, 60.0), 0.0672, 0.0672, 0),
	"gemini-3.1-flash-lite-image": withImageUnitPrices(
		pricedImageSpec("Gemini 3.1 Flash Lite Image", 0.25, 0.025, 30.0), 0.0336, 0, 0),

	// ── xAI Grok（OpenAI 兼容协议，经 TokenMart 中继转发）──
	// 官方价 2026-08-24 核实（docs.x.ai）：4.20 系 / 4.3 为 $1.25/$2.50，
	// 4.5 / 4.6 为 $2/$6（缓存读分别 $0.30 / $0.50）；≥200K 整笔 ×2。
	// 中继实报价 = 官方 × 0.27，逐模型逐档验证吻合；基准价只写官方，采购
	// 折扣由账号倍率核算。上游 completion_tokens 不含 reasoning（见 grokChat）。
	"grok-4.20-0309-reasoning":   grokChat("Grok 4.20 Reasoning", 2_000_000, 1.25, 0.20, 2.50),
	"grok-4.20-multi-agent-0309": grokChat("Grok 4.20 Multi-Agent", 2_000_000, 1.25, 0.20, 2.50),
	"grok-4.3":                   grokChat("Grok 4.3", 1_000_000, 1.25, 0.20, 2.50),
	"grok-4.5":                   grokChat("Grok 4.5", 500_000, 2.0, 0.30, 6.0),
	"grok-4.6":                   grokChat("Grok 4.6", 500_000, 2.0, 0.50, 6.0),

	// ── xAI Grok Imagine 图像（按张计费，$/张官方牌价，响应无 token usage）──
	// 官方口径：image $0.02/张（输入图 $0.002）；2.0 按 resolution 1k $0.06 /
	// 2k $0.08；quality 1k $0.05 / 2k $0.07（2.0 与 quality 输入图 $0.01）。
	// 实测上游 cost_in_usd_ticks（官方 $×1e10）与上表逐档吻合；resolution
	// 缺省按 1k 计（实测缺省单 6e8 ticks = 1k 档）。
	"grok-imagine-image":         grokImage("Grok Imagine Image", 0.02, 0, 0.002),
	"grok-imagine-image-2.0":     grokImage("Grok Imagine Image 2.0", 0.06, 0.08, 0.01),
	"grok-imagine-image-quality": grokImage("Grok Imagine Image Quality", 0.05, 0.07, 0.01),

	// ── DeepSeek（OpenAI 兼容协议，经 TokenHub 转发）──
	// TokenHub 实际基础价（每 1M tokens）为 ¥1 / ¥0.2 / ¥2；按项目固定汇率
	// ¥6.8/$ 换算为精确美元值。生产分组的 5.644 倍（83 折）由 Core 配置，
	// 不在这里重复相乘。这样缓存价也保持 TokenHub 的 1:5 比例，避免沿用
	// 上游公开价的 1:50 缓存折扣而低收费用。
	// 生产仅发布腾讯 TokenHub 自部署优惠线路已实测可用的正式版本。不要注册旧
	// 无后缀别名，避免请求误路由到未购买优惠的官方直连线路。
	"deepseek-v4-flash-202605": deepSeekFlash("DeepSeek V4 Flash"),
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
	// 顺序敏感：先细分（codex / mini / image）后粗分（gpt-5 / gpt-4）。
	// deepseek 必须最先判：变体名可能含 "chat"/"mini" 等关键字，
	// 掉进 GPT 系兜底价会让输入多收 17 倍、输出多收 51 倍。
	switch {
	case strings.Contains(id, "deepseek"):
		return reg["deepseek-v4-flash-202605"], true
	// grok 必须先于 "image" 关键字判：未注册的 grok-imagine 变体掉进 gpt-image
	// token 价会因响应无 token usage 变成免费流量；未注册 grok 对话模型掉进
	// GPT 兜底价会丢 OutputExcludesReasoning，推理输出几乎全漏计费。
	case strings.Contains(id, "grok") && strings.Contains(id, "imagine"):
		return reg["grok-imagine-image-2.0"], true
	case strings.Contains(id, "grok"):
		return reg["grok-4.6"], true
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
	if series := seriesForModel(id); series != "" {
		mi.Metadata["series"] = series
	}
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
	case strings.HasPrefix(id, "grok"):
		return "xai"
	case strings.HasPrefix(id, "deepseek"):
		return "deepseek"
	default:
		return "openai"
	}
}

// seriesForModel 按模型 ID 推断系列标识(metadata 约定键 "series",模型广场 L3 折叠)。
// 与调度侧 family(账号家族冷却)语义不同,勿混用。空串=不折叠。
func seriesForModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(id, "gpt-5.6"):
		return "gpt-5.6"
	case strings.HasPrefix(id, "gpt-image"):
		return "gpt-image"
	case strings.HasPrefix(id, "gemini") && strings.Contains(id, "image"):
		return "gemini-image"
	case strings.HasPrefix(id, "deepseek-v4"):
		return "deepseek-v4"
	case strings.HasPrefix(id, "grok-imagine-image"):
		return "grok-imagine-image"
	case strings.HasPrefix(id, "grok-4"):
		return "grok-4"
	default:
		return ""
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
	// 官方单张牌价（$/张）。core 解析 price.image.<档位> 后，模型广场按档位铺价，
	// 不再拿 token 价充数；未声明档位的模型仍回落 token 展示。
	for _, tier := range spec.ImageUnit.Tiers() {
		put("price.image."+tier.Tier, tier.Price)
	}
	// 按张计费模型的每张输入参考图官方单价（纯展示；计费走 gateway 按张分支）。
	put("price.image_input", spec.ImageInputUnitPrice)
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
