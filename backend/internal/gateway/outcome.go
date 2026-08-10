package gateway

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"

	"github.com/DouDOU-start/airgate-openai/backend/internal/model"
)

// 构造 ForwardOutcome 的小 helper，避免各路径散落一堆 struct literal。
//
// Success 必带 Usage，fillCost 会基于 Model / ServiceTier 补成本字段。
// ClientError / AccountRateLimited / AccountDead / UpstreamTransient 都从
// Upstream + 上游错误消息推出 Kind。

const (
	usageCurrencyUSD              = "USD"
	defaultImageTokenBillingModel = "gpt-5.5"

	usageAttrModel       = "model"
	usageAttrServiceTier = "service_tier"
	usageAttrImageSize   = "image_size"

	usageMetricInputTokens           = "input_tokens"
	usageMetricCachedInputTokens     = "cached_input_tokens"
	usageMetricOutputTokens          = "output_tokens"
	usageMetricReasoningOutputTokens = "reasoning_output_tokens"
	usageMetricTotalTokens           = "total_tokens"
	usageMetricImages                = "images"

	usageCostInput       = "input_tokens"
	usageCostCachedInput = "cached_input_tokens"
	usageCostOutput      = "output_tokens"
	usageCostImage       = "images"
	usageCostImageTool   = "image_tool"
)

// successOutcome 构造 Success 判决，Usage 由调用方填。Duration 由调用方填。
func successOutcome(statusCode int, body []byte, headers http.Header, usage *sdk.Usage) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind: sdk.OutcomeSuccess,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Usage: usage,
	}
}

// failureOutcome 从 HTTP 状态码 + 错误消息分类并构造非 Success 的 Outcome。
// 会原样保留 Upstream（Body / Headers / StatusCode）供 Core 在 ClientError 路径下透传。
func failureOutcome(statusCode int, body []byte, headers http.Header, message string, retryAfter time.Duration) sdk.ForwardOutcome {
	classificationText := failureClassificationText(body, message)
	kind := classifyHTTPFailureBody(statusCode, body, message)
	if retryAfter <= 0 {
		retryAfter = extractRetryAfterHeader(headers)
	}
	if retryAfter <= 0 && kind == sdk.OutcomeAccountRateLimited {
		retryAfter = openAIRateLimitReset(body)
	}
	if retryAfter <= 0 && kind == sdk.OutcomeAccountRateLimited {
		retryAfter = openAIRateLimitResetFromHeaders(headers)
	}
	if retryAfter <= 0 && (kind == sdk.OutcomeAccountRateLimited || kind == sdk.OutcomeUpstreamTransient) {
		retryAfter = defaultRetryAfterForKind(kind, statusCode, classificationText)
	}
	retryAfter = cappedUpstreamRetryAfterDuration(retryAfter)
	reason := message
	if reason != "" {
		reason = fmt.Sprintf("HTTP %d: %s", statusCode, message)
	}
	return sdk.ForwardOutcome{
		Kind: kind,
		Upstream: sdk.UpstreamResponse{
			StatusCode: statusCode,
			Headers:    headers,
			Body:       body,
		},
		Reason:     reason,
		RetryAfter: retryAfter,
	}
}

// openAIRateLimitReset extracts an explicit OAuth rate-limit reset from an
// upstream JSON error body. A transport Retry-After header remains higher
// priority; callers use this only when that header supplied no valid delay.
//
// The fields are meaningful only after the response has been classified as an
// account-level rate limit, so overload and generic 5xx responses cannot open
// a cooldown merely by carrying a similarly named field.
func openAIRateLimitReset(body []byte) time.Duration {
	return openAIRateLimitResetFromNodes(body)
}

func webSocketHandshakeFailureOutcome(resp *http.Response, err error) sdk.ForwardOutcome {
	if resp == nil {
		if err == nil {
			return transientOutcome("WebSocket 握手失败")
		}
		return transientOutcome(err.Error())
	}
	message := "WebSocket 握手失败"
	if err != nil {
		message = err.Error()
	}
	body := webSocketDialFailureBody(err)
	if len(body) == 0 {
		body = openAIErrorJSON("", "", message)
	}
	return failureOutcome(
		resp.StatusCode,
		body,
		resp.Header.Clone(),
		message,
		extractRetryAfterHeader(resp.Header),
	)
}

