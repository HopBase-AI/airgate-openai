package gateway

import (
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// withCatalogTimePricing 装上「DeepSeek V4.1 Flash 峰谷定价」覆盖层：
// 标准价=官方高峰价（¥2/¥8/¥0.04 ÷ 6.8），工作日 09:00-12:00、14:00-18:00 为高峰。
func withCatalogTimePricing(t *testing.T) {
	t.Helper()
	const raw = `[{"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash",
	  "pricing":{"input":0.29411764705882354,"output":1.1764705882352942,"cached_input":0.0058823529411764705},
	  "context_window":1000000,"max_output_tokens":384000,
	  "time_pricing":{"timezone":"Asia/Shanghai","off_peak_multiplier":0.5,
	    "peak_windows":[{"weekdays":["mon","tue","wed","thu","fri"],"start":"09:00","end":"12:00"},
	                    {"weekdays":["mon","tue","wed","thu","fri"],"start":"14:00","end":"18:00"}]}}]`
	if _, err := model.SetCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("装载覆盖层失败: %v", err)
	}
	t.Cleanup(model.ResetCatalogOverlay)
}

// freezeBillingClock 把计费时刻钉死在给定的北京时间。
func freezeBillingClock(t *testing.T, year int, month time.Month, day, hour, minute int) {
	t.Helper()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	frozen := time.Date(year, month, day, hour, minute, 0, 0, shanghai)
	original := billingNow
	billingNow = func() time.Time { return frozen }
	t.Cleanup(func() { billingNow = original })
}

// 低峰时段整单半价，且单价 metadata 打上 pricing_window=off_peak 便于对账。
func TestFillUsageCostAppliesOffPeakDiscount(t *testing.T) {
	withCatalogTimePricing(t)
	freezeBillingClock(t, 2026, time.September, 17, 22, 0) // 周四 22:00 → 低峰

	usage := newTokenUsage("deepseek-v4.1-flash", "", 1_000_000, 1_000_000, 0, 0, 0)
	fillUsageCost(usage)

	spec := model.Lookup("deepseek-v4.1-flash")
	if got, want := metricAccountCost(usage, usageMetricInputTokens), spec.InputPrice*0.5; got != want {
		t.Fatalf("低峰输入费 = %v, want %v（标准价 %v 的一半）", got, want, spec.InputPrice)
	}
	if got, want := metricAccountCost(usage, usageMetricOutputTokens), spec.OutputPrice*0.5; got != want {
		t.Fatalf("低峰输出费 = %v, want %v", got, want)
	}
	for _, key := range []string{usageMetricInputTokens, usageMetricOutputTokens} {
		if window := metricMetadataValue(usage, key, "pricing_window"); window != "off_peak" {
			t.Fatalf("%s 的 pricing_window = %q, want off_peak", key, window)
		}
	}
}

// 高峰时段按标准价，且不写 pricing_window 标记。
func TestFillUsageCostKeepsPeakPrice(t *testing.T) {
	withCatalogTimePricing(t)
	freezeBillingClock(t, 2026, time.September, 17, 10, 0) // 周四 10:00 → 高峰

	usage := newTokenUsage("deepseek-v4.1-flash", "", 1_000_000, 1_000_000, 0, 0, 0)
	fillUsageCost(usage)

	spec := model.Lookup("deepseek-v4.1-flash")
	if got := metricAccountCost(usage, usageMetricInputTokens); got != spec.InputPrice {
		t.Fatalf("高峰输入费 = %v, want 标准价 %v", got, spec.InputPrice)
	}
	if window := metricMetadataValue(usage, usageMetricInputTokens, "pricing_window"); window != "" {
		t.Fatalf("高峰不应写 pricing_window, got %q", window)
	}
}

// 周末整日低峰。
func TestFillUsageCostWeekendIsOffPeak(t *testing.T) {
	withCatalogTimePricing(t)
	freezeBillingClock(t, 2026, time.September, 19, 10, 0) // 周六 10:00

	usage := newTokenUsage("deepseek-v4.1-flash", "", 1_000_000, 0, 0, 0, 0)
	fillUsageCost(usage)

	spec := model.Lookup("deepseek-v4.1-flash")
	if got, want := metricAccountCost(usage, usageMetricInputTokens), spec.InputPrice*0.5; got != want {
		t.Fatalf("周六应按低峰价: got %v want %v", got, want)
	}
}

// 没配峰谷的模型任何时刻都按标准价——本次改动不得波及其它模型。
func TestFillUsageCostUnaffectedForModelsWithoutTimePricing(t *testing.T) {
	freezeBillingClock(t, 2026, time.September, 17, 22, 0) // 低峰时刻

	usage := newTokenUsage("deepseek-v4-flash-202605", "", 1_000_000, 0, 0, 0, 0)
	fillUsageCost(usage)

	spec := model.Lookup("deepseek-v4-flash-202605")
	if got := metricAccountCost(usage, usageMetricInputTokens); got != spec.InputPrice {
		t.Fatalf("未配峰谷的模型不应打折: got %v want %v", got, spec.InputPrice)
	}
}

// 缓存读同样随时段浮动（官方缓存命中价也是峰谷各一档）。
func TestFillUsageCostOffPeakCoversCachedInput(t *testing.T) {
	withCatalogTimePricing(t)
	freezeBillingClock(t, 2026, time.September, 17, 3, 0) // 凌晨低峰

	usage := newTokenUsage("deepseek-v4.1-flash", "", 0, 0, 1_000_000, 0, 0)
	fillUsageCost(usage)

	spec := model.Lookup("deepseek-v4.1-flash")
	if got, want := metricAccountCost(usage, usageMetricCachedInputTokens), spec.CachedPrice*0.5; got != want {
		t.Fatalf("低峰缓存读 = %v, want %v", got, want)
	}
}

// metricMetadataValue 取指定指标单价 metadata 里的某个键，测试辅助。
func metricMetadataValue(usage *sdk.Usage, metricKey, metadataKey string) string {
	if usage == nil {
		return ""
	}
	for _, m := range usage.Metrics {
		if m.Key == metricKey {
			return m.Metadata[metadataKey]
		}
	}
	return ""
}
