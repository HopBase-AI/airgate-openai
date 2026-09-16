package gateway

import (
	"math"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

const qwenListPriceOverlay = `[
  {"id":"qwen3-max","vendor":"alibaba",
   "pricing":{"input":1.7647,"cached_input":0.3529,"output":5.2941},
   "list_price":{"currency":"CNY","fx":6.8,"input":12,"cached_input":2.4,"output":36}}
]`

func withListPriceOverlay(t *testing.T, raw string) {
	t.Helper()
	if _, err := model.SetCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("SetCatalogOverlayJSON: %v", err)
	}
	t.Cleanup(model.ResetCatalogOverlay)
}

func costDetailByKey(usage *sdk.Usage, key string) sdk.UsageCostDetail {
	for _, detail := range usage.CostDetails {
		if detail.Key == key {
			return detail
		}
	}
	return sdk.UsageCostDetail{}
}

// 方案验收口径：覆盖层配千问 list_price 后，一笔 10,000 input tokens 的
// usage_cost_details[0].metadata = {unit_price:1.7647, list_unit_price:12,
// list_fx:6.8, list_currency:CNY}。
func TestFillUsageCost_ListPriceSnapshot(t *testing.T) {
	withListPriceOverlay(t, qwenListPriceOverlay)

	usage := newTokenUsage("qwen3-max", "", 10_000, 2_000, 500, 0, 0)
	fillUsageCost(usage)

	input := costDetailByKey(usage, usageCostInput)
	for key, want := range map[string]string{
		"unit_price":      "1.7647",
		"unit":            "USD/1M tokens",
		"list_unit_price": "12",
		"list_fx":         "6.8",
		"list_currency":   "CNY",
	} {
		if input.Metadata[key] != want {
			t.Fatalf("input.%s = %q, want %q (meta=%v)", key, input.Metadata[key], want, input.Metadata)
		}
	}
	if math.Abs(input.AccountCost-0.017647) > 1e-9 {
		t.Fatalf("input AccountCost = %v, want 0.017647", input.AccountCost)
	}
	// 验算：list_unit_price × 用量 ÷ list_fx ≈ unit_price × 用量
	if implied := 12.0 * 10_000 / 1_000_000 / 6.8; math.Abs(implied-input.AccountCost) > 1e-6 {
		t.Fatalf("identity broken: %v vs %v", implied, input.AccountCost)
	}

	cached := costDetailByKey(usage, usageCostCachedInput)
	if cached.Metadata["list_unit_price"] != "2.4" || cached.Metadata["list_currency"] != "CNY" {
		t.Fatalf("cached metadata = %v", cached.Metadata)
	}
	output := costDetailByKey(usage, usageCostOutput)
	if output.Metadata["list_unit_price"] != "36" || output.Metadata["list_fx"] != "6.8" {
		t.Fatalf("output metadata = %v", output.Metadata)
	}
	// 指标与费用明细共用同一份 metadata，指标侧同样带快照。
	for _, metric := range usage.Metrics {
		if metric.Key == usageMetricInputTokens && metric.Metadata["list_unit_price"] != "12" {
			t.Fatalf("input metric metadata = %v", metric.Metadata)
		}
	}
	// 行级快照。
	if usage.Metadata["list_currency"] != "CNY" || usage.Metadata["list_fx"] != "6.8" {
		t.Fatalf("row-level metadata = %v", usage.Metadata)
	}
}

// 未声明牌价的模型（内置 GPT、按采购价录入的 deepseek-v4-flash）一个 list_* 键都不写。
func TestFillUsageCost_NoListKeysWhenUndeclared(t *testing.T) {
	model.ResetCatalogOverlay()
	for _, id := range []string{"gpt-5.5", "deepseek-v4-flash-202605"} {
		usage := newTokenUsage(id, "", 10_000, 2_000, 500, 0, 0)
		fillUsageCost(usage)
		for _, detail := range usage.CostDetails {
			for _, key := range []string{"list_currency", "list_unit_price", "list_fx"} {
				if _, ok := detail.Metadata[key]; ok {
					t.Fatalf("%s detail %s must not carry %s", id, detail.Key, key)
				}
			}
		}
		for _, key := range []string{"list_currency", "list_fx"} {
			if _, ok := usage.Metadata[key]; ok {
				t.Fatalf("%s row metadata must not carry %s", id, key)
			}
		}
	}
}

// 长上下文阶梯：list_unit_price 与 unit_price 乘同一倍率，恒等式在阶梯档仍成立。
func TestFillUsageCost_ListPriceFollowsLongContextTier(t *testing.T) {
	withListPriceOverlay(t, `[
	  {"id":"qwen3-max","pricing":{"input":1.7647,"cached_input":0.3529,"output":5.2941},
	   "long_context":{"threshold":128000,"input_multiplier":2,"cached_multiplier":2,"output_multiplier":1.5},
	   "list_price":{"currency":"CNY","fx":6.8,"input":12,"cached_input":2.4,"output":36}}
	]`)

	usage := newTokenUsage("qwen3-max", "", 200_000, 1_000, 0, 0, 0)
	fillUsageCost(usage)

	input := costDetailByKey(usage, usageCostInput)
	if input.Metadata["long_context"] != "true" {
		t.Fatalf("precondition: long context tier not applied, meta=%v", input.Metadata)
	}
	if input.Metadata["unit_price"] != "3.5294" || input.Metadata["list_unit_price"] != "24" {
		t.Fatalf("input tier metadata = %v, want unit_price 3.5294 list_unit_price 24", input.Metadata)
	}
	output := costDetailByKey(usage, usageCostOutput)
	if output.Metadata["list_unit_price"] != "54" {
		t.Fatalf("output tier metadata = %v, want list_unit_price 54", output.Metadata)
	}
}

// 换回公开名重算成本时：有牌价 → 无牌价，行级键必须被清掉，不能残留。
func TestFillUsageCost_RepriceClearsStaleRowListKeys(t *testing.T) {
	withListPriceOverlay(t, qwenListPriceOverlay)

	usage := newTokenUsage("qwen3-max", "", 10_000, 0, 0, 0, 0)
	fillUsageCost(usage)
	if usage.Metadata["list_currency"] != "CNY" {
		t.Fatalf("precondition: row list keys missing, meta=%v", usage.Metadata)
	}
	usage.Model = "gpt-5.5"
	setUsageModelAttribute(usage, "gpt-5.5")
	fillUsageCost(usage)
	for _, key := range []string{"list_currency", "list_fx"} {
		if _, ok := usage.Metadata[key]; ok {
			t.Fatalf("stale %s survived reprice: %v", key, usage.Metadata)
		}
	}
	if _, ok := costDetailByKey(usage, usageCostInput).Metadata["list_unit_price"]; ok {
		t.Fatal("stale list_unit_price survived reprice")
	}
}
