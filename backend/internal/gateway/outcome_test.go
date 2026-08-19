package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestForwardErrForOutcomeSuppressesClientError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeClientError}, err)
	if got != nil {
		t.Fatalf("expected nil err for client outcome, got %v", got)
	}
}

func TestForwardErrForOutcomeKeepsAccountError(t *testing.T) {
	err := errors.New("boom")
	got := forwardErrForOutcome(sdk.ForwardOutcome{Kind: sdk.OutcomeAccountDead}, err)
	if got != err {
		t.Fatalf("expected original err for account outcome, got %v", got)
	}
}

// TestUpstreamTransportOutcomeClientDisconnect 外层 ctx 已取消 → 客户端断开，
// 记 StreamAborted，不追责账号。
func TestUpstreamTransportOutcomeClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fmt.Errorf("Post \"https://api.example.com/v1/responses\": %w", context.Canceled)
	got := upstreamTransportOutcome(ctx, err)
	if got.Kind != sdk.OutcomeStreamAborted {
		t.Fatalf("expected StreamAborted, got %v (reason=%s)", got.Kind, got.Reason)
	}
}

// TestUpstreamTransportOutcomeGuardTimeout 外层 ctx 健康但错误是 context.Canceled →
// 插件守卫（首字节/流停滞）主动断开，维持 UpstreamTransient 并改写原因。
func TestUpstreamTransportOutcomeGuardTimeout(t *testing.T) {
	err := fmt.Errorf("Post \"https://api.example.com/v1/responses\": %w", context.Canceled)
	got := upstreamTransportOutcome(context.Background(), err)
	if got.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected UpstreamTransient, got %v", got.Kind)
	}
	if !strings.Contains(got.Reason, "守卫") {
		t.Fatalf("expected guard-timeout reason, got %q", got.Reason)
	}
}

// TestUpstreamTransportOutcomePlainNetworkError 普通网络错误维持既有 transient 行为。
func TestUpstreamTransportOutcomePlainNetworkError(t *testing.T) {
	got := upstreamTransportOutcome(context.Background(), fmt.Errorf("dial tcp: connection refused"))
	if got.Kind != sdk.OutcomeUpstreamTransient {
		t.Fatalf("expected UpstreamTransient, got %v", got.Kind)
	}
	if got.Reason != "dial tcp: connection refused" {
		t.Fatalf("unexpected reason %q", got.Reason)
	}
}

func TestImagePublicModelIDCollapsesRelayAliases(t *testing.T) {
	cases := []struct {
		responseModel, fallbackModel, want string
	}{
		{"canvas-20", "gpt-image-2", "gpt-image-2"},                        // MiniMax 别名还原
		{"gpt-image-2-124k", "gpt-image-2", "gpt-image-2"},                 // yhshu 别名还原
		{"gpt-image-2", "gpt-image-2-1k", "gpt-image-2-1k"},                // 公开变体保持请求侧口径
		{"", "gpt-image-2", "gpt-image-2"},                                 // 响应缺 model 回退
		{"gemini-3-pro-image", "gemini-3-pro-image", "gemini-3-pro-image"}, // 非 gpt-image-2 请求原样透传
		{"whatever-model", "gpt-5.5", "whatever-model"},
	}
	for _, c := range cases {
		if got := imagePublicModelID(c.responseModel, c.fallbackModel); got != c.want {
			t.Errorf("imagePublicModelID(%q, %q) = %q, want %q", c.responseModel, c.fallbackModel, got, c.want)
		}
	}
}
