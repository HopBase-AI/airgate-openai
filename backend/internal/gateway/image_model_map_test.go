package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

const (
	testImage25Flare            = "gpt-image-2.5-flare"
	testImage25Sunburst         = "gpt-image-2.5-sunburst"
	testImage25FlareUpstream    = "MM-H3-sft-Mlogic-High-25-image"
	testImage25SunburstUpstream = "MM-H3-sft-Mlogic-Max-25-image"
	testImage25Map              = `{"` + testImage25Flare + `":"` + testImage25FlareUpstream + `","` + testImage25Sunburst + `":"` + testImage25SunburstUpstream + `"}`
)

func acctWithImageMap(raw string, extra map[string]string) *sdk.Account {
	creds := map[string]string{}
	if raw != "" {
		creds[imageModelMapCredential] = raw
	}
	for k, v := range extra {
		creds[k] = v
	}
	return &sdk.Account{Credentials: creds}
}

func TestImageModelMapUpstreamForAccount(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		public string
		want   string
	}{
		{"flare 命中", testImage25Map, testImage25Flare, testImage25FlareUpstream},
		{"sunburst 命中", testImage25Map, testImage25Sunburst, testImage25SunburstUpstream},
		{"未命中返回空", testImage25Map, "gpt-image-2", ""},
		{"键大小写不敏感", `{"GPT-Image-2.5-Flare":"x-upstream"}`, testImage25Flare, "x-upstream"},
		{"映射到自身视为未映射", `{"gpt-image-2.5-flare":"gpt-image-2.5-flare"}`, testImage25Flare, ""},
		{"非法 JSON 不阻断", `{not json`, testImage25Flare, ""},
		{"未配置返回空", "", testImage25Flare, ""},
		{"空公开名返回空", testImage25Map, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageModelMapUpstreamForAccount(acctWithImageMap(tt.raw, nil), tt.public); got != tt.want {
				t.Fatalf("imageModelMapUpstreamForAccount(%q) = %q, want %q", tt.public, got, tt.want)
			}
		})
	}
	if got := imageModelMapUpstreamForAccount(nil, testImage25Flare); got != "" {
		t.Fatalf("nil account must return empty, got %q", got)
	}
}

