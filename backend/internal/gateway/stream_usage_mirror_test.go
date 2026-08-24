package gateway

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestFillZeroedChatStreamUsageMirrors 上游流式 usage 块的置零镜像字段回填。
func TestFillZeroedChatStreamUsageMirrors(t *testing.T) {
	// 实测形态：标准字段正确、镜像字段恒 0 → 回填。
	chunk := `{"object":"chat.completion.chunk","model":"grok-4.3","choices":[],"usage":{"prompt_tokens":194,"completion_tokens":1,"total_tokens":333,"input_tokens":0,"output_tokens":0}}`
	got := fillZeroedChatStreamUsageMirrors(chunk)
	if gjson.Get(got, "usage.input_tokens").Int() != 194 || gjson.Get(got, "usage.output_tokens").Int() != 1 {
		t.Fatalf("镜像字段未回填: %s", got)
	}
	// 标准字段保持不动。
	if gjson.Get(got, "usage.prompt_tokens").Int() != 194 || gjson.Get(got, "usage.total_tokens").Int() != 333 {
		t.Fatalf("标准字段被改动: %s", got)
	}

	// 没有镜像字段的正常 OpenAI 块：不新增字段。
	plain := `{"object":"chat.completion.chunk","usage":{"prompt_tokens":10,"completion_tokens":5}}`
	if got := fillZeroedChatStreamUsageMirrors(plain); gjson.Get(got, "usage.input_tokens").Exists() {
		t.Fatalf("不应新增镜像字段: %s", got)
	}

	// 镜像字段本来就有值：原样保留。
	filled := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"input_tokens":10,"output_tokens":5}}`
	if got := fillZeroedChatStreamUsageMirrors(filled); got != filled {
		t.Fatalf("有值镜像字段不应改写: %s", got)
	}

	// Responses 事件（usage 挂在 response 下）与无 usage 块：原样透传。
	responses := `{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":50}}}`
	if got := fillZeroedChatStreamUsageMirrors(responses); got != responses {
		t.Fatalf("Responses 事件被误改: %s", got)
	}
	delta := `{"object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}],"usage":null}`
	if got := fillZeroedChatStreamUsageMirrors(delta); got != delta {
		t.Fatalf("无 usage 块被误改: %s", got)
	}
}
