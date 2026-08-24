# airgate-openai — Claude 开发指南

> 叠加在 monorepo 根 `../CLAUDE.md` 之上。本仓是**网关插件**，完整开发流程见共享 skill **`develop-plugin`**；接口契约见 `../airgate-sdk/CLAUDE.md`。

- **插件身份**：id `gateway-openai`，type `gateway`，上游 = OpenAI / Anthropic 协议转换。
- 实现 `sdk.GatewayPlugin`：声明 models / routes / account fields，`Forward()` 把请求转发到上游并返回 `ForwardOutcome`（usage/cost 交给 core 计费）。

## 🚫 红线

- **只依赖 `airgate-sdk`**，禁止 import `airgate-core` 内部包。
- 要用 core 能力（用量、配置等）只能经 `Host.Invoke` / `Host.InvokeStream`。
- **`plugin.yaml` 由 `make manifest` 生成，不可手改**（模型/路由/账号字段在 Go 代码里声明）。
- 前端是单 `index.js` bundle，输出到 `web/dist/index.js`，用 `@doudou-start/airgate-theme`。
- 协议转换是本仓核心职责：OpenAI ↔ Anthropic 字段映射改动要保证既有路由不回归，配套测试同包。
- **图像尺寸校验分两类，别混**：`gpt-image-2`（含 `-low`）官方支持**任意分辨率**——宽高各 16 的
  倍数、长短边比 1:3~3:1、≤3840×2160，走 `images.go` 的 `validateImageSize()` 规则式校验；
  Gemini 系是**真的只支持固定档位**，留在 `image_model_limits.go` 的枚举白名单里。
  2026-08-24 事故：曾用枚举表卡 `gpt-image-2`，把 720x1280 / 1152x864 / 864x1152 / 864x2592 /
  1952x800 / 1024x640 六个官方合法尺寸全拒成 400，而直连上游逐个实测均可出图。
  根因是**正确实现存在但接错了线**——`forward.go` 才是转发链路上最先执行的尺寸闸门，
  它先 400 掉，后面 `images.go` 里那份照官方规则写的校验永远轮不到执行。
  **改尺寸相关逻辑必须跑真实链路验证，单测证明不了闸门顺序。**
  ⚠️ 前提：放宽后的规则是按 **OpenAI 官方**写的，但 `gpt-image-2` 实际上游可能不是 OpenAI
  （2026-08-24 生产上是 `api.minimax.io` 的 `canvas-20`，实测口径一致）。
  **换上游时必须重验这个口径**，否则会变成「我们放行、上游拒绝」，错误延后到上游侧暴露。

## 混合现状（过渡态）

本仓当前混合了网关 + provider + UI 三层职责（目标应拆为独立组件）：

- **Provider 职责**（应归 provider 插件）：ChatGPT OAuth（`oauth.go`/`oauth_handler.go`/`session_state.go`）、WebSocket 上游（`ws.go`/`ws_handler.go`）、Web 反向图像（`images_web_reverse.go`）
- **图像任务执行**（应归 Core task engine + provider）：`task_image.go`/`task_runner.go`/`task_registry.go`/`task_input_resolver.go`
- **UI 职责**（应归 UI 插件）：6 个账号 widget（Identity/Create/Edit/UsageWindow/MetricDetail/CostDetail）

> 新增/改动须按职责归位，勿加深混合。详见 `../airgate-core/docs/architecture/current/plugins.md`。

## 命令

```bash
make dev       # devserver 独立调试（不依赖 core）
make build     # 前端 → embed → Go 二进制
make manifest  # 重新生成 plugin.yaml
make ci        # lint + test + vet + build
make release   # 交叉编译 linux-amd64，供上传
```
