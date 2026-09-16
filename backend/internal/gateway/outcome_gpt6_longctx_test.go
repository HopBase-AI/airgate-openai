package gateway

import (
	"math"
	"strconv"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// GPT-6 Astra 官方长上下文阶梯(developers.openai.com/api/docs/models/gpt-6-astra,
// 2026-09-16 核):"Prompts with more than 272K input tokens are priced at 2x input
// and cache rates and 1.5x output for the full request."
//
// 生产 2026-09 有 1162 笔 >272K 输入的 Astra 请求按基准价结算(阶梯当初刻意留空),
// 这组用例就是那次少收的回归闸门:阈值上下各一笔、四个服务档位的逐档单价、
// 以及 usage_cost_details[].metadata.unit_price 与实扣金额的自洽。
const gpt6Astra = "gpt-6-astra"

// 阈值判定按「未缓存输入 + 缓存输入」之和,与官方 "input tokens" 同口径;
// 恰好 272,000 仍属 Short context(官方表头 "≤272K input tokens"),272,001 才进阶梯。
func TestFillUsageCost_GPT6AstraLongContextThresholdBoundary(t *testing.T) {
	model.ResetCatalogOverlay()

	cases := []struct {
		name         string
		input        int
		cached       int
		wantLongCtx  bool
		wantInputPPM float64
	}{
		{"阈值下一笔", 271_999, 0, false, 10},
		{"恰好等于阈值", 272_000, 0, false, 10},
		{"阈值上一笔", 272_001, 0, true, 20},
		{"缓存 token 计入阈值", 271_000, 2_000, true, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usage := newTokenUsage(gpt6Astra, "", c.input, 1_000, c.cached, 0, 0)
			fillUsageCost(usage)

			input := costDetailByKey(usage, usageCostInput)
			_, got := input.Metadata["long_context"]
			if got != c.wantLongCtx {
				t.Fatalf("long_context = %v, want %v (meta=%v)", got, c.wantLongCtx, input.Metadata)
			}
			assertUnitPrice(t, input, c.wantInputPPM)
			assertCostMatchesUnitPrice(t, usage)
		})
	}
}

// 各服务档位的阶梯单价 = 该档标准价 × (2 input / 2 cached / 1.5 output),
// 与官方定价页 Long context 四张表逐格吻合。
func TestFillUsageCost_GPT6AstraLongContextPricesByServiceTier(t *testing.T) {
	model.ResetCatalogOverlay()

	cases := []struct {
		tier                  string
		input, cached, output float64
	}{
		// standard 10/1/50 → 20/2/75
		{"", 20, 2, 75},
		// fast(priority) 20/2/100 → 40/4/150
		{"priority", 40, 4, 150},
		// flex 与 batch 同为标准 ×0.5:5/0.5/25 → 10/1/37.5
		{"flex", 10, 1, 37.5},
	}
	for _, c := range cases {
		name := c.tier
		if name == "" {
			name = "standard"
		}
		t.Run(name, func(t *testing.T) {
			usage := newTokenUsage(gpt6Astra, c.tier, 300_000, 10_000, 5_000, 0, 0)
			fillUsageCost(usage)

			assertUnitPrice(t, costDetailByKey(usage, usageCostInput), c.input)
			assertUnitPrice(t, costDetailByKey(usage, usageCostCachedInput), c.cached)
			assertUnitPrice(t, costDetailByKey(usage, usageCostOutput), c.output)
			assertCostMatchesUnitPrice(t, usage)
		})
	}
}

// 少收回归:同一笔 300K 输入的请求,阶梯生效后的总成本必须严格高于按基准价结算的旧口径。
func TestFillUsageCost_GPT6AstraLongContextChargesMoreThanFlatRate(t *testing.T) {
	model.ResetCatalogOverlay()

	usage := newTokenUsage(gpt6Astra, "", 300_000, 20_000, 0, 0, 0)
	fillUsageCost(usage)

	// 旧口径(无阶梯):300000×$10 + 20000×$50 = $3.0 + $1.0 = $4.0
	// 阶梯口径:300000×$20 + 20000×$75 = $6.0 + $1.5 = $7.5
	if math.Abs(usage.AccountCost-7.5) > 1e-9 {
		t.Fatalf("AccountCost = %v, want 7.5 (少收回归)", usage.AccountCost)
	}
}

// gpt-6-astra 是美元厂商、未声明 ListPrice:一个 price.list.* / list_* 键都不该出现。
// (若将来声明了牌价,annotateListPrice 会按同一 baseEffective/baseStandard 倍率折算,
// 阶梯档的 list_unit_price 自动跟着 ×2 / ×1.5,无需再改这里。)
func TestGPT6AstraHasNoListPriceKeys(t *testing.T) {
	model.ResetCatalogOverlay()

	usage := newTokenUsage(gpt6Astra, "", 300_000, 1_000, 0, 0, 0)
	fillUsageCost(usage)
	for _, detail := range usage.CostDetails {
		for _, key := range []string{"list_currency", "list_unit_price", "list_fx"} {
			if _, ok := detail.Metadata[key]; ok {
				t.Fatalf("%s detail 不应带 %s: %v", gpt6Astra, key, detail.Metadata)
			}
		}
	}
	if spec := model.Lookup(gpt6Astra); spec.ListPrice.Declared() {
		t.Fatalf("%s 不应声明 ListPrice: %+v", gpt6Astra, spec.ListPrice)
	}
}

func assertUnitPrice(t *testing.T, detail sdk.UsageCostDetail, want float64) {
	t.Helper()
	got := parseFloatMeta(t, detail, "unit_price")
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s unit_price = %v, want %v (meta=%v)", detail.Key, got, want, detail.Metadata)
	}
	if detail.Metadata["unit"] != "USD/1M tokens" {
		t.Fatalf("%s unit = %q", detail.Key, detail.Metadata["unit"])
	}
}

// assertCostMatchesUnitPrice 校验每档 AccountCost == 用量 × metadata.unit_price / 1e6,
// 并且行级 AccountCost 等于各档之和——客户拿账单上的单价就能逐笔验算。
func assertCostMatchesUnitPrice(t *testing.T, usage *sdk.Usage) {
	t.Helper()
	metricKeyByCostKey := map[string]string{
		usageCostInput:       usageMetricInputTokens,
		usageCostCachedInput: usageMetricCachedInputTokens,
		usageCostOutput:      usageMetricOutputTokens,
	}
	var total float64
	for _, detail := range usage.CostDetails {
		metricKey, ok := metricKeyByCostKey[detail.Key]
		if !ok {
			total += detail.AccountCost
			continue
		}
		tokens := float64(usageMetricInt(usage, metricKey))
		want := tokens * parseFloatMeta(t, detail, "unit_price") / 1_000_000
		if math.Abs(detail.AccountCost-want) > 1e-9 {
			t.Fatalf("%s AccountCost = %v, want %v (tokens=%v meta=%v)",
				detail.Key, detail.AccountCost, want, tokens, detail.Metadata)
		}
		total += detail.AccountCost
	}
	if math.Abs(usage.AccountCost-total) > 1e-9 {
		t.Fatalf("行级 AccountCost = %v, want %v", usage.AccountCost, total)
	}
}

func parseFloatMeta(t *testing.T, detail sdk.UsageCostDetail, key string) float64 {
	t.Helper()
	raw, ok := detail.Metadata[key]
	if !ok {
		t.Fatalf("%s 缺少 metadata.%s: %v", detail.Key, key, detail.Metadata)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s metadata.%s = %q 无法解析: %v", detail.Key, key, raw, err)
	}
	return v
}