func defaultRetryAfterForKind(kind sdk.OutcomeKind, statusCode int, message string) time.Duration {
	switch kind {
	case sdk.OutcomeUpstreamTransient:
		if statusCode == 529 || isOverloadedText(message) {
			// Only propagate an explicit upstream delay. Core owns the fallback
			// window when overload responses merely say "try again later".
			return parseRetryDelay(message)
		}
	case sdk.OutcomeAccountRateLimited:
		if statusCode == http.StatusTooManyRequests {
			return time.Minute
		}
		return 10 * time.Minute
	}
	return 0
}

func isOverloadedText(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return false
	}
	for _, signal := range []string{"server_is_overloaded", "server_overloaded", "service_overloaded", "model_overloaded", "engine_overloaded", "overloaded_error"} {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return strings.Contains(lower, "overloaded")
}

// transientOutcome 连接级 / 网络层错误（无上游 HTTP 响应），归类为 UpstreamTransient。
// statusCode 给 0 或 502 均可，Core 不会基于此做判断。
func transientOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeUpstreamTransient,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusBadGateway},
		Reason:   reason,
	}
}

// streamAbortedOutcome reports a downstream/client-side stream failure without
// blaming the selected upstream account. Any usage already reported upstream is
// retained so Core can settle work that was completed before the disconnect.
func streamAbortedOutcome(err error, usage *sdk.Usage, duration time.Duration) sdk.ForwardOutcome {
	reason := "响应流已中断"
	if err != nil {
		reason = err.Error()
	}
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeStreamAborted,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusOK},
		Reason:   reason,
		Usage:    usage,
		Duration: duration,
	}
}

// accountDeadOutcome 账号级确定性失败（凭证缺失、账号配置错误等），核心会把账号打入 disabled。
func accountDeadOutcome(reason string) sdk.ForwardOutcome {
	return sdk.ForwardOutcome{
		Kind:     sdk.OutcomeAccountDead,
		Upstream: sdk.UpstreamResponse{StatusCode: http.StatusUnauthorized},
		Reason:   reason,
	}
}

// forwardErrForOutcome 只在非 ClientError 时保留 err。
// 这样 core 才会把客户端 4xx 的原始 body 直接透传回去；
// 账号级 / 上游级失败则继续走统一脱敏和 failover。
func forwardErrForOutcome(outcome sdk.ForwardOutcome, err error) error {
	if outcome.Kind == sdk.OutcomeClientError {
		return nil
	}
	return err
}

func newTokenUsage(modelID, serviceTier string, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int, firstTokenMs int64) *sdk.Usage {
	usage := &sdk.Usage{
		Model:        modelID,
		Currency:     usageCurrencyUSD,
		FirstTokenMs: firstTokenMs,
	}
	setUsageModelAttribute(usage, modelID)
	setUsageServiceTier(usage, serviceTier)
	setUsageTokens(usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens)
	return usage
}

func setUsageModelAttribute(usage *sdk.Usage, modelID string) {
	if usage == nil || modelID == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrModel,
		Label: "模型",
		Kind:  "model",
		Value: modelID,
	})
}

func setUsageReasoningEffort(usage *sdk.Usage, effort string) {
	if usage == nil || effort == "" {
		return
	}
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata["reasoning_effort"] = effort
}

func setUsageServiceTier(usage *sdk.Usage, tier string) {
	if usage == nil {
		return
	}
	tier = normalizeOpenAIServiceTier(tier)
	if tier == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrServiceTier,
		Label: "服务档位",
		Kind:  "tier",
		Value: tier,
	})
	if usage.Metadata == nil {
		usage.Metadata = map[string]string{}
	}
	usage.Metadata[usageAttrServiceTier] = tier
}

func usageServiceTier(usage *sdk.Usage) string {
	if usage == nil {
		return ""
	}
	for _, attr := range usage.Attributes {
		if attr.Key == usageAttrServiceTier {
			return normalizeOpenAIServiceTier(attr.Value)
		}
	}
	if usage.Metadata != nil {
		return normalizeOpenAIServiceTier(usage.Metadata[usageAttrServiceTier])
	}
	return ""
}

