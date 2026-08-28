package gateway

import (
	"testing"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

func acctBase(base string) *sdk.Account {
	return &sdk.Account{Credentials: map[string]string{"base_url": base}}
}

// 覆盖生产在用的每一种 base_url 形态，确保放宽拼接条件不回归。
func TestBuildAPIKeyURL_RealWorldBaseURLs(t *testing.T) {
	cases := []struct{ name, base, path, want string }{
		{"域名无路径（TokenMart）", "https://model.service-inference.ai", "/v1/chat/completions",
			"https://model.service-inference.ai/v1/chat/completions"},
		{"域名带端口无路径", "http://210.16.177.241:43262", "/v1/chat/completions",
			"http://210.16.177.241:43262/v1/chat/completions"},
		{"以 /v1 结尾（腾讯 TokenHub）", "https://tokenhub.tencentmaas.com/v1", "/v1/chat/completions",
			"https://tokenhub.tencentmaas.com/v1/chat/completions"},
		{"多段路径以 /v1 结尾（阿里百炼）", "https://llm-x.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "/v1/chat/completions",
			"https://llm-x.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"路径不叫 v1（火山方舟）", "https://ark.cn-beijing.volces.com/api/v3", "/v1/chat/completions",
			"https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{"尾部斜杠被裁剪", "https://ark.cn-beijing.volces.com/api/v3/", "/v1/chat/completions",
			"https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		{"空 base_url 回落官方", "", "/v1/chat/completions",
			"https://api.openai.com/v1/chat/completions"},
		{"图像路径同样适用", "https://ark.cn-beijing.volces.com/api/v3", "/v1/images/generations",
			"https://ark.cn-beijing.volces.com/api/v3/images/generations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildAPIKeyURL(acctBase(c.base), c.path); got != c.want {
				t.Fatalf("base=%q path=%q\n  got  %s\n  want %s", c.base, c.path, got, c.want)
			}
		})
	}
}
