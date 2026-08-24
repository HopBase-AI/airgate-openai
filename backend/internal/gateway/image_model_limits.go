package gateway

import (
	"fmt"
	"strings"
)

// imageModelSupportedSizes 只登记**真的只支持固定档位**的模型（Gemini 系）。
//
// ⚠️ gpt-image-2 不在这里：官方口径是**任意分辨率**——宽高各能被 16 整除、
// 长短边比在 1:3~3:1 之间、不超过 3840×2160。用枚举白名单卡它会把大量合法
// 尺寸拒成 400。2026-08-24 实测：720x1280 / 1152x864 / 864x1152 / 864x2592 /
// 1952x800 / 1024x640 六个官方合法尺寸全被我们拒了，而直连上游逐个实测均
// HTTP 200 且出图尺寸与请求分毫不差。gpt-image-2 改走 validateImageSize()
// 的规则式校验（images.go），那份实现本就是照官方规则写的。
var imageModelSupportedSizes = map[string]map[string]struct{}{
	"gemini-2.5-flash-image": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
	},
	"gemini-3-pro-image": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
		"3840x2160": {}, "2160x3840": {},
	},
	"gemini-3-pro-image-c": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
		"3840x2160": {}, "2160x3840": {},
	},
	"gemini-3-pro-image-preview": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
		"3840x2160": {}, "2160x3840": {},
	},
	"gemini-3-pro-image-preview-c": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
		"3840x2160": {}, "2160x3840": {},
	},
	"gemini-3.1-flash-image": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
	},
	"gemini-3.1-flash-image-c": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
	},
	"gemini-3.1-flash-image-preview": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
	},
	"gemini-3.1-flash-image-preview-c": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
		"2048x2048": {}, "2048x1152": {}, "1152x2048": {},
	},
	"gemini-3.1-flash-lite-image": {
		"1024x1024": {}, "1536x1024": {}, "1024x1536": {},
	},
}

func validateImageModelSize(model, size string) error {
	model = strings.ToLower(strings.TrimSpace(model))
	size = strings.ToLower(strings.TrimSpace(size))
	if model == "" || size == "" {
		return nil
	}
	// gpt-image-2 支持任意合法分辨率，按官方规则校验而不是查表。
	// 这里是转发链路上**最先**执行的尺寸闸门（forward.go），先前查表把
	// 合法尺寸拒掉后，后面 images.go 里那份正确的规则式校验根本轮不到执行。
	if isGPTImage2Model(model) {
		return validateImageSize(size, model)
	}
	allowed, ok := imageModelSupportedSizes[model]
	if !ok {
		return nil
	}
	if _, ok := allowed[size]; ok {
		return nil
	}
	return fmt.Errorf("模型 %s 不支持尺寸 %s", model, size)
}
