package gateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func TestWebReverseImagesErrorClientStatusReturnsNilErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusBadRequest, nil, "图片尺寸不合法")
	if err != nil {
		t.Fatalf("expected nil err for client status, got %v", err)
	}
	if outcome.Kind != sdk.OutcomeClientError {
		t.Fatalf("Kind = %v, want OutcomeClientError", outcome.Kind)
	}
	if outcome.Upstream.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", outcome.Upstream.StatusCode, http.StatusBadRequest)
	}
	if !strings.Contains(string(outcome.Upstream.Body), "图片尺寸不合法") {
		t.Fatalf("body = %s, want message to be preserved", outcome.Upstream.Body)
	}
}

func TestWebReverseImagesErrorAccountStatusKeepsErr(t *testing.T) {
	outcome, err := webReverseImagesError(time.Now(), http.StatusUnauthorized, nil, "OAuth 账号缺少 access_token")
	if err == nil {
		t.Fatalf("expected err for account status")
	}
	if outcome.Kind != sdk.OutcomeAccountDead {
		t.Fatalf("Kind = %v, want OutcomeAccountDead", outcome.Kind)
	}
}

func TestWebReverseRiskControlUsesRecoverableCooldown(t *testing.T) {
	tests := []string{
		`获取 chat token 失败: HTTP 403: {"detail":"sentinel challenge required"}`,
		"未获取到任何图片（可能原因: PoW 未通过 / AT 过期 / 触发风控）",
	}
	for _, message := range tests {
		t.Run(message, func(t *testing.T) {
			status := classifyWebReverseError(errors.New(message))
			if status != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", status)
			}
			outcome, err := webReverseImagesError(time.Now(), status, nil, message)
			if err == nil {
				t.Fatal("error = nil, want Core-facing failover error")
			}
			if outcome.Kind != sdk.OutcomeAccountRateLimited {
				t.Fatalf("Kind = %v, want AccountRateLimited", outcome.Kind)
			}
			if outcome.RetryAfter != webReverseRiskControlRetryAfter {
				t.Fatalf("RetryAfter = %v, want %v", outcome.RetryAfter, webReverseRiskControlRetryAfter)
			}
		})
	}
}

func TestWebReverseDefinitiveCredentialFailureRemainsAccountDead(t *testing.T) {
	tests := []string{
		`HTTP 401: {"error":"invalid access token"}`,
		`HTTP 403: {"error":"account_deactivated","message":"This account has been deactivated"}`,
	}
	for _, message := range tests {
		t.Run(message, func(t *testing.T) {
			status := classifyWebReverseError(errors.New(message))
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			outcome, err := webReverseImagesError(time.Now(), status, nil, message)
			if err == nil {
				t.Fatal("error = nil, want account failure")
			}
			if outcome.Kind != sdk.OutcomeAccountDead {
				t.Fatalf("Kind = %v, want AccountDead", outcome.Kind)
			}
			if outcome.RetryAfter != 0 {
				t.Fatalf("RetryAfter = %v, want zero for dead credential", outcome.RetryAfter)
			}
		})
	}
}
