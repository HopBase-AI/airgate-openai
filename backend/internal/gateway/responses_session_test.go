package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func accountWithBaseURL(baseURL string, extra ...string) *sdk.Account {
	creds := map[string]string{"api_key": "sk-test", "base_url": baseURL}
	for i := 0; i+1 < len(extra); i += 2 {
		creds[extra[i]] = extra[i+1]
	}
	return &sdk.Account{ID: 124, Credentials: creds}
}

// 放行范围必须与字段白名单同源：认方舟就是认方舟，别处一律保持旧行为。
func TestResponsesSessionPassthroughEnabled(t *testing.T) {
	cases := []struct {
		name    string
		account *sdk.Account
		want    bool
	}{
		{"火山方舟直连默认放行", accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3"), true},
		{"BytePlus 海外版默认放行", accountWithBaseURL("https://ark.ap-southeast.byteplusapi.com/api/v3"), true},
		{"OpenAI 官方默认不放行", accountWithBaseURL("https://api.openai.com/v1"), false},
		{"中继默认不放行", accountWithBaseURL("https://relay.example.com/gw/abc/v1"), false},
		{"空 base_url 不放行", &sdk.Account{ID: 1, Credentials: map[string]string{"api_key": "sk"}}, false},
		{"nil 账号不放行", nil, false},
		{"凭证强制开", accountWithBaseURL("https://api.openai.com/v1", responsesSessionPassthroughCredential, "on"), true},
		{"凭证强制关（方舟也关）", accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3", responsesSessionPassthroughCredential, "off"), false},
		{"auto 等于缺省", accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3", responsesSessionPassthroughCredential, "auto"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesSessionPassthroughEnabled(tc.account); got != tc.want {
				t.Fatalf("responsesSessionPassthroughEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// 放行账号：previous_response_id 原样到上游（客户要的「有记忆」全靠它）。
func TestPreprocessKeepsPreviousResponseIDForPassthroughAccount(t *testing.T) {
	account := accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3")
	body := []byte(`{"model":"deepseek-v4.1-flash","input":"and what did I just say?","previous_response_id":"resp_abc"}`)

	got := preprocessRequestBody(body, "deepseek-v4.1-flash", "/v1/responses", account)

	if id := gjson.GetBytes(got, "previous_response_id").String(); id != "resp_abc" {
		t.Fatalf("previous_response_id = %q, want resp_abc; body=%s", id, got)
	}
}

// 不放行账号：保持改动前行为，绝不因为本次改动把别的通道带崩。
func TestPreprocessStillDropsPreviousResponseIDForOtherAccounts(t *testing.T) {
	for _, account := range []*sdk.Account{
		accountWithBaseURL("https://api.openai.com/v1"),
		accountWithBaseURL("https://relay.example.com/gw/abc/v1"),
		accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3", responsesSessionPassthroughCredential, "off"),
		nil,
	} {
		body := []byte(`{"model":"gpt-5.4","input":"hi","previous_response_id":"resp_abc"}`)
		got := preprocessRequestBody(body, "gpt-5.4", "/v1/responses", account)
		if gjson.GetBytes(got, "previous_response_id").Exists() {
			t.Fatalf("非放行账号必须继续剔除 previous_response_id; body=%s", got)
		}
		if store := gjson.GetBytes(got, "store"); !store.Exists() || store.Bool() {
			t.Fatalf("非放行账号必须继续强制 store=false; body=%s", got)
		}
	}
}

// store 三种客户端形态：传 true / 传 false / 不传（不传就尊重上游默认，不替客户拍板）。
func TestPreprocessStoreFollowsClientForPassthroughAccount(t *testing.T) {
	account := accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3")
	cases := []struct {
		name       string
		body       string
		wantExists bool
		wantValue  bool
	}{
		{"客户端传 true", `{"model":"m","input":"hi","store":true}`, true, true},
		{"客户端传 false", `{"model":"m","input":"hi","store":false}`, true, false},
		{"客户端不传", `{"model":"m","input":"hi"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preprocessRequestBody([]byte(tc.body), "m", "/v1/responses", account)
			store := gjson.GetBytes(got, "store")
			if store.Exists() != tc.wantExists {
				t.Fatalf("store 存在性 = %v, want %v; body=%s", store.Exists(), tc.wantExists, got)
			}
			if tc.wantExists && store.Bool() != tc.wantValue {
				t.Fatalf("store = %v, want %v; body=%s", store.Bool(), tc.wantValue, got)
			}
		})
	}
}

// /chat/completions 与本次改动无关：两类账号都不得被注入 store、也不做会话绑定。
func TestChatCompletionsUnaffectedBySessionPassthrough(t *testing.T) {
	for _, account := range []*sdk.Account{
		accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3"),
		accountWithBaseURL("https://api.openai.com/v1"),
		nil,
	} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		got := preprocessRequestBody(body, "m", "/v1/chat/completions", account)
		if gjson.GetBytes(got, "store").Exists() {
			t.Fatalf("chat completions 不应出现 store 字段; body=%s", got)
		}
		if !gjson.GetBytes(got, "messages").IsArray() {
			t.Fatalf("chat completions 的 messages 不应被改写; body=%s", got)
		}
		fields := responsesSessionFieldsForAccount(account, body, "/v1/chat/completions")
		if fields.passthrough || !fields.storeDisabled {
			t.Fatalf("chat completions 不应参与会话亲和: %+v", fields)
		}
	}
}

// 绑定只在「响应真的会被上游留存」时才值得登记。
func TestResponsesSessionFieldsStoreDisabled(t *testing.T) {
	ark := accountWithBaseURL("https://ark.cn-beijing.volces.com/api/v3")
	relay := accountWithBaseURL("https://relay.example.com/v1")

	if f := responsesSessionFieldsForAccount(ark, []byte(`{"input":"hi"}`), "/v1/responses"); !f.passthrough || f.storeDisabled {
		t.Fatalf("放行账号 + 未传 store 应登记绑定: %+v", f)
	}
	if f := responsesSessionFieldsForAccount(ark, []byte(`{"input":"hi","store":true}`), "/v1/responses"); f.storeDisabled {
		t.Fatalf("store=true 应登记绑定: %+v", f)
	}
	if f := responsesSessionFieldsForAccount(ark, []byte(`{"input":"hi","store":false}`), "/v1/responses"); !f.storeDisabled {
		t.Fatalf("客户端显式 store=false 时上游不留存，不该登记绑定: %+v", f)
	}
	if f := responsesSessionFieldsForAccount(relay, []byte(`{"input":"hi"}`), "/v1/responses"); f.passthrough || !f.storeDisabled {
		t.Fatalf("非放行账号不参与会话亲和: %+v", f)
	}
}

func TestResponseIDExtraction(t *testing.T) {
	if got := responseIDFromResponsesJSON([]byte(`{"id":"resp_abc","object":"response"}`)); got != "resp_abc" {
		t.Fatalf("非流式 response id = %q", got)
	}
	if got := responseIDFromResponsesJSON([]byte(`{"response":{"id":"resp_nested"}}`)); got != "resp_nested" {
		t.Fatalf("嵌套 response id = %q", got)
	}
	if got := responseIDFromResponsesJSON(nil); got != "" {
		t.Fatalf("空 body 应返回空，got %q", got)
	}
	if got := responseIDFromSSEData([]byte(`{"type":"response.created","response":{"id":"resp_sse"}}`)); got != "resp_sse" {
		t.Fatalf("SSE response id = %q", got)
	}
	// chat completions 的 chunk 顶层也有 id，但那不是 Responses 会话 id，不能误抓。
	if got := responseIDFromSSEData([]byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk"}`)); got != "" {
		t.Fatalf("chat chunk 不应被当成 Responses 会话 id，got %q", got)
	}
}

func TestAirgateUserIDFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Airgate-User-ID", "81")
	if got := airgateUserIDFromHeaders(h); got != 81 {
		t.Fatalf("user id = %d, want 81", got)
	}
	if got := airgateUserIDFromHeaders(http.Header{}); got != 0 {
		t.Fatalf("缺头时应为 0, got %d", got)
	}
	bad := http.Header{}
	bad.Set("X-Airgate-User-ID", "not-a-number")
	if got := airgateUserIDFromHeaders(bad); got != 0 {
		t.Fatalf("非法值应为 0, got %d", got)
	}
}

// 流式请求 core 是直通的，只有插件看得见 response id——抓不到就等于下一轮钉不住。
// 火山的 response.created 第一帧就带 id，这里锁住「第一个 id 即绑定目标」。
func TestStreamCapturesResponseID(t *testing.T) {
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream_1\",\"model\":\"m\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	w := newSignalingResponseWriter()

	captured := []string{}
	outcome, err := handleStreamResponseWithOptions(nil, resp, w, time.Now(), "", streamResponseOptions{
		captureResponseID: func(id string) { captured = append(captured, id) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if len(captured) == 0 || captured[0] != "resp_stream_1" {
		t.Fatalf("captured response ids = %v, want 首个为 resp_stream_1", captured)
	}
	// 抓取只读不写：客户端拿到的字节不受影响。
	if !strings.Contains(w.BodyString(), "resp_stream_1") {
		t.Fatalf("SSE 原样透传给客户端的内容缺失: %s", w.BodyString())
	}
}

// 不开会话亲和时钩子为 nil，流式路径一个字节都不变（回归保护）。
func TestStreamWithoutCaptureHookIsUnchanged(t *testing.T) {
	stream := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_x\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}]}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	w := newSignalingResponseWriter()
	if _, err := handleStreamResponseWithOptions(nil, resp, w, time.Now(), "", streamResponseOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(w.BodyString(), "resp_x") {
		t.Fatalf("流内容不应被改动: %s", w.BodyString())
	}
}
