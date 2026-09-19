package gateway

import sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

//go:generate go run ../../cmd/genmanifest

const (
	PluginID             = "gateway-openai"
	PluginDisplayName    = "OpenAI 网关"
	PluginDescription    = "OpenAI Responses API / Chat Completions 转发"
	PluginAuthor         = "hopbase"
	PluginPlatform       = "openai"
	PluginMode           = "simple"
	PluginMinCoreVersion = "1.0.0"
)

// PluginVersion 插件版本号。
//
// 默认值是开发态版本，正式 release 构建时由 GitHub Actions 通过 ldflags 注入：
//
//	go build -ldflags "-X 'github.com/DouDOU-start/airgate-openai/backend/internal/gateway.PluginVersion=0.1.42'"
//
// 这样 git tag 即唯一发版来源，无需手动维护 plugin.yaml / metadata.go 里的版本字段。
var PluginVersion = "dev"

func PluginDependencies() []string {
	return []string{}
}

func BuildPluginInfo() sdk.PluginInfo {
	return sdk.PluginInfo{
		ID:          PluginID,
		Name:        PluginDisplayName,
		Version:     PluginVersion,
		SDKVersion:  sdk.SDKVersion,
		Description: PluginDescription,
		Author:      PluginAuthor,
		Type:        sdk.PluginTypeGateway,
		Capabilities: []sdk.Capability{
			sdk.CapabilityHostInvoke,
			sdk.CapabilityForHostMethod(hostMethodTasksCreate),
			sdk.CapabilityForHostMethod(hostMethodTasksUpdate),
			sdk.CapabilityForHostMethod(hostMethodTasksGet),
			sdk.CapabilityForHostMethod(hostMethodTasksList),
			sdk.CapabilityForHostMethod(hostMethodGatewayForward),
			sdk.CapabilityForHostMethod(hostMethodAssetsStore),
			sdk.CapabilityForHostMethod(hostMethodAssetsStoreURL),
			sdk.CapabilityForHostMethod(hostMethodAssetsGetBytes),
			sdk.CapabilityForHostMethod(hostMethodModelsCatalog),
			sdk.CapabilityForHostMethod(hostMethodSchedulerBindResponse),
		},
		Metadata: map[string]string{
			"account.oauth_plans": `[
				{"key":"free","label":"Free","credential_key":"plan_type","matches":["free"]},
				{"key":"plus","label":"Plus","credential_key":"plan_type","matches":["plus"]},
				{"key":"pro","label":"Pro","credential_key":"plan_type","matches":["pro"]}
			]`,
		},
		AccountTypes: []sdk.AccountType{
			{
				Key:         "apikey",
				Label:       "API Key",
				Description: "支持所有提供 Responses 标准接口的服务",
				Fields: []sdk.CredentialField{
					{Key: "api_key", Label: "API Key", Type: "password", Required: true, Placeholder: "sk-..."},
					{Key: "base_url", Label: "API 地址", Type: "text", Required: false, Placeholder: "https://api.openai.com"},
					{Key: gptImage2UpstreamModelCredential, Label: "GPT Image 2 上游模型 ID", Type: "text", Required: false, Placeholder: "留空时保持 gpt-image-2；特殊上游可填写专用模型 ID（仅对 gpt-image-2 生效，多模型请用「图像模型 ID 映射」）"},
					{Key: imageModelMapCredential, Label: "图像模型 ID 映射", Type: "text", Required: false, Placeholder: `可选；JSON 对象，公开图像模型名→该上游真实 ID，对任意图像模型生效且优先于「GPT Image 2 上游模型 ID」，例如 {"gpt-image-2.5-flare":"MM-H3-sft-Mlogic-High-25-image"}`},
					{Key: imagesPathPrefixCredential, Label: "图像端点路径前缀", Type: "text", Required: false, Placeholder: "可选；非标图像中转的路径前缀，生成/编辑分别拼 /generations、/edits，例如 /v1/content/models/canvas-20；可用 {model} 占位按映射后的上游模型 ID 替换，如 /v1/content/models/{model}"},
					{Key: imagesAsyncCredential, Label: "图像异步任务模式", Type: "text", Required: false, Placeholder: "可选；填 true 时图像请求走 X-Async 提交 + 任务轮询（MiniMax canvas-20 契约）"},
					{Key: geminiImageProtocolCredential, Label: "Gemini 生图协议", Type: "text", Required: false, Placeholder: "默认 chat_completions；纯 Images API 上游填 images_api"},
					{Key: chatModelMapCredential, Label: "对话模型 ID 映射", Type: "text", Required: false, Placeholder: `可选；JSON 对象，公开模型名→该上游真实 ID，例如 {"deepseek-v4-pro-202606":"deepseek-v4-pro-ga-260813"}`},
					// 三个守卫时限：留空即用插件默认值，只有确知该上游行为异常时才按账号覆盖。
					// 写法是 Go duration（"150s"、"3m"），**必须带单位**——裸数字解析失败会被
					// 静默忽略并回落默认值，看不出报错（2026-09-19 排查时确认过这个坑）。
					{Key: "stream_idle_timeout", Label: "流式读空闲上限", Type: "text", Required: false, Placeholder: `留空=默认 150s。流开始后连续多久没有任何数据就判定上游卡死并中止。调小=更快失败，但会误杀"憋大段工具参数"的慢上游；调大=更能容忍慢上游，但真卡死时占用并发槽位更久`},
					{Key: "first_byte_timeout", Label: "响应头等待上限", Type: "text", Required: false, Placeholder: "留空=默认 60s。等上游返回 HTTP 响应头的上限，超时即换账号重试。大上下文账号可放宽到 180s"},
					{Key: "hedge_after", Label: "对冲换号阈值", Type: "text", Required: false, Placeholder: "留空=不对冲。首字节迟迟不来时并行向另一个账号发起同一请求，先返回者胜；只在首字前生效，已出内容不对冲"},
				},
			},
			{
				Key:         "oauth",
				Label:       "OAuth 登录",
				Description: "通过浏览器授权登录 ChatGPT 账号",
				Fields: []sdk.CredentialField{
					{Key: "access_token", Label: "Access Token", Type: "password", Required: false, Placeholder: "授权后自动填充", EditDisabled: true},
					{Key: "refresh_token", Label: "Refresh Token", Type: "password", Required: false, Placeholder: "授权后自动填充"},
					{Key: "session_token", Label: "Session Token (JWE)", Type: "password", Required: false, Placeholder: "Session 导入后自动填充"},
					{Key: "chatgpt_account_id", Label: "ChatGPT Account ID", Type: "text", Required: false, Placeholder: "授权后自动填充", EditDisabled: true},
					// 与 apikey 类型同义，见上方说明；同样留空即用默认值、必须带单位。
					{Key: "stream_idle_timeout", Label: "流式读空闲上限", Type: "text", Required: false, Placeholder: "留空=默认 150s。流开始后连续多久没有任何数据就判定上游卡死并中止"},
					{Key: "first_byte_timeout", Label: "响应头等待上限", Type: "text", Required: false, Placeholder: "留空=默认 60s。等上游返回 HTTP 响应头的上限，超时即换账号重试"},
					{Key: "hedge_after", Label: "对冲换号阈值", Type: "text", Required: false, Placeholder: "留空=不对冲。首字节迟迟不来时并行向另一个账号发起同一请求，先返回者胜"},
				},
			},
		},
		FrontendWidgets: []sdk.FrontendWidget{
			{Slot: sdk.SlotAccountIdentity, EntryFile: "index.js", Title: "OpenAI 账号身份"},
			{Slot: sdk.SlotAccountCreate, EntryFile: "index.js", Title: "创建 OpenAI 账号"},
			{Slot: sdk.SlotAccountEdit, EntryFile: "index.js", Title: "编辑 OpenAI 账号"},
			{Slot: sdk.SlotAccountUsageWindow, EntryFile: "index.js", Title: "账号用量窗口"},
			{Slot: sdk.SlotUsageMetricDetail, EntryFile: "index.js", Title: "OpenAI 计量明细"},
			{Slot: sdk.SlotUsageCostDetail, EntryFile: "index.js", Title: "OpenAI 费用明细"},
		},
		InstructionPresets: []string{"default", "simple", "nsfw", "cc"},
	}
}

