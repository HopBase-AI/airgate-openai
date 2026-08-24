package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// TestBillableOutputTokensGrokReasoning grok 两种接口的 usage 口径差（均实测）：
// chat completions 的 completion_tokens 不含 reasoning（须并入）；Responses 的
// output_tokens 已含（并入即双算）；OpenAI 系模型两种口径都保持原样。
func TestBillableOutputTokensGrokReasoning(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		chatStyle bool
		output    int
		reasoning int
		want      int
	}{
		{"grok chat 并入", "grok-4.3", true, 1, 158, 159},
		{"grok responses 不并入", "grok-4.20-multi-agent-0309", false, 1085, 1071, 1085},
		{"gpt chat 不并入", "gpt-5.4", true, 100, 60, 100},
		{"gpt responses 不并入", "gpt-5.4", false, 100, 60, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := billableOutputTokens(tc.model, tc.output, tc.reasoning, tc.chatStyle); got != tc.want {
				t.Fatalf("billableOutputTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestParseUsageChatCompletionStyle 口径判定必须来自响应体 object 自描述——grok
// 的 chat 响应同时带 input_tokens/output_tokens 镜像字段，按字段名猜会判错。
func TestParseUsageChatCompletionStyle(t *testing.T) {
	chatBody := `{"object":"chat.completion","model":"grok-4.3","usage":{"prompt_tokens":194,"completion_tokens":1,"input_tokens":194,"output_tokens":1,"prompt_tokens_details":{"cached_tokens":192},"completion_tokens_details":{"reasoning_tokens":138}}}`
	parsed := parseUsage([]byte(chatBody))
	if !parsed.chatCompletionStyle {
		t.Fatal("chat.completion 响应应判为 chat 口径")
	}
	if parsed.outputTokens != 1 || parsed.reasoningOutputTokens != 138 {
		t.Fatalf("chat 解析 output=%d reasoning=%d", parsed.outputTokens, parsed.reasoningOutputTokens)
	}

	responsesBody := `{"object":"response","model":"grok-4.20-multi-agent-0309","usage":{"input_tokens":2663,"output_tokens":1085,"input_tokens_details":{"cached_tokens":2560},"output_tokens_details":{"reasoning_tokens":1071}}}`
	parsed = parseUsage([]byte(responsesBody))
	if parsed.chatCompletionStyle {
		t.Fatal("Responses 响应不得判为 chat 口径")
	}
	if parsed.outputTokens != 1085 || parsed.reasoningOutputTokens != 1071 {
		t.Fatalf("responses 解析 output=%d reasoning=%d", parsed.outputTokens, parsed.reasoningOutputTokens)
	}
	// 端到端口径：chat 并入、responses 保持。
	if got := billableOutputTokens("grok-4.3", 1, 138, true); got != 139 {
		t.Fatalf("chat 口径 billable = %d, want 139", got)
	}
	if got := billableOutputTokens("grok-4.20-multi-agent-0309", parsed.outputTokens, parsed.reasoningOutputTokens, parsed.chatCompletionStyle); got != 1085 {
		t.Fatalf("responses 口径 billable = %d, want 1085", got)
	}
}

// TestFillUsagePerUnitImageCost 按张计费：输出 = 张数 × 档位价；输入参考图费
// 每请求收一次（上游实测 1 张、2 张编辑同价）；整单基数写入 override 键。
func TestFillUsagePerUnitImageCost(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		numImages   int
		resolution  string
		inputImages int
		wantImage   float64
		wantInput   float64
		wantTier    string
	}{
		{"2.0 两张 2k 两参考图", "grok-imagine-image-2.0", 2, "2k", 2, 0.16, 0.01, "2k"},
		{"2.0 缺省档按 1k", "grok-imagine-image-2.0", 1, "", 0, 0.06, 0, "1k"},
		{"基础版无 2k 档回落 1k", "grok-imagine-image", 1, "2k", 1, 0.02, 0.002, "1k"},
		{"quality 1k 单参考图", "grok-imagine-image-quality", 1, "1k", 1, 0.05, 0.01, "1k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := model.Lookup(tc.model)
			usage := newTokenUsage(tc.model, "", 0, 0, 0, 0, 0)
			fillUsagePerUnitImageCost(usage, spec, tc.numImages, tc.resolution, tc.inputImages)

			var imageCost, inputCost float64
			for _, detail := range usage.CostDetails {
				switch detail.Key {
				case usageCostImage:
					imageCost = detail.AccountCost
					if detail.Metadata["image_tier"] != tc.wantTier {
						t.Fatalf("image_tier = %q, want %q", detail.Metadata["image_tier"], tc.wantTier)
					}
				case "image_input_tokens":
					inputCost = detail.AccountCost
				}
			}
			if !almostEqual(imageCost, tc.wantImage, 1e-9) {
				t.Fatalf("图片输出费 = %v, want %v", imageCost, tc.wantImage)
			}
			if !almostEqual(inputCost, tc.wantInput, 1e-9) {
				t.Fatalf("输入参考图费 = %v, want %v", inputCost, tc.wantInput)
			}
			wantBase := tc.wantImage + tc.wantInput
			gotBase := usage.Metadata[imageBillingBaseCostOverrideMetadataKey]
			if parsed := gjson.Parse(gotBase).Float(); !almostEqual(parsed, wantBase, 1e-9) {
				t.Fatalf("override 基数 = %q, want %v", gotBase, wantBase)
			}
			if usage.Metadata["billing_mode"] != "per_image" {
				t.Fatalf("billing_mode = %q", usage.Metadata["billing_mode"])
			}
		})
	}
}

// TestHandleImagesResponseGrokPerUnit grok 图像响应没有任何 token usage，
// 走按张分支后整单成本 = 张数 × 档位价（经 imgReq.Resolution 决定档位）。
func TestHandleImagesResponseGrokPerUnit(t *testing.T) {
	body := `{"data":[{"url":"https://imgen.x.ai/a.jpeg","mime_type":"image/jpeg"}],"usage":{"cost_in_usd_ticks":800000000}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       ioNopCloserFromString(body),
	}
	imgReq := &imagesRequest{Resolution: "2k"}
	outcome, err := handleImagesResponse(resp, nil, nil, time.Now(), "grok-imagine-image-2.0", imgReq)
	if err != nil {
		t.Fatalf("handleImagesResponse returned err: %v", err)
	}
	if outcome.Usage == nil {
		t.Fatal("Usage 为空")
	}
	var imageCost float64
	for _, detail := range outcome.Usage.CostDetails {
		if detail.Key == usageCostImage {
			imageCost = detail.AccountCost
		}
	}
	if !almostEqual(imageCost, 0.08, 1e-9) {
		t.Fatalf("2k 单张费 = %v, want 0.08", imageCost)
	}
	if got := outcome.Usage.Metadata[imageBillingBaseCostOverrideMetadataKey]; gjson.Parse(got).Float() != 0.08 {
		t.Fatalf("override 基数 = %q, want 0.08", got)
	}
}

// TestValidatePerUnitImagesRequest 档位 / mask / 输入图上限约束。
func TestValidatePerUnitImagesRequest(t *testing.T) {
	spec20 := model.Lookup("grok-imagine-image-2.0")
	specBase := model.Lookup("grok-imagine-image")
	cases := []struct {
		name    string
		spec    model.Spec
		req     *imagesRequest
		wantErr string
	}{
		{"合法 2k", spec20, &imagesRequest{Resolution: "2k"}, ""},
		{"缺省档", spec20, &imagesRequest{}, ""},
		{"4k 拒绝", spec20, &imagesRequest{Resolution: "4k"}, "resolution"},
		// 官方枚举与价目档无关:基础版传 2k 上游照收(平价),必须放行。
		{"基础版 2k 放行", specBase, &imagesRequest{Resolution: "2k"}, ""},
		{"mask 拒绝", spec20, &imagesRequest{Mask: "data:image/png;base64,x"}, "mask"},
		{"两张输入图放行", spec20, &imagesRequest{Images: []string{"https://a", "https://b"}}, ""},
		{"三张输入图拒绝", spec20, &imagesRequest{Images: []string{"https://a", "https://b", "https://c"}}, "输入图"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePerUnitImagesRequest(tc.spec, tc.req)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildPerUnitImagesEditJSONBody xAI edits 契约改写：单图 = {"url":...}
// 结构、多图 = 字符串数组（对象数组上游实测 422）；JSON 原体只改 image 字段，
// resolution / aspect_ratio 等透传参数保持不动。
func TestBuildPerUnitImagesEditJSONBody(t *testing.T) {
	t.Run("JSON 单图改写并保留透传参数", func(t *testing.T) {
		body := []byte(`{"model":"grok-imagine-image-2.0","prompt":"p","image":"https://a.jpg","resolution":"2k","aspect_ratio":"16:9"}`)
		imgReq := &imagesRequest{Model: "grok-imagine-image-2.0", Prompt: "p", Images: []string{"https://a.jpg"}, Resolution: "2k"}
		out, contentType, err := buildPerUnitImagesEditJSONBody(body, "application/json", imgReq)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if contentType != "application/json" {
			t.Fatalf("contentType = %q", contentType)
		}
		if got := gjson.GetBytes(out, "image.url").String(); got != "https://a.jpg" {
			t.Fatalf("image.url = %q", got)
		}
		if got := gjson.GetBytes(out, "aspect_ratio").String(); got != "16:9" {
			t.Fatalf("aspect_ratio 透传丢失: %q", got)
		}
	})
	t.Run("多图改写成字符串数组", func(t *testing.T) {
		body := []byte(`{"model":"grok-imagine-image-2.0","prompt":"p","image":["https://a.jpg","https://b.jpg"]}`)
		imgReq := &imagesRequest{Model: "grok-imagine-image-2.0", Prompt: "p", Images: []string{"https://a.jpg", "https://b.jpg"}}
		out, _, err := buildPerUnitImagesEditJSONBody(body, "application/json", imgReq)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		arr := gjson.GetBytes(out, "image").Array()
		if len(arr) != 2 || arr[0].Type != gjson.String || arr[0].String() != "https://a.jpg" {
			t.Fatalf("image 数组形态错误: %s", gjson.GetBytes(out, "image").Raw)
		}
	})
	t.Run("multipart 重建含 resolution", func(t *testing.T) {
		imgReq := &imagesRequest{Model: "grok-imagine-image-2.0", Prompt: "p", Images: []string{"data:image/png;base64,x"}, Resolution: "1k", N: 1}
		out, contentType, err := buildPerUnitImagesEditJSONBody([]byte("binary"), "multipart/form-data; boundary=x", imgReq)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if contentType != "application/json" {
			t.Fatalf("contentType = %q", contentType)
		}
		if got := gjson.GetBytes(out, "resolution").String(); got != "1k" {
			t.Fatalf("resolution = %q", got)
		}
		if got := gjson.GetBytes(out, "image.url").String(); got != "data:image/png;base64,x" {
			t.Fatalf("image.url = %q", got)
		}
	})
}

// TestParseImagesJSONXAIImageForms /edits 的 image 字段兼容 xAI 结构写法。
func TestParseImagesJSONXAIImageForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"对象形态", `{"prompt":"p","image":{"url":"https://a.jpg"}}`, []string{"https://a.jpg"}},
		{"对象数组", `{"prompt":"p","image":[{"url":"https://a.jpg"},{"url":"https://b.jpg"}]}`, []string{"https://a.jpg", "https://b.jpg"}},
		{"字符串数组", `{"prompt":"p","image":["https://a.jpg"]}`, []string{"https://a.jpg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseImagesRequest([]byte(tc.body), "application/json", true)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(req.Images) != len(tc.want) {
				t.Fatalf("Images = %v, want %v", req.Images, tc.want)
			}
			for i := range tc.want {
				if req.Images[i] != tc.want[i] {
					t.Fatalf("Images[%d] = %q, want %q", i, req.Images[i], tc.want[i])
				}
			}
		})
	}
}
