package gateway

import (
	"testing"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

func acctWithMap(raw string) *sdk.Account {
	return &sdk.Account{Credentials: map[string]string{chatModelMapCredential: raw}}
}

func TestChatUpstreamModelForAccount_MapsConfiguredModel(t *testing.T) {
	acct := acctWithMap(`{"deepseek-v4-pro-202606":"deepseek-v4-pro-ga-260813"}`)
	if got := chatUpstreamModelForAccount(acct, "deepseek-v4-pro-202606"); got != "deepseek-v4-pro-ga-260813" {
		t.Fatalf("want deepseek-v4-pro-ga-260813, got %q", got)
	}
}

func TestChatUpstreamModelForAccount_UnmappedModelReturnsEmpty(t *testing.T) {
	acct := acctWithMap(`{"deepseek-v4-pro-202606":"deepseek-v4-pro-ga-260813"}`)
	if got := chatUpstreamModelForAccount(acct, "gpt-5.6-sol"); got != "" {
		t.Fatalf("unmapped model must return empty, got %q", got)
	}
}

func TestChatUpstreamModelForAccount_NoCredentialReturnsEmpty(t *testing.T) {
	if got := chatUpstreamModelForAccount(&sdk.Account{}, "deepseek-v4-pro-202606"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	if got := chatUpstreamModelForAccount(nil, "deepseek-v4-pro-202606"); got != "" {
		t.Fatalf("nil account must return empty, got %q", got)
	}
}

// 配错不能阻断请求：非法 JSON 时保持公开名直连，由上游给出可诊断错误。
func TestChatUpstreamModelForAccount_InvalidJSONFallsBack(t *testing.T) {
	if got := chatUpstreamModelForAccount(acctWithMap(`{not json`), "deepseek-v4-pro-202606"); got != "" {
		t.Fatalf("invalid JSON must return empty, got %q", got)
	}
}

func TestChatUpstreamModelForAccount_IgnoresIdenticalMapping(t *testing.T) {
	acct := acctWithMap(`{"deepseek-v4-pro-202606":"deepseek-v4-pro-202606"}`)
	if got := chatUpstreamModelForAccount(acct, "deepseek-v4-pro-202606"); got != "" {
		t.Fatalf("identical mapping should be a no-op, got %q", got)
	}
}

func TestChatUpstreamModelForAccount_CaseInsensitiveKey(t *testing.T) {
	acct := acctWithMap(`{"DeepSeek-V4-Pro-202606":"deepseek-v4-pro-ga-260813"}`)
	if got := chatUpstreamModelForAccount(acct, "deepseek-v4-pro-202606"); got != "deepseek-v4-pro-ga-260813" {
		t.Fatalf("want case-insensitive hit, got %q", got)
	}
}

func TestRewriteChatRequestModel_ReplacesOnlyModelField(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro-202606","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	out, err := rewriteChatRequestModel(body, "deepseek-v4-pro-ga-260813")
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "deepseek-v4-pro-ga-260813" {
		t.Fatalf("model not rewritten, got %q", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("payload corrupted, messages.0.content=%q", got)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 8 {
		t.Fatalf("payload corrupted, max_tokens=%d", got)
	}
}

func TestRewriteChatRequestModel_EmptyInputsAreNoOp(t *testing.T) {
	body := []byte(`{"model":"a"}`)
	out, err := rewriteChatRequestModel(body, "")
	if err != nil || string(out) != string(body) {
		t.Fatalf("empty upstream model must be a no-op, got %q err=%v", out, err)
	}
}

// 计费与用量都从 req.Body 读 model，所以重写必须返回新切片、绝不能污染原始 body。
// 一旦原 body 被改成上游 ID，该 ID 不在价格表里会被关键字兜底匹配到别的型号——
// 实测 deepseek-v4-pro-ga-260813 曾被按 Flash 计价，少收 3 倍。
func TestRewriteChatRequestModel_DoesNotMutateOriginalBody(t *testing.T) {
	original := []byte(`{"model":"deepseek-v4-pro-202606","messages":[{"role":"user","content":"hi"}]}`)
	snapshot := string(original)

	out, err := rewriteChatRequestModel(original, "deepseek-v4-pro-ga-260813")
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	if string(original) != snapshot {
		t.Fatalf("original body mutated:\n  before %s\n  after  %s", snapshot, original)
	}
	if gjson.GetBytes(original, "model").String() != "deepseek-v4-pro-202606" {
		t.Fatalf("original model must stay public, got %q", gjson.GetBytes(original, "model").String())
	}
	if gjson.GetBytes(out, "model").String() != "deepseek-v4-pro-ga-260813" {
		t.Fatalf("returned body must carry upstream id, got %q", gjson.GetBytes(out, "model").String())
	}
}

// 上游按自己的 ID 回包时，Usage.Model 必须还原为公开名，成本按公开名重算。
// 若不还原：上游 ID 不在价格表，会被兜底匹配到别的型号（线上 Pro 曾被按 Flash 计价）。
//
// 用内置注册表里确实存在且价格不同的模型来验证——deepseek-v4-pro-202606 的价格
// 来自后台模型目录覆盖层，单测环境没有覆盖层，两个名字会兜底到同一价而测不出差异。
func TestRestoreMappedUsageModel_RestoresPublicNameAndReprices(t *testing.T) {
	const upstreamID = "vendor-internal-xyz-001" // 不含任何系列关键字 → 落 DefaultSpec
	const publicName = "deepseek-v4-flash-202605"

	usage := newTokenUsage(upstreamID, "", 1_000_000, 0, 0, 0, 0)
	fillUsageCost(usage) // 模拟按上游 ID 错算的成本
	wrongInput := metricAccountCost(usage, usageMetricInputTokens)
	if want := model.DefaultSpec.InputPrice; wrongInput != want {
		t.Fatalf("前置条件不成立：未知上游 ID 应落 DefaultSpec %v，实际 %v", want, wrongInput)
	}

	outcome := &sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Usage: usage}
	restoreMappedUsageModel(nil, outcome, publicName)

	if outcome.Usage.Model != publicName {
		t.Fatalf("model not restored, got %q", outcome.Usage.Model)
	}
	gotInput := metricAccountCost(outcome.Usage, usageMetricInputTokens)
	wantInput := model.Lookup(publicName).InputPrice // $/1M × 1M token
	if gotInput != wantInput {
		t.Fatalf("input cost not repriced: got %v want %v", gotInput, wantInput)
	}
	if gotInput == wrongInput {
		t.Fatalf("cost unchanged — reprice did not happen (%v)", gotInput)
	}
}

// 未发生映射时必须完全不介入。
func TestRestoreMappedUsageModel_NoopWhenNotMapped(t *testing.T) {
	usage := newTokenUsage("deepseek-v4-flash-202605", "", 100, 100, 0, 0, 0)
	fillUsageCost(usage)
	before := metricAccountCost(usage, usageMetricInputTokens)

	outcome := &sdk.ForwardOutcome{Kind: sdk.OutcomeSuccess, Usage: usage}
	restoreMappedUsageModel(nil, outcome, "")

	if outcome.Usage.Model != "deepseek-v4-flash-202605" {
		t.Fatalf("model must stay untouched, got %q", outcome.Usage.Model)
	}
	if got := metricAccountCost(outcome.Usage, usageMetricInputTokens); got != before {
		t.Fatalf("cost must stay untouched: got %v want %v", got, before)
	}
}

// metricAccountCost 从 Usage.Metrics 取指定指标的 AccountCost，测试辅助。
func metricAccountCost(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, m := range usage.Metrics {
		if m.Key == key {
			return m.AccountCost
		}
	}
	return 0
}