func setUsageImageSize(usage *sdk.Usage, size string) {
	if usage == nil || size == "" {
		return
	}
	setUsageAttribute(usage, sdk.UsageAttribute{
		Key:   usageAttrImageSize,
		Label: "图片尺寸",
		Kind:  "resolution",
		Value: size,
	})
}

func setUsageTokens(usage *sdk.Usage, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int) {
	if usage == nil {
		return
	}
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricInputTokens,
		Label: "输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricCachedInputTokens,
		Label: "缓存输入 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(cachedInputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricOutputTokens,
		Label: "输出 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(outputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricReasoningOutputTokens,
		Label: "推理 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(reasoningOutputTokens),
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:   usageMetricTotalTokens,
		Label: "总 Token",
		Kind:  "token",
		Unit:  "token",
		Value: float64(inputTokens + cachedInputTokens + outputTokens),
	})
}

func usageMetricInt(usage *sdk.Usage, key string) int {
	return int(usageMetricValue(usage, key))
}

func usageMetricValue(usage *sdk.Usage, key string) float64 {
	if usage == nil {
		return 0
	}
	for _, metric := range usage.Metrics {
		if metric.Key == key {
			return metric.Value
		}
	}
	return 0
}

func setUsageAttribute(usage *sdk.Usage, attr sdk.UsageAttribute) {
	for i := range usage.Attributes {
		if usage.Attributes[i].Key == attr.Key {
			usage.Attributes[i] = attr
			return
		}
	}
	usage.Attributes = append(usage.Attributes, attr)
}

func setUsageMetric(usage *sdk.Usage, metric sdk.UsageMetric) {
	for i := range usage.Metrics {
		if usage.Metrics[i].Key == metric.Key {
			usage.Metrics[i] = metric
			return
		}
	}
	usage.Metrics = append(usage.Metrics, metric)
}

func setUsageCostDetail(usage *sdk.Usage, detail sdk.UsageCostDetail) {
	if detail.AccountCost <= 0 {
		removeUsageCostDetail(usage, detail.Key)
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == detail.Key {
			usage.CostDetails[i] = detail
			recomputeUsageAccountCost(usage)
			return
		}
	}
	usage.CostDetails = append(usage.CostDetails, detail)
	recomputeUsageAccountCost(usage)
}

func removeUsageCostDetail(usage *sdk.Usage, key string) {
	if usage == nil {
		return
	}
	for i := range usage.CostDetails {
		if usage.CostDetails[i].Key == key {
			usage.CostDetails = append(usage.CostDetails[:i], usage.CostDetails[i+1:]...)
			recomputeUsageAccountCost(usage)
			return
		}
	}
}

func recomputeUsageAccountCost(usage *sdk.Usage) {
	if usage == nil {
		return
	}
	var total float64
	for _, detail := range usage.CostDetails {
		total += detail.AccountCost
	}
	usage.AccountCost = total
	if usage.Currency == "" {
		usage.Currency = usageCurrencyUSD
	}
	if total > 0 {
		usage.Summary = fmt.Sprintf("标准成本 $%.6f", total)
	}
}

type tokenPrices struct {
	input  float64
	cached float64
	output float64
}

func pricesForServiceTier(spec model.Spec, tier string) tokenPrices {
	switch normalizeOpenAIServiceTier(tier) {
	case "priority":
		return tokenPrices{
			input: fallbackPrice(spec.InputPricePriority, spec.InputPrice*2),
			cached: fallbackPrice(
				spec.CachedPricePriority,
				spec.CachedPrice*2,
			),
			output: fallbackPrice(spec.OutputPricePriority, spec.OutputPrice*2),
		}
	case "flex":
		return tokenPrices{
			input:  fallbackPrice(spec.InputPriceFlex, spec.InputPrice*0.5),
			cached: fallbackPrice(spec.CachedPriceFlex, spec.CachedPrice*0.5),
			output: fallbackPrice(spec.OutputPriceFlex, spec.OutputPrice*0.5),
		}
	default:
		return tokenPrices{
			input:  spec.InputPrice,
			cached: spec.CachedPrice,
			output: spec.OutputPrice,
		}
	}
}