// 优先级：image_model_map 命中 > gpt_image_2_upstream_model（仅 gpt-image-2 系）> yhshu 别名 > 原样。
func TestImageUpstreamModelIDForAccount_MapPriority(t *testing.T) {
	tests := []struct {
		name  string
		creds map[string]string
		model string
		want  string
	}{
		{
			name:  "map 命中优先于旧键",
			creds: map[string]string{imageModelMapCredential: `{"gpt-image-2":"canvas-25"}`, gptImage2UpstreamModelCredential: "canvas-20"},
			model: "gpt-image-2",
			want:  "canvas-25",
		},
		{
			name:  "map 未含 gpt-image-2 时旧键仍生效（向后兼容）",
			creds: map[string]string{imageModelMapCredential: testImage25Map, gptImage2UpstreamModelCredential: "canvas-20"},
			model: "gpt-image-2",
			want:  "canvas-20",
		},
		{
			name:  "同一账号 2.5 走 map",
			creds: map[string]string{imageModelMapCredential: testImage25Map, gptImage2UpstreamModelCredential: "canvas-20"},
			model: testImage25Flare,
			want:  testImage25FlareUpstream,
		},
		{
			name:  "旧键不影响 2.5（只对 gpt-image-2 系生效）",
			creds: map[string]string{gptImage2UpstreamModelCredential: "canvas-20"},
			model: testImage25Sunburst,
			want:  testImage25Sunburst,
		},
		{
			name:  "map 命中优先于 yhshu 自动别名",
			creds: map[string]string{"base_url": "https://www.yhshu.ai", imageModelMapCredential: `{"gpt-image-2":"custom-2"}`},
			model: "gpt-image-2",
			want:  "custom-2",
		},
		{
			name:  "map 未命中时 yhshu 别名保持",
			creds: map[string]string{"base_url": "https://www.yhshu.ai", imageModelMapCredential: testImage25Map},
			model: "gpt-image-2",
			want:  yhshuGPTImage2UpstreamModel,
		},
		{
			name:  "无任何配置原样透传",
			creds: map[string]string{"base_url": "https://relay.example.com"},
			model: testImage25Flare,
			want:  testImage25Flare,
		},
		{
			name:  "带空白的公开名也能命中",
			creds: map[string]string{imageModelMapCredential: testImage25Map},
			model: "  " + testImage25Flare + " ",
			want:  testImage25FlareUpstream,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageUpstreamModelIDForAccount(&sdk.Account{Credentials: tt.creds}, tt.model); got != tt.want {
				t.Fatalf("imageUpstreamModelIDForAccount(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestUpstreamImagesPath_ModelPlaceholder(t *testing.T) {
	account := &sdk.Account{Credentials: map[string]string{
		"base_url":                 "https://api.minimax.io",
		imagesPathPrefixCredential: "/v1/content/models/{model}",
	}}
	cases := []struct {
		path  string
		model string
		want  string
	}{
		{"/v1/images/generations", testImage25FlareUpstream, "/v1/content/models/" + testImage25FlareUpstream + "/generations"},
		{"/v1/images/edits", testImage25SunburstUpstream, "/v1/content/models/" + testImage25SunburstUpstream + "/edits"},
		{"/images/generations", "canvas-20", "/v1/content/models/canvas-20/generations"},
		{"/v1/chat/completions", testImage25FlareUpstream, "/v1/chat/completions"}, // 非图像请求不受影响
		{"/v1/images/generations", "", "/v1/content/models/{model}/generations"},   // 未解析出模型：占位符原样保留，便于从日志定位
	}
	for _, tc := range cases {
		if got := upstreamImagesPath(account, tc.path, tc.model); got != tc.want {
			t.Errorf("upstreamImagesPath(%q, model=%q) = %q, want %q", tc.path, tc.model, got, tc.want)
		}
	}
	if got := buildAPIKeyURL(account, upstreamImagesPath(account, "/v1/images/generations", testImage25FlareUpstream)); got != "https://api.minimax.io/v1/content/models/"+testImage25FlareUpstream+"/generations" {
		t.Errorf("最终 URL = %q", got)
	}

	// 不含占位符的前缀：传入模型也不改变既有行为。
	legacy := &sdk.Account{Credentials: map[string]string{imagesPathPrefixCredential: "/v1/content/models/canvas-20"}}
	if got := upstreamImagesPath(legacy, "/v1/images/edits", testImage25FlareUpstream); got != "/v1/content/models/canvas-20/edits" {
		t.Errorf("无占位符前缀应保持原样, got %q", got)
	}
}

func TestImagePublicModelID_MappedRequestRestoresPublicName(t *testing.T) {
	tests := []struct {
		name     string
		response string
		fallback string
		upstream string
		want     string
	}{
		{"上游回显自己的 ID → 公开名", testImage25FlareUpstream, testImage25Flare, testImage25FlareUpstream, testImage25Flare},
		{"上游不回 model（MiniMax 实测）→ 公开名", "", testImage25Flare, testImage25FlareUpstream, testImage25Flare},
		{"上游回显其它别名 → 仍按公开名", "canvas-20", testImage25Flare, testImage25FlareUpstream, testImage25Flare},
		{"未映射 → 原样透传", "vendor-x", "gemini-3-pro-image", "", "vendor-x"},
		{"映射到自身视为未映射 → 原样透传", "vendor-x", testImage25Flare, testImage25Flare, "vendor-x"},
		{"gpt-image-2 既有行为不变", "canvas-20", "gpt-image-2", "", "gpt-image-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imagePublicModelID(tt.response, tt.fallback, tt.upstream); got != tt.want {
				t.Fatalf("imagePublicModelID(%q, %q, %q) = %q, want %q", tt.response, tt.fallback, tt.upstream, got, tt.want)
			}
		})
	}
}

func TestNormalizeImagesResponseModelAliases_MappedImageModel(t *testing.T) {
	body := []byte(`{"model":"` + testImage25FlareUpstream + `","data":[{"model":"` + testImage25FlareUpstream + `","b64_json":"AA=="}]}`)
	got := normalizeImagesResponseModelAliases(body, testImage25Flare, testImage25FlareUpstream)
	if m := gjson.GetBytes(got, "model").String(); m != testImage25Flare {
		t.Fatalf("root model = %q, want %q; body=%s", m, testImage25Flare, got)
	}
	if m := gjson.GetBytes(got, "data.0.model").String(); m != testImage25Flare {
		t.Fatalf("item model = %q, want %q; body=%s", m, testImage25Flare, got)
	}
	if strings.Contains(string(got), testImage25FlareUpstream) {
		t.Fatalf("上游 ID 泄漏到响应: %s", got)
	}
}

// sumAccountCost 汇总 Usage 上所有指标与成本明细的账号成本，测试辅助。
func sumAccountCost(usage *sdk.Usage) (metrics, details float64) {
	if usage == nil {
		return 0, 0
	}
	for _, m := range usage.Metrics {
		metrics += m.AccountCost
	}
	for _, d := range usage.CostDetails {
		details += d.AccountCost
	}
	return metrics, details
}

// MiniMax 实测响应：顶层 created/data/usage/trace_id/base_resp，没有 model 字段；
// usage 与 OpenAI 同形。做过映射的请求必须：Usage.Model 记公开名、按公开名计价、
// 回给客户端的 model 字段是公开名、任何位置不出现上游 ID。
func TestHandleImagesResponse_MappedModelBillsAndEchoesPublicName(t *testing.T) {
	body := `{"created":1757400000,"data":[{"b64_json":"AA=="}],` +
		`"usage":{"total_tokens":224,"input_tokens":28,"output_tokens":196,` +
		`"input_tokens_details":{"text_tokens":20,"image_tokens":0,"cached_tokens":8},` +
		`"output_tokens_details":{"text_tokens":0,"image_tokens":196}},` +
		`"trace_id":"t-1","base_resp":{"status_code":0,"status_msg":"success"}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()
	imgReq := &imagesRequest{Model: testImage25FlareUpstream, UpstreamModel: testImage25FlareUpstream, Size: "1024x1024"}
	outcome, err := handleImagesResponseWithLogger(nil, resp, w, nil, time.Now(), testImage25Flare, imgReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("outcome kind = %v, want success", outcome.Kind)
	}
	if outcome.Usage == nil || outcome.Usage.Model != testImage25Flare {
		t.Fatalf("Usage.Model = %v, want %q", outcome.Usage, testImage25Flare)
	}
	client := w.Body.String()
	if got := gjson.Get(client, "model").String(); got != testImage25Flare {
		t.Fatalf("客户端可见 model = %q, want %q; body=%s", got, testImage25Flare, client)
	}
	if strings.Contains(client, testImage25FlareUpstream) {
		t.Fatalf("上游 ID 泄漏到客户端响应: %s", client)
	}

	// 成本必须等于「直接按公开名计费」的控制组（parseUsage 已把 cached 从 input 扣除：28-8=20）。
	want := newTokenUsage(testImage25Flare, "", 20, 196, 8, 0, 0)
	fillUsageCostPerImageBySize(want, 1, "1024x1024", "")
	gotM, gotD := sumAccountCost(outcome.Usage)
	wantM, wantD := sumAccountCost(want)
	if gotM != wantM || gotD != wantD {
		t.Fatalf("cost mismatch: metrics %v/%v details %v/%v", gotM, wantM, gotD, wantD)
	}
	if gotM <= 0 {
		t.Fatalf("cost must be positive, got %v", gotM)
	}
	// 判别力：若按上游 ID 计费会走关键字兜底到 gpt-image-1.5（cached 0.5 vs 1.25），成本不同。
	wrong := newTokenUsage(testImage25FlareUpstream, "", 20, 196, 8, 0, 0)
	fillUsageCostPerImageBySize(wrong, 1, "1024x1024", "")
	if wrongM, _ := sumAccountCost(wrong); wrongM == wantM {
		t.Fatalf("测试失去判别力：上游 ID 与公开名算出同一成本 %v", wrongM)
	}
}

// 未映射的既有路径完全不受影响：上游回显的 model 原样透传。
func TestHandleImagesResponse_UnmappedModelPassthrough(t *testing.T) {
	body := `{"created":1,"model":"gemini-3-pro-image","data":[{"b64_json":"AA=="}],"usage":{"input_tokens":10,"output_tokens":1120}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	w := httptest.NewRecorder()
	outcome, err := handleImagesResponseWithLogger(nil, resp, w, nil, time.Now(), "gemini-3-pro-image", &imagesRequest{Model: "gemini-3-pro-image", Size: "1024x1024"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "gemini-3-pro-image" {
		t.Fatalf("Usage.Model = %v, want gemini-3-pro-image", outcome.Usage)
	}
	if got := gjson.Get(w.Body.String(), "model").String(); got != "gemini-3-pro-image" {
		t.Fatalf("model = %q, want passthrough", got)
	}
}
