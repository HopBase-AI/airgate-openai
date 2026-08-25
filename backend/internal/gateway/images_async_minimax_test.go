package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func fastMiniMaxPoll(t *testing.T) {
	t.Helper()
	oldInit, oldMax := miniMaxPollInitialDelay, miniMaxPollMaxInterval
	miniMaxPollInitialDelay = 5 * time.Millisecond
	miniMaxPollMaxInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		miniMaxPollInitialDelay = oldInit
		miniMaxPollMaxInterval = oldMax
	})
}

func asyncTestAccount(baseURL string) *sdk.Account {
	return &sdk.Account{
		ID:   1,
		Type: "apikey",
		Credentials: map[string]string{
			"api_key":             "sk-test",
			"base_url":            baseURL,
			imagesAsyncCredential: "true",
		},
	}
}

func TestImagesAsyncEnabled(t *testing.T) {
	if imagesAsyncEnabled(&sdk.Account{Credentials: map[string]string{imagesAsyncCredential: "true"}}) != true {
		t.Fatal("true 应启用")
	}
	if imagesAsyncEnabled(&sdk.Account{Credentials: map[string]string{imagesAsyncCredential: "1"}}) != true {
		t.Fatal("1 应启用")
	}
	if imagesAsyncEnabled(&sdk.Account{Credentials: map[string]string{}}) {
		t.Fatal("未配置不应启用")
	}
	if imagesAsyncEnabled(&sdk.Account{Credentials: map[string]string{imagesAsyncCredential: "false"}}) {
		t.Fatal("false 不应启用")
	}
}

func TestMiniMaxAsyncTaskID(t *testing.T) {
	if id, ok := miniMaxAsyncTaskID([]byte(`{"task_id":"418218374308096","status":"queued","base_resp":{"status_code":0}}`)); !ok || id != "418218374308096" {
		t.Fatalf("提交响应应识别为任务: id=%q ok=%v", id, ok)
	}
	if _, ok := miniMaxAsyncTaskID([]byte(`{"created":1,"data":[{"b64_json":"xx"}]}`)); ok {
		t.Fatal("同步图片响应不应识别为任务")
	}
	if _, ok := miniMaxAsyncTaskID([]byte(`{"created":0,"base_resp":{"status_code":400}}`)); ok {
		t.Fatal("无 task_id 不应识别为任务")
	}
}

func TestPollMiniMaxImageTaskCompleted(t *testing.T) {
	fastMiniMaxPoll(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != miniMaxImagesTasksPath+"/42" {
			t.Errorf("poll path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"status":"queued","trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`))
		case 2:
			_, _ = w.Write([]byte(`{"status":"in_progress","trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`))
		default:
			_, _ = w.Write([]byte(`{"created":1783916400,"data":[{"b64_json":"QUJD"}],"usage":{"total_tokens":10},"trace_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`))
		}
	}))
	defer srv.Close()

	g := &OpenAIGateway{}
	body, err := g.pollMiniMaxImageTask(context.Background(), asyncTestAccount(srv.URL), "42", slog.Default())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if id, ok := miniMaxAsyncTaskID(body); ok {
		t.Fatalf("完成体不应再被识别为任务: %s", id)
	}
	if calls.Load() < 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestPollMiniMaxImageTaskFailedTerminal(t *testing.T) {
	fastMiniMaxPoll(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"created":0,"trace_id":"t2","base_resp":{"status_code":400,"status_msg":"Invalid image file or mode for image, please check your image file. "}}`))
	}))
	defer srv.Close()

	g := &OpenAIGateway{}
	_, err := g.pollMiniMaxImageTask(context.Background(), asyncTestAccount(srv.URL), "43", slog.Default())
	var failed *asyncImageTaskFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("应返回终态失败错误, got %v", err)
	}
	if failed.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", failed.StatusCode)
	}
}

func TestPollMiniMaxImageTaskFailedViaBaseRespOn200(t *testing.T) {
	fastMiniMaxPoll(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":0,"trace_id":"t3","base_resp":{"status_code":1033,"status_msg":"system error, unknown error"}}`))
	}))
	defer srv.Close()

	g := &OpenAIGateway{}
	_, err := g.pollMiniMaxImageTask(context.Background(), asyncTestAccount(srv.URL), "44", slog.Default())
	var failed *asyncImageTaskFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("base_resp 非 0 应判终态失败, got %v", err)
	}
	if failed.StatusCode != http.StatusBadGateway {
		t.Fatalf("非 HTTP 语义的 base_resp code 应映射 502, got %d", failed.StatusCode)
	}
}

func TestPollMiniMaxImageTaskQueryHiccupRetries(t *testing.T) {
	fastMiniMaxPoll(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			// 查询端点自身抖动：无 base_resp 结构的 5xx。
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`upstream connect error`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"QUJD"}],"base_resp":{"status_code":0}}`))
	}))
	defer srv.Close()

	g := &OpenAIGateway{}
	body, err := g.pollMiniMaxImageTask(context.Background(), asyncTestAccount(srv.URL), "45", slog.Default())
	if err != nil {
		t.Fatalf("查询抖动应重试后成功: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("空响应")
	}
}

func TestPollMiniMaxImageTaskContextCancel(t *testing.T) {
	fastMiniMaxPoll(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"in_progress","base_resp":{"status_code":0}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	g := &OpenAIGateway{}
	_, err := g.pollMiniMaxImageTask(ctx, asyncTestAccount(srv.URL), "46", slog.Default())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时, got %v", err)
	}
}
