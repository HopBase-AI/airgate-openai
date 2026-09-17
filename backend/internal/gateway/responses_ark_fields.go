package gateway

import (
	"bytes"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// ──────────────────────────────────────────────────────────────────────────────
// 火山方舟 Responses API 字段白名单
//
// 背景（2026-09-17 生产事故）：组 55 的 DeepSeek V4.1 Flash 从腾讯切到火山方舟后，
// 付费客户走 /v1/responses 的每一笔都被火山 400 掉：
//
//	HTTP 400 The parameter `input` specified in the request are not valid:
//	         `json: unknown field "namespace"`
//
// `json: unknown field "xxx"` 是 Go `encoding/json` 在 `Decoder.DisallowUnknownFields()`
// 下的原生文案——**火山方舟对 Responses API 的请求体做严格解码，任何它不认识的字段
// 直接 400**，而且严格解码是逐层的：顶层、`input[]` 每个元素、`content[]` 每个分片、
// `tools[]` 每个条目、`reasoning` / `text` 这些子对象，各有各的结构体。
//
// `namespace` 是 **OpenAI Responses API 规范**里的字段（`FunctionToolCall.namespace`），
// Codex 系客户端在挂 MCP / 分组工具时会带上，DeepSeek 官网与 OpenAI 官方都收，
// 火山不收。**这个字段不是我们发的，是客户端发的**（全仓检索 `namespace` 零命中）。
//
// 为什么这个 400 特别贵：400 会触发 core 的 failover，组 55 的备用是成本倍率 6.8 的
// 腾讯账号，而组卖价倍率只有 4.42 —— 每掉一笔亏 54%。一个字段不兼容 = 持续负毛利。
//
// ## 为什么是白名单而不是把 `namespace` 拉黑
//
// 黑名单等于「一个个字段试」：客户端升一次版、换一个 SDK，随便多一个火山不认的字段
// 就再 400 一次，而每次 400 都静默地掉到亏本的兜底上游——没有人会看见。
// 2026-09-15 Gemini `store` 字段那次就是顶层白名单放过了嵌套字段，主力通道悄悄走
// 兜底好几天没被发现。所以这里按**逐层白名单**放行，未知字段一律剥掉。
//
// ## 白名单的出处
//
// 两个来源，取并集，代码里逐条标注：
//
//  1. 火山方舟官方文档《创建 Response》 docs.volcengine.com/docs/82379/1569618
//     （2026-09-17 核）列出的 21 个顶层 Body 参数。
//  2. 2026-09-17 用生产账号 124 对 https://ark.cn-beijing.volces.com/api/v3/responses
//     逐字段实测的结果。**实测与文档不一致时以实测为准**——文档没列但火山实际接受的
//     有 4 个（parallel_tool_calls / prompt_cache_key / safety_identifier /
//     client_metadata），照文档剥掉会误伤：`parallel_tool_calls` 是真实的行为开关，
//     `prompt_cache_key` 影响缓存亲和，而缓存档单价只有输入价的 1/50。
//
// 嵌套层（`input[]` 元素、`content[]` 分片、`tools[]` 条目）官方文档只给了跨类型的
// 字段并集，**但火山的解码器是按 type 分结构体的**：实测把 `summary` / `call_id` /
// `encrypted_content` 放到 `message` 元素上照样 400。所以这里按 type 分别白名单。
//
// ## 边界：只剥字段，不改结构
//
// 本过滤器**只删对象里的键**，绝不删数组元素、不改 type、不重排会话历史：
//
//   - 删 `input[]` 元素会切断 function_call / function_call_output 的配对，
//     比 400 更难查（2026-09-04 GLM 5.3 的上下文裁剪守卫事故就是这么来的）；
//   - 删 `tools[]` 条目会静默改变模型能力，客户无从察觉。
//
// 因此火山不支持的**元素类型**（`local_shell_call` / `custom_tool_call` /
// `agent_message` / `compaction` / `item_reference` …）与**工具类型**
// （`local_shell` / `custom` / `file_search` / `code_interpreter` /
// `computer_use_preview` / `image_generation` / Codex 的 `type:"namespace"` 容器）
// 仍然会被火山 400——这是**有意为之**：让它响亮地失败，而不是我们替客户猜一个语义。
// 已知清单见 ../CLAUDE.md「火山方舟协议支持面」。
//
// ## 作用域
//
// 默认只对火山方舟账号生效。**绝不能做成全局**：真正的 OpenAI 上游（以及 Codex Pro
// 中继）是认这些字段的，剥掉会让 Codex 的动态工具与加密参数失效，把一个上游的
// 兼容问题变成所有上游的功能退化。
// ──────────────────────────────────────────────────────────────────────────────

// responsesFieldFilterCredential 账号凭证键：Responses 请求体字段过滤模式。
//
//	""/"auto" → 自动：base_url 指向火山方舟时启用（默认）
//	"ark"/"on" → 强制启用（中继转火山时用）
//	"off"      → 强制关闭
const responsesFieldFilterCredential = "responses_field_filter"

// arkHostSuffixes 火山方舟 / BytePlus（方舟海外版）的推理域名后缀。
var arkHostSuffixes = []string{
	"volces.com",
	"volcengineapi.com",
	"byteplusapi.com",
	"bytepluses.com",
}

// isVolcengineArkAccount 按 base_url 主机名判断是否为火山方舟直连账号。
func isVolcengineArkAccount(account *sdk.Account) bool {
	if account == nil {
		return false
	}
	raw := strings.TrimSpace(account.Credentials["base_url"])
	if raw == "" {
		return false
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = strings.ToLower(strings.TrimSpace(u.Hostname()))
	}
	if host == "" {
		host = strings.ToLower(raw)
	}
	for _, suffix := range arkHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// arkResponsesFieldFilterEnabled 判断本次转发是否要按火山口径裁剪 Responses 请求体。
func arkResponsesFieldFilterEnabled(account *sdk.Account) bool {
	switch strings.ToLower(strings.TrimSpace(accountCredential(account, responsesFieldFilterCredential))) {
	case "off", "false", "0", "none":
		return false
	case "ark", "on", "true", "1", "volcengine":
		return true
	default: // "" / "auto"
		return isVolcengineArkAccount(account)
	}
}

// arkResponsesTopLevelFields 火山方舟 /api/v3/responses 接受的顶层 Body 字段。
var arkResponsesTopLevelFields = map[string]bool{
	// —— 官方文档《创建 Response》（82379/1569618）列出的 21 个 ——
	"input": true, "model": true,
	"caching": true, "context_management": true, "expire_at": true,
	"include": true, "instructions": true,
	"max_output_tokens": true, "max_tool_calls": true, "metadata": true,
	"previous_response_id": true, "reasoning": true, "service_tier": true,
	"store": true, "stream": true, "temperature": true, "text": true,
	"thinking": true, "tool_choice": true, "tools": true, "top_p": true,

	// —— 文档未列、2026-09-17 实测火山接受（保留，剥掉会误伤真实语义）——
	// parallel_tool_calls：并行工具调用开关，行为性字段。
	// prompt_cache_key：缓存亲和键，直接影响能否命中缓存档单价。
	// safety_identifier / client_metadata：客户端自带的标识，火山原样接受。
	"parallel_tool_calls": true, "prompt_cache_key": true,
	"safety_identifier": true, "client_metadata": true,
}

// arkResponsesReasoningFields `reasoning` 子对象。实测只认 effort；
// OpenAI 的 summary / generate_summary 都会 400。
var arkResponsesReasoningFields = map[string]bool{"effort": true}

// arkResponsesTextFields `text` 子对象。实测只认 format；
// OpenAI 的 verbosity 会 400（Codex 默认就发这个）。
var arkResponsesTextFields = map[string]bool{"format": true}

// arkResponsesInputItemFields 按 `input[].type` 分类的字段白名单。
//
// 火山的解码器按 type 分结构体，不是一份并集：实测 `summary` 放在 message 元素上
// 会 400（unknown field "summary"），但放在 reasoning 元素上正常。
// 未登记的 type 不做裁剪（见文件头「只剥字段，不改结构」）。
var arkResponsesInputItemFields = map[string]map[string]bool{
	// Codex 系客户端在这里带 internal_chat_message_metadata_passthrough / author /
	// recipient，火山全部 400。phase 与 partial 是火山认的（partial 用于前缀续写）。
	"message": {"type": true, "role": true, "content": true, "id": true, "status": true, "phase": true, "partial": true},
	// 本次事故的 400 源头：namespace / encrypted_function_args / execution 三个
	// Codex 私有字段都挂在这里。
	"function_call":        {"type": true, "id": true, "call_id": true, "name": true, "arguments": true, "status": true},
	"function_call_output": {"type": true, "id": true, "call_id": true, "output": true, "status": true},
	"reasoning":            {"type": true, "id": true, "summary": true, "content": true, "encrypted_content": true, "status": true},
	"web_search_call":      {"type": true, "id": true, "status": true, "action": true},
}

// arkResponsesContentPartFields 按 `content[].type` 分类的字段白名单。
//
// 只裁剪分片自身的键，不递归进 image_url / video_url 的值。
// 实测会被火山 400 的典型来源：Anthropic 风格客户端的 `cache_control`、
// OpenAI 的 `annotations`、生图侧的 `image_size`。
var arkResponsesContentPartFields = map[string]map[string]bool{
	"input_text":  {"type": true, "text": true},
	"text":        {"type": true, "text": true},
	"input_image": {"type": true, "image_url": true, "detail": true, "file_id": true},
	"input_file":  {"type": true, "file_id": true, "file_url": true, "filename": true, "file_data": true},
	"video_url":   {"type": true, "video_url": true, "fps": true},
}

// arkResponsesFunctionToolFields `tools[]` 中 type=function 条目的字段白名单。
// Codex 会在这里带 namespace（把函数归到 MCP 分组下），火山 400。
// 其它 tool type（web_search / mcp / knowledge_search …）的配置结构各不相同，
// 不做裁剪，避免把内置工具配置改坏。
var arkResponsesFunctionToolFields = map[string]bool{
	"type": true, "name": true, "description": true, "parameters": true, "strict": true,
}

// arkFieldDropLimit 记录被剥字段路径的条数上限，避免超长上下文刷爆日志。
const arkFieldDropLimit = 32

type arkFieldSanitizer struct {
	dropped []string
	total   int
}

func (s *arkFieldSanitizer) drop(path string) {
	s.total++
	if len(s.dropped) < arkFieldDropLimit {
		s.dropped = append(s.dropped, path)
	}
}

// filterObject 按白名单删掉 obj 中不被允许的键，返回是否有改动。
func (s *arkFieldSanitizer) filterObject(path string, obj map[string]any, allowed map[string]bool) bool {
	if len(obj) == 0 || allowed == nil {
		return false
	}
	var doomed []string
	for key := range obj {
		if !allowed[key] {
			doomed = append(doomed, key)
		}
	}
	if len(doomed) == 0 {
		return false
	}
	sort.Strings(doomed) // 稳定的日志与测试输出
	for _, key := range doomed {
		delete(obj, key)
		s.drop(path + "." + key)
	}
	return true
}

func asObject(v any) (map[string]any, bool) {
	obj, ok := v.(map[string]any)
	return obj, ok
}

func itemType(obj map[string]any) string {
	t, _ := obj["type"].(string)
	return strings.TrimSpace(t)
}

// sanitizeResponsesPayload 对已解析的 Responses 请求体做逐层白名单裁剪。
func (s *arkFieldSanitizer) sanitizeResponsesPayload(root map[string]any) bool {
	changed := s.filterObject("", root, arkResponsesTopLevelFields)

	if obj, ok := asObject(root["reasoning"]); ok {
		changed = s.filterObject("reasoning", obj, arkResponsesReasoningFields) || changed
	}
	if obj, ok := asObject(root["text"]); ok {
		changed = s.filterObject("text", obj, arkResponsesTextFields) || changed
	}

	if items, ok := root["input"].([]any); ok {
		for i, raw := range items {
			item, ok := asObject(raw)
			if !ok {
				continue
			}
			itemPath := "input[" + strconv.Itoa(i) + "]"
			if allowed, known := arkResponsesInputItemFields[itemType(item)]; known {
				changed = s.filterObject(itemPath, item, allowed) || changed
			}
			// content 分片无论元素类型是否登记都要过一遍：未登记的元素类型本身会被
			// 火山拒绝，但已登记的（message）里混进 cache_control 这类分片键同样会 400。
			parts, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for j, rawPart := range parts {
				part, ok := asObject(rawPart)
				if !ok {
					continue
				}
				if allowed, known := arkResponsesContentPartFields[itemType(part)]; known {
					changed = s.filterObject(itemPath+".content["+strconv.Itoa(j)+"]", part, allowed) || changed
				}
			}
		}
	}

	if tools, ok := root["tools"].([]any); ok {
		for i, raw := range tools {
			tool, ok := asObject(raw)
			if !ok {
				continue
			}
			if itemType(tool) != "function" {
				continue
			}
			changed = s.filterObject("tools["+strconv.Itoa(i)+"]", tool, arkResponsesFunctionToolFields) || changed
		}
	}

	return changed
}

// sanitizeArkResponsesBody 按火山方舟口径裁剪 Responses 请求体。
//
// 返回裁剪后的 body 与被剥掉的字段路径；没有任何改动（或 body 不是可解析的 JSON
// 对象）时原样返回、dropped 为空——**绝不因为解析失败就改写或丢弃请求体**。
func sanitizeArkResponsesBody(body []byte) ([]byte, []string) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // 保住大整数与小数的原始写法，别把 max_output_tokens 变成 3.84e+05
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return body, nil
	}

	s := &arkFieldSanitizer{}
	if !s.sanitizeResponsesPayload(root) {
		return body, nil
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // 保持与入站字节一致，别把 prompt 里的 < > & 转义掉
	if err := enc.Encode(root); err != nil {
		return body, nil
	}
	return bytes.TrimRight(out.Bytes(), "\n"), s.dropped
}

// sanitizeResponsesBodyForAccount 是转发链路上的入口：只在 Responses 路径、
// 且账号命中火山口径时裁剪。
func sanitizeResponsesBodyForAccount(account *sdk.Account, body []byte, reqPath string) ([]byte, []string) {
	if !isResponsesRequestPath(reqPath) || !arkResponsesFieldFilterEnabled(account) {
		return body, nil
	}
	return sanitizeArkResponsesBody(body)
}