func fallbackPrice(value, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func applyLongContextPricing(spec model.Spec, prices tokenPrices, inputTokens, cachedInputTokens int) (tokenPrices, bool) {
	if spec.LongContextThreshold <= 0 {
		return prices, false
	}
	if inputTokens+cachedInputTokens <= spec.LongContextThreshold {
		return prices, false
	}
	if spec.LongContextInputMultiplier > 0 {
		prices.input *= spec.LongContextInputMultiplier
	}
	if spec.LongContextCachedMultiplier > 0 {
		prices.cached *= spec.LongContextCachedMultiplier
	}
	if spec.LongContextOutputMultiplier > 0 {
		prices.output *= spec.LongContextOutputMultiplier
	}
	return prices, true
}

func tokenCost(tokens int, pricePerMillion float64) float64 {
	if tokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	return float64(tokens) * pricePerMillion / 1_000_000
}

func priceMetadata(price float64, tier string, longContext bool) map[string]string {
	metadata := map[string]string{
		"unit_price": fmt.Sprintf("%.10g", price),
		"unit":       "USD/1M tokens",
	}
	if tier != "" {
		metadata["service_tier"] = tier
	}
	if longContext {
		metadata["long_context"] = "true"
	}
	return metadata
}

// fillUsageCost 用插件自己的模型规格填充 Usage 的平台标准成本。
//
// SDK 只承载通用 Usage 结构；OpenAI 的标准价格、服务档位和长上下文阶梯都留在
// 插件内部实现，Core 入库后再按用户倍率写入 UserCost。
func fillUsageCost(usage *sdk.Usage) {
	fillUsageCostForModel(usage, usage.Model, true)
}

func fillUsageCostForModel(usage *sdk.Usage, billingModelID string, includeOutputCost bool) {
	if usage == nil {
		return
	}
	billingModelID = strings.TrimSpace(billingModelID)
	if billingModelID == "" {
		return
	}
	spec := model.Lookup(billingModelID)
	serviceTier := usageServiceTier(usage)
	inputTokens := usageMetricInt(usage, usageMetricInputTokens)
	outputTokens := usageMetricInt(usage, usageMetricOutputTokens)
	cachedInputTokens := usageMetricInt(usage, usageMetricCachedInputTokens)
	prices, longContext := applyLongContextPricing(
		spec,
		pricesForServiceTier(spec, serviceTier),
		inputTokens,
		cachedInputTokens,
	)

	inputCost := tokenCost(inputTokens, prices.input)
	cachedCost := tokenCost(cachedInputTokens, prices.cached)
	outputCost := tokenCost(outputTokens, prices.output)
	if !includeOutputCost {
		outputCost = 0
	}

	inputMetadata := priceMetadata(prices.input, serviceTier, longContext)
	cachedMetadata := priceMetadata(prices.cached, serviceTier, longContext)
	outputMetadata := priceMetadata(prices.output, serviceTier, longContext)
	if billingModelID != usage.Model {
		inputMetadata["billing_model"] = billingModelID
		cachedMetadata["billing_model"] = billingModelID
		outputMetadata["billing_model"] = billingModelID
	}

	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricInputTokens,
		Label:       "输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(inputTokens),
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    inputMetadata,
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricCachedInputTokens,
		Label:       "缓存输入 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(cachedInputTokens),
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    cachedMetadata,
	})
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricOutputTokens,
		Label:       "输出 Token",
		Kind:        "token",
		Unit:        "token",
		Value:       float64(outputTokens),
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    outputMetadata,
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostInput,
		Label:       "输入 Token",
		AccountCost: inputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    inputMetadata,
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostCachedInput,
		Label:       "缓存输入 Token",
		AccountCost: cachedCost,
		Currency:    usageCurrencyUSD,
		Metadata:    cachedMetadata,
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         usageCostOutput,
		Label:       "输出 Token",
		AccountCost: outputCost,
		Currency:    usageCurrencyUSD,
		Metadata:    outputMetadata,
	})
}

func imageTokenBillingModel(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return defaultImageTokenBillingModel
	}
	if model.IsImageOnly(modelID) {
		spec := model.Lookup(modelID)
		fallback := model.Lookup(defaultImageTokenBillingModel)
		if spec.InputPrice == fallback.InputPrice &&
			spec.CachedPrice == fallback.CachedPrice &&
			spec.OutputPrice == fallback.OutputPrice {
			return defaultImageTokenBillingModel
		}
	}
	return modelID
}

