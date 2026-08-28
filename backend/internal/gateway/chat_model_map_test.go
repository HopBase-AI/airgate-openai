package gateway

import (
	"testing"

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
