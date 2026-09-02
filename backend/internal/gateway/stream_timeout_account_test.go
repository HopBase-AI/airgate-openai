package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// stream_timeout_account_test.go —— 流式超时按账号覆盖。

func TestAccountTimeoutOverride(t *testing.T) {
	cases := []struct {
		name    string
		account *sdk.Account
		want    time.Duration
	}{
		{"nil 账号", nil, 0},
		{"无凭证", &sdk.Account{}, 0},
		{"未配置", &sdk.Account{Credentials: map[string]string{"api_key": "k"}}, 0},
		{"合法 30s", &sdk.Account{Credentials: map[string]string{"first_byte_timeout": "30s"}}, 30 * time.Second},
		{"带空白", &sdk.Account{Credentials: map[string]string{"first_byte_timeout": " 1m "}}, time.Minute},
		{"非法值回落", &sdk.Account{Credentials: map[string]string{"first_byte_timeout": "thirty"}}, 0},
		{"非正值回落", &sdk.Account{Credentials: map[string]string{"first_byte_timeout": "-5s"}}, 0},
		{"零值回落", &sdk.Account{Credentials: map[string]string{"first_byte_timeout": "0"}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountTimeoutOverride(tc.account, "first_byte_timeout"); got != tc.want {
				t.Fatalf("accountTimeoutOverride = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFirstByteTimeoutFor_FallsBackToDefault(t *testing.T) {
	g := &OpenAIGateway{}
	if got := g.firstByteTimeoutFor(&sdk.Account{Credentials: map[string]string{}}); got != defaultFirstByteTimeout {
		t.Fatalf("无覆盖应回落默认: %v", got)
	}
	if got := g.firstByteTimeoutFor(&sdk.Account{Credentials: map[string]string{"first_byte_timeout": "30s"}}); got != 30*time.Second {
		t.Fatalf("账号覆盖未生效: %v", got)
	}
	if got := g.streamIdleTimeoutFor(&sdk.Account{Credentials: map[string]string{"stream_idle_timeout": "45s"}}); got != 45*time.Second {
		t.Fatalf("读空闲覆盖未生效: %v", got)
	}
}

// 上游迟迟不回响应头：账号级 first_byte_timeout 必须先于插件默认的 60s 断开，
// 让 core 及时换号；把覆盖值放宽后同一上游又能正常等到响应头。
func TestDoStreamableUpstream_AccountFirstByteOverride(t *testing.T) {
	const headerDelay = 400 * time.Millisecond
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(headerDelay):
		case <-r.Context().Done():
			return
		case <-done:
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(func() {
		close(done)
		server.Close()
	})

	g := &OpenAIGateway{transportPool: NewTransportPool()}
	newReq := func() *http.Request {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/responses", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		return req
	}

	tight := &sdk.Account{ID: 1, Credentials: map[string]string{"first_byte_timeout": "100ms"}}
	start := time.Now()
	resp, cancel, err := g.doStreamableUpstream(context.Background(), newReq(), tight, true)
	if err == nil {
		cancel()
		_ = resp.Body.Close()
		t.Fatal("账号级 100ms 响应头上限未生效，请求居然成功")
	}
	if elapsed := time.Since(start); elapsed >= headerDelay {
		t.Fatalf("耗时 %v，没有在响应头到达前断开", elapsed)
	}

	loose := &sdk.Account{ID: 2, Credentials: map[string]string{"first_byte_timeout": "5s"}}
	resp, cancel, err = g.doStreamableUpstream(context.Background(), newReq(), loose, true)
	if err != nil {
		t.Fatalf("放宽到 5s 后仍失败: %v", err)
	}
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
