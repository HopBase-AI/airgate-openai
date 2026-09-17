package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func arkAccount(creds map[string]string) *sdk.Account {
	if creds == nil {
		creds = map[string]string{}
	}
	if _, ok := creds["base_url"]; !ok {
		creds["base_url"] = "https://ark.cn-beijing.volces.com/api/v3"
	}
	return &sdk.Account{ID: 124, Credentials: creds}
}

func TestArkResponsesFieldFilterScope(t *testing.T) {
	cases := []struct {
		name  string
		creds map[string]string
		want  bool
	}{
		{"ark base_url auto", map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/v3"}, true},
		{"byteplus auto", map[string]string{"base_url": "https://ark.ap-southeast.bytepluses.com/api/v3"}, true},
		{"openai official stays off", map[string]string{"base_url": "https://api.openai.com/v1"}, false},
		{"relay stays off", map[string]string{"base_url": "https://relay.example.com/gw/abc/v1"}, false},
		// 域名里带 volces 但不是后缀，不能误判
		{"lookalike host stays off", map[string]string{"base_url": "https://volces.com.evil.example/v1"}, false},
		{"relay fronting ark opts in", map[string]string{"base_url": "https://relay.example.com/v1", "responses_field_filter": "ark"}, true},
		{"ark opt-out", map[string]string{"base_url": "https://ark.cn-beijing.volces.com/api/v3", "responses_field_filter": "off"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := arkResponsesFieldFilterEnabled(&sdk.Account{Credentials: tc.creds}); got != tc.want {
				t.Fatalf("arkResponsesFieldFilterEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSanitizeResponsesBodyForAccountOnlyOnResponsesPath(t *testing.T) {
	body := []byte(`{"model":"m","truncation":"auto","input":[]}`)
	for _, path := range []string{"/v1/chat/completions", "/v1/images/generations", "/v1/models"} {
		got, dropped := sanitizeResponsesBodyForAccount(arkAccount(nil), body, path)
		if len(dropped) != 0 || string(got) != string(body) {
			t.Fatalf("path %s must be untouched, dropped=%v", path, dropped)
		}
	}
	_, dropped := sanitizeResponsesBodyForAccount(arkAccount(nil), body, "/v1/responses")
	if len(dropped) == 0 {
		t.Fatal("/v1/responses should have been sanitized")
	}
}

func TestSanitizeResponsesBodyLeavesNonArkAccountsAlone(t *testing.T) {
	// 红线：真正的 OpenAI 上游认 namespace / encrypted_function_args，剥掉会让
	// Codex 的动态工具失效——一个上游的兼容问题不能变成所有上游的功能退化。
	body := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"c","name":"n","arguments":"{}","namespace":"mcp__x"}]}`)
	acct := &sdk.Account{Credentials: map[string]string{"base_url": "https://api.openai.com/v1"}}
	got, dropped := sanitizeResponsesBodyForAccount(acct, body, "/v1/responses")
	if len(dropped) != 0 || string(got) != string(body) {
		t.Fatalf("non-ark account must pass through verbatim, dropped=%v", dropped)
	}
}

// TestSanitizeArkResponsesBodyRealCodexShape 用 2026-09-17 生产 400 的真实形状回归：
// 客户端（Codex 系）发的每一个火山不认的字段都必须被剥掉，认的必须一个不少。
func TestSanitizeArkResponsesBodyRealCodexShape(t *testing.T) {
	in := `{
	  "model":"deepseek-v4.1-flash",
	  "instructions":"be brief",
	  "max_output_tokens":32,
	  "store":false,
	  "stream":true,
	  "parallel_tool_calls":false,
	  "prompt_cache_key":"pck-1",
	  "truncation":"auto",
	  "stream_options":{"include_usage":true},
	  "user":"u-1",
	  "access_programs":[],
	  "codex_output_schema":null,
	  "include":["reasoning.encrypted_content"],
	  "reasoning":{"effort":"medium","summary":"auto"},
	  "text":{"verbosity":"medium","format":{"type":"text"}},
	  "tool_choice":"auto",
	  "tools":[{"type":"function","name":"shell","description":"run","strict":false,
	            "parameters":{"type":"object"},"namespace":"mcp__codex_apps__shell"}],
	  "input":[
	    {"type":"message","role":"user","status":"completed",
	     "content":[{"type":"input_text","text":"a < b && c","cache_control":{"type":"ephemeral"}}],
	     "internal_chat_message_metadata_passthrough":{"k":1},"author":"x","recipient":"y"},
	    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}",
	     "status":"completed","namespace":"codex","encrypted_function_args":"ZZ","execution":{"mode":"direct"}},
	    {"type":"function_call_output","call_id":"call_1","output":"a.txt"},
	    {"type":"reasoning","id":"r1","summary":[],"encrypted_content":"E"}
	  ]
	}`

	out, dropped := sanitizeArkResponsesBody([]byte(in))

	wantDropped := []string{
		"input[0].author",
		"input[0].content[0].cache_control",
		"input[0].internal_chat_message_metadata_passthrough",
		"input[0].recipient",
		"input[1].encrypted_function_args",
		"input[1].execution",
		"input[1].namespace",
		".access_programs",
		".codex_output_schema",
		".stream_options",
		".truncation",
		".user",
		"reasoning.summary",
		"text.verbosity",
		"tools[0].namespace",
	}
	gotSorted := append([]string(nil), dropped...)
	sortStrings(gotSorted)
	sortStrings(wantDropped)
	if !reflect.DeepEqual(gotSorted, wantDropped) {
		t.Fatalf("dropped mismatch\n got: %v\nwant: %v", gotSorted, wantDropped)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v", err)
	}

	// 火山认的顶层字段必须原样留着
	for _, key := range []string{"model", "instructions", "max_output_tokens", "store", "stream",
		"parallel_tool_calls", "prompt_cache_key", "include", "reasoning", "text", "tool_choice", "tools", "input"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("supported top-level field %q was dropped", key)
		}
	}
	if eff := got["reasoning"].(map[string]any)["effort"]; eff != "medium" {
		t.Fatalf("reasoning.effort lost: %v", eff)
	}
	if _, ok := got["text"].(map[string]any)["format"]; !ok {
		t.Fatal("text.format lost")
	}

	items := got["input"].([]any)
	if len(items) != 4 {
		t.Fatalf("input items must never be removed, got %d", len(items))
	}
	fc := items[1].(map[string]any)
	for _, key := range []string{"type", "id", "call_id", "name", "arguments", "status"} {
		if _, ok := fc[key]; !ok {
			t.Fatalf("function_call lost supported field %q", key)
		}
	}
	reasoningItem := items[3].(map[string]any)
	if _, ok := reasoningItem["encrypted_content"]; !ok {
		t.Fatal("reasoning item encrypted_content must survive (Ark accepts it)")
	}
	tool := got["tools"].([]any)[0].(map[string]any)
	if _, ok := tool["parameters"]; !ok {
		t.Fatal("function tool parameters lost")
	}

	// 大整数与 prompt 原文都不能被改写
	if n, ok := got["max_output_tokens"].(float64); !ok || n != 32 {
		t.Fatalf("max_output_tokens mangled: %v", got["max_output_tokens"])
	}
	if !strings.Contains(string(out), `a < b && c`) {
		t.Fatalf("prompt text was HTML-escaped or altered: %s", out)
	}
}

func TestSanitizeArkResponsesBodyNoopAndSafety(t *testing.T) {
	t.Run("clean body untouched byte for byte", func(t *testing.T) {
		body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
		got, dropped := sanitizeArkResponsesBody(body)
		if len(dropped) != 0 || string(got) != string(body) {
			t.Fatalf("clean body must be returned verbatim, dropped=%v", dropped)
		}
	})

	t.Run("unparseable body untouched", func(t *testing.T) {
		for _, body := range [][]byte{nil, []byte(""), []byte("   "), []byte("not json"), []byte(`[1,2]`)} {
			got, dropped := sanitizeArkResponsesBody(body)
			if len(dropped) != 0 || string(got) != string(body) {
				t.Fatalf("body %q must be returned verbatim", body)
			}
		}
	})

	t.Run("unknown item and tool types keep every field", func(t *testing.T) {
		// 未登记的元素/工具类型不裁剪：宁可让火山响亮地 400，也不替客户猜语义、
		// 更不能悄悄改写会话历史（2026-09-04 GLM 5.3 上下文裁剪事故）。
		body := []byte(`{"model":"m","input":[{"type":"local_shell_call","call_id":"c","action":{"type":"exec"},"weird":1}],` +
			`"tools":[{"type":"web_search","limit":3,"whatever":true}]}`)
		got, dropped := sanitizeArkResponsesBody(body)
		if len(dropped) != 0 || string(got) != string(body) {
			t.Fatalf("unknown types must pass through verbatim, dropped=%v", dropped)
		}
	})

	t.Run("big numbers keep their exact form", func(t *testing.T) {
		body := []byte(`{"model":"m","truncation":"auto","max_output_tokens":384000,"metadata":{"n":12345678901234567890}}`)
		got, _ := sanitizeArkResponsesBody(body)
		if !strings.Contains(string(got), `12345678901234567890`) || !strings.Contains(string(got), `384000`) {
			t.Fatalf("numbers were mangled: %s", got)
		}
	})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
