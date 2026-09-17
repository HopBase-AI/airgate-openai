package model

import (
	"testing"
	"time"
)

// 官方口径（DeepSeek Flash 系列，2026-09-10 12:00 起）：北京时间工作日
// 09:00-12:00、14:00-18:00 为高峰，其余时段半价。
func deepSeekTimePricing(t *testing.T) *TimePricing {
	t.Helper()
	tp, err := ParseTimePricing(&TimePricingConfig{
		Timezone:          "Asia/Shanghai",
		OffPeakMultiplier: 0.5,
		PeakWindows: []PeakWindowConfig{
			{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "09:00", End: "12:00"},
			{Weekdays: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "14:00", End: "18:00"},
		},
	})
	if err != nil {
		t.Fatalf("ParseTimePricing 失败: %v", err)
	}
	return tp
}

func TestTimePricingPeakAndOffPeakWindows(t *testing.T) {
	tp := deepSeekTimePricing(t)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败（二进制是否漏了 time/tzdata?）: %v", err)
	}

	cases := []struct {
		name string
		when time.Time
		want float64
	}{
		// 2026-09-17 是周四。
		{"高峰首刻 09:00", time.Date(2026, 9, 17, 9, 0, 0, 0, shanghai), 1},
		{"高峰中段 11:59", time.Date(2026, 9, 17, 11, 59, 59, 0, shanghai), 1},
		{"高峰右开 12:00", time.Date(2026, 9, 17, 12, 0, 0, 0, shanghai), 0.5},
		{"午间低峰 13:30", time.Date(2026, 9, 17, 13, 30, 0, 0, shanghai), 0.5},
		{"高峰次段 14:00", time.Date(2026, 9, 17, 14, 0, 0, 0, shanghai), 1},
		{"高峰次段 17:59", time.Date(2026, 9, 17, 17, 59, 0, 0, shanghai), 1},
		{"下班低峰 18:00", time.Date(2026, 9, 17, 18, 0, 0, 0, shanghai), 0.5},
		{"凌晨低峰 03:00", time.Date(2026, 9, 17, 3, 0, 0, 0, shanghai), 0.5},
		// 2026-09-19 是周六、20 是周日：整日低峰。
		{"周六 10:00 仍低峰", time.Date(2026, 9, 19, 10, 0, 0, 0, shanghai), 0.5},
		{"周日 15:00 仍低峰", time.Date(2026, 9, 20, 15, 0, 0, 0, shanghai), 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tp.Multiplier(tc.when); got != tc.want {
				t.Fatalf("Multiplier(%s) = %v, want %v", tc.when.Format(time.RFC3339), got, tc.want)
			}
			if offPeak := tp.IsOffPeak(tc.when); offPeak != (tc.want != 1) {
				t.Fatalf("IsOffPeak(%s) = %v, 与倍数 %v 不一致", tc.when.Format(time.RFC3339), offPeak, tc.want)
			}
		})
	}
}

// 判定按配置时区走，不跟随宿主机时区：UTC 01:00 = 北京 09:00，属高峰。
func TestTimePricingUsesConfiguredZoneNotHostZone(t *testing.T) {
	tp := deepSeekTimePricing(t)
	utcMorning := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC) // 北京 09:00
	if got := tp.Multiplier(utcMorning); got != 1 {
		t.Fatalf("UTC 01:00（北京 09:00）应算高峰, got %v", got)
	}
	utcNight := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC) // 北京 21:00
	if got := tp.Multiplier(utcNight); got != 0.5 {
		t.Fatalf("UTC 13:00（北京 21:00）应算低峰, got %v", got)
	}
}

func TestTimePricingCrossMidnightWindow(t *testing.T) {
	tp, err := ParseTimePricing(&TimePricingConfig{
		Timezone:          "Asia/Shanghai",
		OffPeakMultiplier: 0.5,
		PeakWindows:       []PeakWindowConfig{{Start: "22:00", End: "02:00"}},
	})
	if err != nil {
		t.Fatalf("ParseTimePricing 失败: %v", err)
	}
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	if got := tp.Multiplier(time.Date(2026, 9, 17, 23, 0, 0, 0, shanghai)); got != 1 {
		t.Fatalf("23:00 应在跨零点高峰段内, got %v", got)
	}
	if got := tp.Multiplier(time.Date(2026, 9, 17, 1, 0, 0, 0, shanghai)); got != 1 {
		t.Fatalf("01:00 应在跨零点高峰段内, got %v", got)
	}
	if got := tp.Multiplier(time.Date(2026, 9, 17, 12, 0, 0, 0, shanghai)); got != 0.5 {
		t.Fatalf("12:00 应在高峰段外, got %v", got)
	}
}