func PluginRouteDefinitions() []sdk.RouteDefinition {
	return []sdk.RouteDefinition{
		{Method: "POST", Path: "/v1/responses", Description: "Responses API（Codex 核心端点）"},
		{Method: "POST", Path: "/v1/chat/completions", Description: "Chat Completions API"},
		{Method: "POST", Path: "/v1/messages", Description: "Anthropic Messages API（协议翻译）", Metadata: anthropicRouteMetadata()},
		{Method: "POST", Path: "/v1/messages/count_tokens", Description: "Anthropic Count Tokens（兼容回退）", Metadata: anthropicRouteMetadata()},
		{Method: "GET", Path: "/v1/models", Description: "模型列表", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "POST", Path: "/v1/images/generations", Description: "Images API（文生图）"},
		{Method: "POST", Path: "/v1/images/edits", Description: "Images API（图生图 / 编辑）"},
		{Method: "GET", Path: "/v1/images/tasks", Description: "Images Task 状态查询", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "GET", Path: "/v1/images/tasks/list", Description: "Images Task 历史列表", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "WS", Path: "/v1/responses", Description: "Responses API（WebSocket）"},
		// 不带 /v1 前缀的别名路由，方便用户配置时直接使用站点根地址
		{Method: "POST", Path: "/responses", Description: "Responses API（无 /v1 前缀）"},
		{Method: "POST", Path: "/chat/completions", Description: "Chat Completions API（无 /v1 前缀）"},
		{Method: "POST", Path: "/messages", Description: "Anthropic Messages API（无 /v1 前缀）", Metadata: anthropicRouteMetadata()},
		{Method: "POST", Path: "/messages/count_tokens", Description: "Anthropic Count Tokens（无 /v1 前缀）", Metadata: anthropicRouteMetadata()},
		{Method: "GET", Path: "/models", Description: "模型列表（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "POST", Path: "/images/generations", Description: "Images API（文生图，无 /v1 前缀）"},
		{Method: "POST", Path: "/images/edits", Description: "Images API（图生图，无 /v1 前缀）"},
		{Method: "GET", Path: "/images/tasks", Description: "Images Task 状态查询（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "GET", Path: "/images/tasks/list", Description: "Images Task 历史列表（无 /v1 前缀）", Metadata: map[string]string{"metadata_only": "true"}},
		{Method: "WS", Path: "/responses", Description: "Responses API WebSocket（无 /v1 前缀）"},
	}
}