func usagePricesForBillingModel(usage *sdk.Usage, billingModelID string) (tokenPrices, bool) {
	spec := model.Lookup(billingModelID)
	serviceTier := usageServiceTier(usage)
	return applyLongContextPricing(
		spec,
		pricesForServiceTier(spec, serviceTier),
		usageMetricInt(usage, usageMetricInputTokens),
		usageMetricInt(usage, usageMetricCachedInputTokens),
	)
}

// fillUsageCostPerImageBySize 按图像 token 规则填充 Usage。
// Core 如配置 1K/2K/4K 固定价，会根据这里写入的 image_count/image_tier
// 覆盖图片输出部分；未配置时按对话模型价格（纯图像接口回退 gpt-5.5）并套倍率。
func fillUsageCostPerImageBySize(usage *sdk.Usage, numImages int, size, quality string) {
	if usage == nil {
		return
	}
	billingModelID := imageTokenBillingModel(usage.Model)
	fillUsageCostForModel(usage, billingModelID, false)
	if numImages <= 0 {
		return
	}
	outputTokens := usageMetricInt(usage, usageMetricOutputTokens)
	if outputTokens <= 0 {
		outputTokens = numImages * lookupImageGenOutputTokens(size, quality)
	}
	prices, _ := usagePricesForBillingModel(usage, billingModelID)
	cost := tokenCost(outputTokens, prices.output)
	setUsageImageSize(usage, size)
	addImageOutputCost(usage, usageCostImage, "图片生成", numImages, cost, size, quality, outputTokens, prices.output, billingModelID)
}

func addUsageCostForModel(
	usage *sdk.Usage,
	modelID, serviceTier string,
	inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens int,
	keyPrefix, labelPrefix string,
) {
	if usage == nil || modelID == "" {
		return
	}
	source := newTokenUsage(modelID, serviceTier, inputTokens, outputTokens, cachedInputTokens, reasoningOutputTokens, 0)
	fillUsageCost(source)
	for _, detail := range source.CostDetails {
		copied := detail
		if keyPrefix != "" {
			copied.Key = keyPrefix + "_" + copied.Key
		}
		if labelPrefix != "" {
			copied.Label = labelPrefix + copied.Label
		}
		if copied.Metadata != nil {
			metadata := make(map[string]string, len(copied.Metadata)+1)
			for k, v := range copied.Metadata {
				metadata[k] = v
			}
			metadata["source_model"] = modelID
			copied.Metadata = metadata
		}
		setUsageCostDetail(usage, copied)
	}
}

// fillUsageCostWithImageTool 先按主 model 定价算 token 成本，再按同一对话模型
// 价格叠加 image_generation 工具费用。
func fillUsageCostWithImageTool(usage *sdk.Usage, numImages int, size string, imageInputTokens, imageOutputTokens int) {
	if usage == nil {
		return
	}
	billingModelID := imageTokenBillingModel(usage.Model)
	fillUsageCostForModel(usage, billingModelID, true)
	if numImages <= 0 && imageInputTokens <= 0 && imageOutputTokens <= 0 {
		return
	}
	prices, longContext := usagePricesForBillingModel(usage, billingModelID)
	serviceTier := usageServiceTier(usage)
	if imageInputTokens > 0 {
		metadata := priceMetadata(prices.input, serviceTier, longContext)
		metadata["billing_model"] = billingModelID
		metadata["tokens"] = fmt.Sprintf("%d", imageInputTokens)
		setUsageCostDetail(usage, sdk.UsageCostDetail{
			Key:         "image_input_tokens",
			Label:       "图片输入 Token",
			AccountCost: tokenCost(imageInputTokens, prices.input),
			Currency:    usageCurrencyUSD,
			Metadata:    metadata,
		})
	}
	if numImages <= 0 && imageOutputTokens > 0 {
		numImages = estimateImageCountFromTokens(imageOutputTokens)
	}
	if numImages <= 0 {
		return
	}
	if imageOutputTokens <= 0 {
		imageOutputTokens = numImages * lookupImageGenOutputTokens(size, "")
	}
	cost := tokenCost(imageOutputTokens, prices.output)
	setUsageImageSize(usage, size)
	addImageOutputCost(usage, usageCostImageTool, "图片生成", numImages, cost, size, "", imageOutputTokens, prices.output, billingModelID)
}

