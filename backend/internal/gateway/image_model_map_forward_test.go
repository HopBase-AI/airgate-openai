package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

// 真链路：一个账号靠 image_model_map + images_path_prefix 的 {model} 占位同时服务
// 两个 2.5 模型。生成 / JSON 编辑（网关转 multipart）/ 客户端直传 multipart 三条出站
// 路径都必须：URL 落到映射后的上游模型路径、body.model 是上游 ID、响应与计费还原为公开名。
func TestForwardAPIKeyImageModelMapWithPathPlaceholder(t *testing.T) {
	paths := []struct {
		name      string
		path      string
		multipart bool
		wantTail  string
	}{
		{name: "generate_json", path: "/v1/images/generations", wantTail: "/generations"},
		{name: "edit_json", path: "/v1/images/edits", wantTail: "/edits"},
		{name: "edit_multipart", path: "/v1/images/edits", multipart: true, wantTail: "/edits"},
	}
	models := []struct {
		public   string
		upstream string
	}{
		{testImage25Flare, testImage25FlareUpstream},
		{testImage25Sunburst, testImage25SunburstUpstream},
	}
	_, imageBytes, err := decodeDataImageURL(tinyPNGDataURL)
	if err != nil {
		t.Fatalf("decodeDataImageURL: %v", err)
	}
	// MiniMax 实测响应：没有 model 字段。
	const upstreamBody = `{"created":1757400000,"data":[{"b64_json":"AA=="}],` +
		`"usage":{"total_tokens":216,"input_tokens":20,"output_tokens":196,` +
		`"input_tokens_details":{"text_tokens":20,"image_tokens":0,"cached_tokens":0},` +
		`"output_tokens_details":{"text_tokens":0,"image_tokens":196}},"trace_id":"t","base_resp":{"status_code":0}}`

	for _, pathCase := range paths {
		for _, m := range models {
			t.Run(pathCase.name+"_"+m.public, func(t *testing.T) {
				var upstreamPath, upstreamModel, upstreamContentType string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamPath = r.URL.Path
					upstreamContentType = r.Header.Get("Content-Type")
					if strings.HasPrefix(strings.ToLower(upstreamContentType), "multipart/") {
						if err := r.ParseMultipartForm(2 << 20); err != nil {
							t.Errorf("ParseMultipartForm: %v", err)
						} else {
							upstreamModel = r.FormValue("model")
						}
					} else {
						body, _ := io.ReadAll(r.Body)
						upstreamModel = gjson.GetBytes(body, "model").String()
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(upstreamBody))
				}))
				defer server.Close()

				headers := http.Header{}
				headers.Set("X-Forwarded-Path", pathCase.path)
				var body []byte
				if pathCase.multipart {
					var buf bytes.Buffer
					writer := multipart.NewWriter(&buf)
					_ = writer.WriteField("model", m.public)
					_ = writer.WriteField("prompt", "a product hero")
					_ = writer.WriteField("size", "1024x1024")
					part, createErr := writer.CreateFormFile("image", "input.png")
					if createErr != nil {
						t.Fatal(createErr)
					}
					_, _ = part.Write(imageBytes)
					if closeErr := writer.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					body = buf.Bytes()
					headers.Set("Content-Type", writer.FormDataContentType())
				} else {
					body = []byte(fmt.Sprintf(`{"model":%q,"prompt":"a product hero","size":"1024x1024"}`, m.public))
					if pathCase.path == "/v1/images/edits" {
						body = []byte(fmt.Sprintf(`{"model":%q,"prompt":"a product hero","size":"1024x1024","image":%q}`, m.public, tinyPNGDataURL))
					}
					headers.Set("Content-Type", "application/json")
				}

				g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
					Account: &sdk.Account{ID: 67, Credentials: map[string]string{
						"base_url":                 server.URL,
						"api_key":                  "sk-test",
						imageModelMapCredential:    testImage25Map,
						imagesPathPrefixCredential: "/v1/content/models/{model}",
					}},
					Model:   m.public,
					Body:    body,
					Headers: headers,
				}, "")
				if err != nil {
					t.Fatalf("forwardAPIKey returned err: %v", err)
				}
				if outcome.Kind != sdk.OutcomeSuccess {
					t.Fatalf("Kind = %v, want success; body=%s", outcome.Kind, outcome.Upstream.Body)
				}
				if want := "/v1/content/models/" + m.upstream + pathCase.wantTail; upstreamPath != want {
					t.Fatalf("upstream path = %q, want %q", upstreamPath, want)
				}
				if upstreamModel != m.upstream {
					t.Fatalf("upstream body model = %q, want %q (content-type %q)", upstreamModel, m.upstream, upstreamContentType)
				}
				if outcome.Usage == nil || outcome.Usage.Model != m.public {
					t.Fatalf("billing model = %v, want public %q", outcome.Usage, m.public)
				}
				if got := gjson.GetBytes(outcome.Upstream.Body, "model").String(); got != m.public {
					t.Fatalf("client-visible model = %q, want %q; body=%s", got, m.public, outcome.Upstream.Body)
				}
				if bytes.Contains(outcome.Upstream.Body, []byte(m.upstream)) {
					t.Fatalf("upstream ID leaked to client: %s", outcome.Upstream.Body)
				}
			})
		}
	}
}

// 既有 canvas-20 账号（固定前缀 + gpt_image_2_upstream_model，无占位符）行为不变。
func TestForwardAPIKeyLegacyImagesPathPrefixUnchanged(t *testing.T) {
	var upstreamPath, upstreamModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		upstreamModel = gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":20,"output_tokens":196}}`))
	}))
	defer server.Close()

	headers := http.Header{}
	headers.Set("X-Forwarded-Path", "/v1/images/generations")
	headers.Set("Content-Type", "application/json")
	g := &OpenAIGateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	outcome, err := g.forwardAPIKey(context.Background(), &sdk.ForwardRequest{
		Account: &sdk.Account{ID: 67, Credentials: map[string]string{
			"base_url":                       server.URL,
			"api_key":                        "sk-test",
			gptImage2UpstreamModelCredential: "canvas-20",
			imagesPathPrefixCredential:       "/v1/content/models/canvas-20",
		}},
		Model:   "gpt-image-2",
		Body:    []byte(`{"model":"gpt-image-2","prompt":"a product hero","size":"1024x1024"}`),
		Headers: headers,
	}, "")
	if err != nil {
		t.Fatalf("forwardAPIKey returned err: %v", err)
	}
	if outcome.Kind != sdk.OutcomeSuccess {
		t.Fatalf("Kind = %v, want success; body=%s", outcome.Kind, outcome.Upstream.Body)
	}
	if upstreamPath != "/v1/content/models/canvas-20/generations" {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	if upstreamModel != "canvas-20" {
		t.Fatalf("upstream model = %q, want canvas-20", upstreamModel)
	}
	if outcome.Usage == nil || outcome.Usage.Model != "gpt-image-2" {
		t.Fatalf("billing model = %v, want gpt-image-2", outcome.Usage)
	}
	if got := gjson.GetBytes(outcome.Upstream.Body, "model").String(); got != "gpt-image-2" {
		t.Fatalf("client-visible model = %q, want gpt-image-2", got)
	}
}
