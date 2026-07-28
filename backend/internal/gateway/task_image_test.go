package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

type imageTaskHost struct {
	t              *testing.T
	forwardHeaders http.Header
	forwardBody    []byte
	forwardPath    string
	forwardModel   string
	updateCalls    int
}

func (h *imageTaskHost) Invoke(_ context.Context, req sdk.HostInvokeRequest) (*sdk.HostInvokeResponse, error) {
	h.t.Helper()
	switch req.Method {
	case hostMethodTasksUpdate:
		h.updateCalls++
		return &sdk.HostInvokeResponse{Status: "ok", Payload: map[string]any{}}, nil
	case hostMethodGatewayForward:
		h.forwardHeaders = headerFromPayload(req.Payload["headers"])
		h.forwardBody = bytesFromPayload(req.Payload["body"])
		h.forwardPath = stringFromAny(req.Payload["path"])
		h.forwardModel = stringFromAny(req.Payload["model"])
		return &sdk.HostInvokeResponse{
			Status: "ok",
			Payload: map[string]any{
				"status_code": http.StatusOK,
				"body":        `{"data":[{"b64_json":"AA=="}]}`,
			},
		}, nil
	case hostMethodAssetsStore:
		return &sdk.HostInvokeResponse{
			Status: "ok",
			Payload: map[string]any{
				"public_url": "/assets-runtime/generated/1/test.png",
				"object_key": "generated/1/test.png",
			},
		}, nil
	default:
		body, _ := json.Marshal(req.Payload)
		return nil, fmt.Errorf("unexpected method %s payload=%s", req.Method, body)
	}
}

func TestExecuteImageTaskKeepsPublicModelForFinalAccountDispatch(t *testing.T) {
	tests := []struct {
		name     string
		taskType string
		path     string
		size     string
		images   []string
	}{
		{name: "generate_1k", taskType: taskTypeImageGenerate, path: "/v1/images/generations", size: "1024x1024"},
		{name: "generate_4k", taskType: taskTypeImageGenerate, path: "/v1/images/generations", size: "3840x2160"},
		{name: "edit_2k", taskType: taskTypeImageEdit, path: "/v1/images/edits", size: "2048x1152", images: []string{tinyPNGDataURL}},
		{name: "edit_4k", taskType: taskTypeImageEdit, path: "/v1/images/edits", size: "3840x2160", images: []string{tinyPNGDataURL}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := &imageTaskHost{t: t}
			g := &OpenAIGateway{host: host, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			rt := &TaskRuntime{g: g, taskID: int64(300 + i), logger: g.logger}
			input := map[string]any{
				"prompt": "a product hero",
				"model":  "gpt-image-2",
				"size":   tt.size,
			}
			if len(tt.images) > 0 {
				input["images"] = tt.images
			}
			if err := executeImageTask(context.Background(), g, sdk.HostTask{
				ID:       int64(300 + i),
				UserID:   1,
				TaskType: tt.taskType,
				Input:    input,
			}, rt, tt.path); err != nil {
				t.Fatalf("executeImageTask returned err: %v", err)
			}
			if host.forwardPath != tt.path {
				t.Fatalf("forward path = %q, want %q", host.forwardPath, tt.path)
			}
			if host.forwardModel != "gpt-image-2" || gjson.GetBytes(host.forwardBody, "model").String() != "gpt-image-2" {
				t.Fatalf("account dispatch must retain public alias: model=%q body=%s", host.forwardModel, host.forwardBody)
			}
			if gjson.GetBytes(host.forwardBody, "size").String() != tt.size {
				t.Fatalf("size = %q, want %q", gjson.GetBytes(host.forwardBody, "size").String(), tt.size)
			}
		})
	}
}

func (h *imageTaskHost) InvokeStream(context.Context, sdk.HostStreamRequest) (sdk.HostStream, error) {
	return nil, fmt.Errorf("not used")
}

func TestExecuteImageTaskDeclaresOpenAIPlatformForCompatibleGeminiModel(t *testing.T) {
	host := &imageTaskHost{t: t}
	g := &OpenAIGateway{host: host, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rt := &TaskRuntime{g: g, taskID: 208, logger: g.logger}

	err := executeImageTask(context.Background(), g, sdk.HostTask{
		ID:       208,
		UserID:   1,
		TaskType: taskTypeImageGenerate,
		Input: map[string]any{
			"prompt":   "生成一个小狗",
			"model":    "gemini-3-pro-image",
			"size":     "1024x1024",
			"group_id": int64(18),
		},
	}, rt, "/v1/images/generations")
	if err != nil {
		t.Fatalf("executeImageTask returned err: %v", err)
	}
	if got := host.forwardHeaders.Get("X-Airgate-Platform"); got != PluginPlatform {
		t.Fatalf("X-Airgate-Platform = %q, want %q; headers=%v", got, PluginPlatform, host.forwardHeaders)
	}
	if got := host.forwardHeaders.Get(taskExecHeader); !strings.EqualFold(got, "true") {
		t.Fatalf("%s = %q, want true", taskExecHeader, got)
	}
	if host.updateCalls < 2 {
		t.Fatalf("updateCalls = %d, want progress and complete updates", host.updateCalls)
	}
}
