package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// TestSetUsageTokensGrokReasoning grok 上游 completion_tokens 不含 reasoning
// tokens，计费输出必须并入；OpenAI 系模型保持原样。
func TestSetUsageTokensGrokReasoning(t *testing.T) {
	cases := []struct {
		model      string
		wantOutput int
	}{
		{"grok-4.3", 159}, // 1 completion + 158 reasoning（实测形态）
		{"gpt-5.4", 1},    // OpenAI 口径 completion 已含 reasoning，不得重复加
	}
	for _, tc := range cases {
		usage := newTokenUsage(tc.model, "", 2, 1, 192, 158, 0)
		if got := usageMetricInt(usage, usageMetricOutputTokens); got != tc.wantOutput {
			t.Fatalf("%s 输出 token = %d, want %d", tc.model, got, tc.wantOutput)
		}
		if got := usageMetricInt(usage, usageMetricReasoningOutputTokens); got != 158 {
			t.Fatalf("%s 推理 token 指标 = %d, want 158", tc.model, got)
		}
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
		{"基础版 2k 拒绝", specBase, &imagesRequest{Resolution: "2k"}, "resolution"},
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
