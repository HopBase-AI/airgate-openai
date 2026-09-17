package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	// 插件二进制跑在精简镜像里，/usr/share/zoneinfo 不一定存在；时区数据必须编进
	// 二进制，否则 LoadLocation("Asia/Shanghai") 失败会让峰谷定价整体失效。
	_ "time/tzdata"
)

// TimePricing 描述「峰谷定价」：Spec 里的标准价即高峰价，落在 PeakWindows 之外
// 的时刻整体乘 OffPeakMultiplier。
//
// DeepSeek 自 2026-09-10 12:00 起对 Flash 系列按北京时间分时段计价：工作日
// 09:00-12:00、14:00-18:00 为高峰，其余时段（含周末整日）为低峰、价格减半。
// 上游按时段收我们的钱，我方卖价与成本记账必须同步浮动——否则低峰段按高峰价
// 多收客户，成本侧还会把实际支出记成两倍。
type TimePricing struct {
	// Location 判定时段用的时区。nil 视为未配置。
	Location *time.Location
	// OffPeakMultiplier 低峰时段乘到标准价上的倍数，取值须在 (0,1)。
	OffPeakMultiplier float64
	// PeakWindows 高峰时段，为空视为未配置（保持标准价，绝不静默打折）。
	PeakWindows []PeakWindow
}

// PeakWindow 一段高峰时段。StartMinute / EndMinute 是当地零点起的分钟数，
// 左闭右开：09:00-12:00 含 09:00:00.000，不含 12:00:00.000。
// EndMinute <= StartMinute 表示跨零点（如 22:00-02:00）。
type PeakWindow struct {
	// Weekdays 生效的星期，为空表示每天。
	Weekdays    []time.Weekday
	StartMinute int
	EndMinute   int
}

// Multiplier 返回 t 时刻应乘到标准价上的倍数：高峰 1，低峰 OffPeakMultiplier。
// 未配置 / 配置不合法一律返回 1——宁可按高峰价收，也不能静默半价。
func (tp *TimePricing) Multiplier(t time.Time) float64 {
	if !tp.valid() {
		return 1
	}
	if tp.isPeak(t) {
		return 1
	}
	return tp.OffPeakMultiplier
}

// IsOffPeak 报告 t 是否落在低峰时段。未配置时恒为 false。
func (tp *TimePricing) IsOffPeak(t time.Time) bool {
	return tp.valid() && !tp.isPeak(t)
}

func (tp *TimePricing) valid() bool {
	return tp != nil &&
		tp.Location != nil &&
		tp.OffPeakMultiplier > 0 &&
		tp.OffPeakMultiplier < 1 &&
		len(tp.PeakWindows) > 0
}

func (tp *TimePricing) isPeak(t time.Time) bool {
	local := t.In(tp.Location)
	minute := local.Hour()*60 + local.Minute()
	for _, window := range tp.PeakWindows {
		if window.contains(local.Weekday(), minute) {
			return true
		}
	}
	return false
}

func (w PeakWindow) contains(day time.Weekday, minute int) bool {
	if !w.matchesWeekday(day) {
		return false
	}
	if w.EndMinute > w.StartMinute {
		return minute >= w.StartMinute && minute < w.EndMinute
	}
	// 跨零点：[Start, 24:00) ∪ [00:00, End)
	return minute >= w.StartMinute || minute < w.EndMinute
}

func (w PeakWindow) matchesWeekday(day time.Weekday) bool {
	if len(w.Weekdays) == 0 {
		return true
	}
	for _, candidate := range w.Weekdays {
		if candidate == day {
			return true
		}
	}
	return false
}

// TimePricingConfig 是覆盖层 JSON 里的 time_pricing 原文结构。
type TimePricingConfig struct {
	Timezone          string             `json:"timezone"`
	OffPeakMultiplier float64            `json:"off_peak_multiplier"`
	PeakWindows       []PeakWindowConfig `json:"peak_windows"`
}

// PeakWindowConfig 覆盖层里的单段高峰时段。
type PeakWindowConfig struct {
	Weekdays []string `json:"weekdays"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday, "0": time.Sunday, "7": time.Sunday,
	"mon": time.Monday, "monday": time.Monday, "1": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday, "2": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday, "3": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday, "4": time.Thursday,
	"fri": time.Friday, "friday": time.Friday, "5": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday, "6": time.Saturday,
}

// ParseTimePricing 把覆盖层配置解析成可判定的 TimePricing。
// 任何一项不合法都返回错误，调用方据此整体忽略该配置、按标准价（高峰价）计费。
func ParseTimePricing(cfg *TimePricingConfig) (*TimePricing, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.OffPeakMultiplier <= 0 || cfg.OffPeakMultiplier >= 1 {
		return nil, fmt.Errorf("off_peak_multiplier 须在 (0,1) 之间，得到 %v", cfg.OffPeakMultiplier)
	}
	if len(cfg.PeakWindows) == 0 {
		return nil, fmt.Errorf("peak_windows 为空：高峰时段缺失会把全天都算成低峰")
	}
	zone := strings.TrimSpace(cfg.Timezone)
	if zone == "" {
		return nil, fmt.Errorf("timezone 不能为空")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q 无法解析: %w", zone, err)
	}
	windows := make([]PeakWindow, 0, len(cfg.PeakWindows))
	for index, raw := range cfg.PeakWindows {
		window, err := parsePeakWindow(raw)
		if err != nil {
			return nil, fmt.Errorf("peak_windows[%d]: %w", index, err)
		}
		windows = append(windows, window)
	}
	return &TimePricing{
		Location:          location,
		OffPeakMultiplier: cfg.OffPeakMultiplier,
		PeakWindows:       windows,
	}, nil
}

func parsePeakWindow(cfg PeakWindowConfig) (PeakWindow, error) {
	start, err := parseClockMinute(cfg.Start)
	if err != nil {
		return PeakWindow{}, fmt.Errorf("start %q: %w", cfg.Start, err)
	}
	end, err := parseClockMinute(cfg.End)
	if err != nil {
		return PeakWindow{}, fmt.Errorf("end %q: %w", cfg.End, err)
	}
	if start == end {
		return PeakWindow{}, fmt.Errorf("start 与 end 相同（%s），时段为空", cfg.Start)
	}
	weekdays := make([]time.Weekday, 0, len(cfg.Weekdays))
	for _, raw := range cfg.Weekdays {
		day, ok := weekdayNames[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return PeakWindow{}, fmt.Errorf("weekday %q 无法识别", raw)
		}
		weekdays = append(weekdays, day)
	}
	return PeakWindow{Weekdays: weekdays, StartMinute: start, EndMinute: end}, nil
}

// parseClockMinute 解析 "HH:MM"，返回当地零点起的分钟数。"24:00" 合法，等于 1440。
func parseClockMinute(raw string) (int, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("格式须为 HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("小时不是数字")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("分钟不是数字")
	}
	if hour < 0 || hour > 24 || minute < 0 || minute > 59 || (hour == 24 && minute != 0) {
		return 0, fmt.Errorf("超出 00:00-24:00 范围")
	}
	return hour*60 + minute, nil
}
