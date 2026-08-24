package gateway

import (
	"testing"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// TestServerToolCallsCost 官方价表（$5/1K 次；文件 $10/1K）+ 未知类型兜底。
// 实测锚点：9 次 x_search 的 cost ticks 恰比纯 token 费多 $0.045。
func TestServerToolCallsCost(t *testing.T) {
	cases := []struct {
		name    string
		total   int
		details map[string]int
		want    float64
	}{
		{"9 次 x_search", 9, map[string]int{"x_search_calls": 9}, 0.045},
		{"混合类型", 3, map[string]int{"web_search_calls": 1, "code_interpreter_calls": 1, "file_search_calls": 1}, 0.02},
		{"mcp 不计费", 2, map[string]int{"mcp_calls": 2}, 0},
		{"未知类型按兜底价", 2, map[string]int{"future_tool_calls": 2}, 0.01},
		{"总数超细分按兜底补", 5, map[string]int{"x_search_calls": 2}, 0.025},
		{"无细分全按兜底", 4, nil, 0.02},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverToolCallsCost(tc.total, tc.details); !almostEqual(got, tc.want, 1e-9) {
				t.Fatalf("cost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseUsageServerToolFields 从真实 multi-agent Responses usage 提取工具
// 调用数与上游成本刻度（实测响应形态原样）。
func TestParseUsageServerToolFields(t *testing.T) {
	body := `{"object":"response","model":"grok-4.20-multi-agent-0309","usage":{
		"input_tokens":35973,"input_tokens_details":{"cached_tokens":7936},
		"output_tokens":3457,"output_tokens_details":{"reasoning_tokens":3324},
		"num_server_side_tools_used":9,
		"server_side_tool_usage_details":{"web_search_calls":0,"x_search_calls":9,"code_interpreter_calls":0},
		"cost_in_usd_ticks":902759500}}`
	parsed := parseUsage([]byte(body))
	if parsed.serverToolCalls != 9 {
		t.Fatalf("serverToolCalls = %d, want 9", parsed.serverToolCalls)
	}
	if parsed.serverToolDetails["x_search_calls"] != 9 || len(parsed.serverToolDetails) != 1 {
		t.Fatalf("serverToolDetails = %v", parsed.serverToolDetails)
	}
	if parsed.upstreamCostTicks != 902759500 {
		t.Fatalf("upstreamCostTicks = %d", parsed.upstreamCostTicks)
	}
	// chat 口径的 cost 字段同样要拿到。
	chatBody := `{"object":"chat.completion","model":"grok-4.3","usage":{"prompt_tokens":194,"completion_tokens":1,"cost":3884000}}`
	if parsed := parseUsage([]byte(chatBody)); parsed.upstreamCostTicks != 3884000 {
		t.Fatalf("chat cost ticks = %d", parsed.upstreamCostTicks)
	}
}

// TestApplyServerToolCallsCost 工具费经 fillUsageCost 落进 "output" 键费用
// 明细（与 output_tokens 明细共存归入输出管道），gpt 系模型零改动。
func TestApplyServerToolCallsCost(t *testing.T) {
	usage := newTokenUsage("grok-4.20-multi-agent-0309", "", 100, 3457, 7836, 3324, 0)
	setServerToolCallsMetric(usage, 9, map[string]int{"x_search_calls": 9})
	fillUsageCost(usage)

	var toolCost, outputTokensCost float64
	for _, detail := range usage.CostDetails {
		switch detail.Key {
		case "output":
			toolCost = detail.AccountCost
			if detail.Metadata["unit_price"] != "" {
				t.Fatal("工具费明细不得携带 unit_price(会污染 core 的 token 单价快照)")
			}
		case usageCostOutput:
			outputTokensCost = detail.AccountCost
		}
	}
	if !almostEqual(toolCost, 0.045, 1e-9) {
		t.Fatalf("工具费 = %v, want 0.045", toolCost)
	}
	if outputTokensCost <= 0 {
		t.Fatalf("输出 token 费不应被工具费覆盖: %v", outputTokensCost)
	}
	var metricCost float64
	for _, metric := range usage.Metrics {
		if metric.Key == usageMetricServerToolCalls {
			metricCost = metric.AccountCost
		}
	}
	if !almostEqual(metricCost, 0.045, 1e-9) {
		t.Fatalf("指标 AccountCost = %v, want 0.045", metricCost)
	}

	// gpt 系不声明 BillServerSideTools：即使指标在也不计费。
	gpt := newTokenUsage("gpt-5.4", "", 100, 50, 0, 0, 0)
	setServerToolCallsMetric(gpt, 9, map[string]int{"x_search_calls": 9})
	fillUsageCost(gpt)
	for _, detail := range gpt.CostDetails {
		if detail.Key == "output" {
			t.Fatalf("gpt 不应产生工具费明细: %+v", detail)
		}
	}
	if spec := model.Lookup("gpt-5.4"); spec.BillServerSideTools {
		t.Fatal("gpt-5.4 不应声明 BillServerSideTools")
	}
}