func addImageOutputCost(usage *sdk.Usage, key, label string, numImages int, cost float64, size, quality string, outputTokens int, pricePerMillion float64, billingModelID string) {
	if usage == nil || numImages <= 0 || cost <= 0 {
		return
	}
	metadata := map[string]string{
		"unit_price":  fmt.Sprintf("%.10g", pricePerMillion),
		"unit":        "USD/1M tokens",
		"image_count": fmt.Sprintf("%d", numImages),
		"tokens":      fmt.Sprintf("%d", outputTokens),
	}
	if billingModelID != "" {
		metadata["billing_model"] = billingModelID
	}
	if size != "" {
		metadata["size"] = size
		if tier := imageTierForSize(size); tier != "" {
			metadata["image_tier"] = tier
		}
	}
	if quality != "" {
		metadata["quality"] = quality
	}
	setUsageMetric(usage, sdk.UsageMetric{
		Key:         usageMetricImages,
		Label:       "图片数量",
		Kind:        "image",
		Unit:        "image",
		Value:       float64(numImages),
		AccountCost: cost,
		Currency:    usageCurrencyUSD,
		Metadata:    metadata,
	})
	setUsageCostDetail(usage, sdk.UsageCostDetail{
		Key:         key,
		Label:       label,
		AccountCost: cost,
		Currency:    usageCurrencyUSD,
		Metadata:    metadata,
	})
}

func imageTierForSize(size string) string {
	width, height, ok := parseImageSize(size)
	if !ok {
		return "1k"
	}
	longest := width
	if height > longest {
		longest = height
	}
	switch {
	case longest <= 1536:
		return "1k"
	case longest <= 2048:
		return "2k"
	default:
		return "4k"
	}
}

const (
	yhshuGPTImage2UpstreamModel      = "gpt-image-2-124k"
	gptImage2UpstreamModelCredential = "gpt_image_2_upstream_model"
)

// imageUpstreamModelIDForAccount applies provider-specific aliases only after
// Core has selected the upstream account. An explicit credential override wins
// because OpenAI-compatible relays do not share one naming convention. yhshu.ai
// keeps its legacy automatic alias; every other provider receives the public
// model ID unless its account explicitly configures an override.
func imageUpstreamModelIDForAccount(account *sdk.Account, modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if isGPTImage2PublicModel(modelID) {
		if configured := accountCredential(account, gptImage2UpstreamModelCredential); configured != "" {
			return configured
		}
		if isYhshuAccount(account) {
			return yhshuGPTImage2UpstreamModel
		}
	}
	return modelID
}

func isGPTImage2PublicModel(modelID string) bool {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "gpt-image-2", "gpt-image-2-1k", "gpt-image-2-2k", "gpt-image-2-4k":
		return true
	default:
		return false
	}
}

func isYhshuAccount(account *sdk.Account) bool {
	host := accountBaseURLHost(account)
	return host == "yhshu.ai" || strings.HasSuffix(host, ".yhshu.ai")
}

func accountCredential(account *sdk.Account, key string) string {
	if account == nil {
		return ""
	}
	return strings.TrimSpace(account.Credentials[key])
}

func accountBaseURLHost(account *sdk.Account) string {
	if account == nil {
		return ""
	}
	raw := strings.TrimSpace(account.Credentials["base_url"])
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(u.Hostname())), ".")
}

func imagePublicModelID(responseModel, fallbackModel string) string {
	responseModel = strings.TrimSpace(responseModel)
	fallbackModel = strings.TrimSpace(fallbackModel)
	if !isGPTImage2PublicModel(fallbackModel) {
		return responseModel
	}
	switch strings.ToLower(responseModel) {
	case "gpt-image-2", "gpt-image-2-1k", "gpt-image-2-2k", "gpt-image-2-4k", yhshuGPTImage2UpstreamModel:
		return fallbackModel
	default:
		return responseModel
	}
}