// 配置有问题时一律按标准价（高峰价）收 —— 绝不能因为一处笔误让全天半价。
func TestTimePricingInvalidConfigRejected(t *testing.T) {
	cases := map[string]*TimePricingConfig{
		"倍数为零":    {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0, PeakWindows: []PeakWindowConfig{{Start: "09:00", End: "12:00"}}},
		"倍数不小于一":  {Timezone: "Asia/Shanghai", OffPeakMultiplier: 1, PeakWindows: []PeakWindowConfig{{Start: "09:00", End: "12:00"}}},
		"高峰时段为空":  {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0.5},
		"时区为空":    {OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Start: "09:00", End: "12:00"}}},
		"时区不存在":   {Timezone: "Mars/Olympus", OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Start: "09:00", End: "12:00"}}},
		"时刻格式错":   {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Start: "9am", End: "12:00"}}},
		"起止相同":    {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Start: "09:00", End: "09:00"}}},
		"星期拼写错":   {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Weekdays: []string{"monday!"}, Start: "09:00", End: "12:00"}}},
		"小时越界":    {Timezone: "Asia/Shanghai", OffPeakMultiplier: 0.5, PeakWindows: []PeakWindowConfig{{Start: "25:00", End: "26:00"}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTimePricing(cfg); err == nil {
				t.Fatalf("非法配置应当报错，实际被接受")
			}
		})
	}
}

// 未配置峰谷（nil）时一切照旧：倍数恒为 1。
func TestTimePricingNilIsNeutral(t *testing.T) {
	var tp *TimePricing
	if got := tp.Multiplier(time.Now()); got != 1 {
		t.Fatalf("未配置峰谷定价时倍数应为 1, got %v", got)
	}
	if tp.IsOffPeak(time.Now()) {
		t.Fatalf("未配置峰谷定价时不应判为低峰")
	}
}

// 覆盖层能把峰谷定价配到模型上；非法配置整体忽略但不影响同条目的其它字段。
func TestCatalogOverlayAppliesTimePricing(t *testing.T) {
	t.Cleanup(ResetCatalogOverlay)

	const raw = `[{"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash",
	  "pricing":{"input":0.29411764705882354,"output":1.1764705882352942,"cached_input":0.0058823529411764705},
	  "context_window":1000000,"max_output_tokens":384000,
	  "time_pricing":{"timezone":"Asia/Shanghai","off_peak_multiplier":0.5,
	    "peak_windows":[{"weekdays":["mon","tue","wed","thu","fri"],"start":"09:00","end":"12:00"},
	                    {"weekdays":["mon","tue","wed","thu","fri"],"start":"14:00","end":"18:00"}]}}]`
	if _, err := SetCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("SetCatalogOverlayJSON 失败: %v", err)
	}
	spec := Lookup("deepseek-v4.1-flash")
	if spec.TimePricing == nil {
		t.Fatalf("覆盖层里的 time_pricing 没有落到 Spec 上")
	}
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	if got := spec.TimePricing.Multiplier(time.Date(2026, 9, 17, 10, 0, 0, 0, shanghai)); got != 1 {
		t.Fatalf("周四 10:00 应按高峰价, got %v", got)
	}
	if got := spec.TimePricing.Multiplier(time.Date(2026, 9, 17, 22, 0, 0, 0, shanghai)); got != 0.5 {
		t.Fatalf("周四 22:00 应按低峰价, got %v", got)
	}
	if spec.InputPrice != 0.29411764705882354 {
		t.Fatalf("标准价应保持高峰价, got %v", spec.InputPrice)
	}
}

func TestCatalogOverlayIgnoresInvalidTimePricing(t *testing.T) {
	t.Cleanup(ResetCatalogOverlay)

	const raw = `[{"id":"deepseek-v4.1-flash","pricing":{"input":0.3,"output":1.2,"cached_input":0.006},
	  "time_pricing":{"timezone":"Asia/Shanghai","off_peak_multiplier":0.5,"peak_windows":[]}}]`
	if _, err := SetCatalogOverlayJSON(raw); err != nil {
		t.Fatalf("非法 time_pricing 不应让整个覆盖层解析失败: %v", err)
	}
	spec := Lookup("deepseek-v4.1-flash")
	if spec.TimePricing != nil {
		t.Fatalf("非法 time_pricing 应被整体忽略")
	}
	if spec.InputPrice != 0.3 {
		t.Fatalf("同条目的价格字段仍应生效, got %v", spec.InputPrice)
	}
}
