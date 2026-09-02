# airgate-openai — Claude 开发指南

> 叠加在 monorepo 根 `../CLAUDE.md` 之上。本仓是**网关插件**，完整开发流程见共享 skill **`develop-plugin`**；接口契约见 `../airgate-sdk/CLAUDE.md`。

- **插件身份**：id `gateway-openai`，type `gateway`，上游 = OpenAI / Anthropic 协议转换。
- 实现 `sdk.GatewayPlugin`：声明 models / routes / account fields，`Forward()` 把请求转发到上游并返回 `ForwardOutcome`（usage/cost 交给 core 计费）。

## 🚫 红线

- **只依赖 `airgate-sdk`**，禁止 import `airgate-core` 内部包。
- **流式超时可按账号覆盖**(`forward.go` `accountTimeoutOverride`,2026-09-02):账号凭证键
  `first_byte_timeout`(等响应头上限,默认 60s)/ `stream_idle_timeout`(读空闲上限,默认 60s),
  值为 Go duration(如 `30s`),优先于插件 config。同一插件下上游差异太大不能一刀切:Codex 中继
  偶发连响应头都 60s 不回(用户 45s 就放弃),而 inference.ai 合法首字 p50 就 25s。
- **账号 `base_url` 带路径时必须自带版本段**（`request.go` `buildAPIKeyURL`，2026-09-01 踩坑）：
  base_url 有路径 → 视为完整 API 前缀，**请求路径里的 `/v1` 会被剥掉**再拼（为兼容火山方舟
  `/api/v3` 这类前缀）。所以中继给的专属路由 `https://host/gw/xxx` 必须配成 `https://host/gw/xxx/v1`，
  配成 `https://host/gw/xxx` 会拼出 `/gw/xxx/chat/completions` → 上游 404,
  症状极像「这条路由没开通我们的模型」（假 key 401、真 key 404）。只有域名的 base_url 不受影响。
- 要用 core 能力（用量、配置等）只能经 `Host.Invoke` / `Host.InvokeStream`。
- **`plugin.yaml` 由 `make manifest` 生成，不可手改**（模型/路由/账号字段在 Go 代码里声明）。
- 前端是单 `index.js` bundle，输出到 `web/dist/index.js`，用 `@doudou-start/airgate-theme`。
- 协议转换是本仓核心职责：OpenAI ↔ Anthropic 字段映射改动要保证既有路由不回归，配套测试同包。
- **图像尺寸校验分两类，别混**：`gpt-image-2`（含 `-low`）官方支持**任意分辨率**——宽高各 16 的
  倍数、长短边比 1:3~3:1、≤3840×2160，走 `images.go` 的 `validateImageSize()` 规则式校验；
  Gemini 系上游**只收 aspect_ratio（10 个官方比例）+ image_size（1K/2K/4K）**，客户的 WxH
  是我们翻译出去的：`image_model_limits.go` 就近吸附官方比例、档位按长边推导后钳到模型
  声明档位（与 model 包 ImageUnit 牌价档位同源），**WxH 永不拒绝**；只有显式点名 1K/2K/4K
  且超出模型能力档位才 400。
  两次同型事故：2026-08-24 枚举表卡 `gpt-image-2`，把 720x1280 / 1152x864 / 864x1152 /
  864x2592 / 1952x800 / 1024x640 六个官方合法尺寸全拒成 400，直连上游逐个实测均可出图；
  2026-08-29 Gemini 枚举白名单把 9:16 竖图拒成 400，上游 chat 桥接实测正常出图（768x1344）。
  教训一致：**枚举白名单必然漏行，拒的是官方合法请求**。
  另一半根因是**正确实现存在但接错了线**——`forward.go` 才是转发链路上最先执行的尺寸闸门，
  它先 400 掉，后面 `images.go` 里那份照官方规则写的校验永远轮不到执行。
  **改尺寸相关逻辑必须跑真实链路验证，单测证明不了闸门顺序。**
  ⚠️ 前提：放宽后的规则是按 **OpenAI 官方**写的，但 `gpt-image-2` 实际上游可能不是 OpenAI
  （2026-08-24 生产上是 `api.minimax.io` 的 `canvas-20`，实测口径一致）。
  **换上游时必须重验这个口径**，否则会变成「我们放行、上游拒绝」，错误延后到上游侧暴露。
- **edits 参考图必须过归一化**（`image_normalize.go`，2026-08-25）：相机直出 JPEG 的
  EXIF 色彩元数据（bt470bg 等）会让 MiniMax 上游**概率性** 400/挂死——同一份字节
  实测 3 挂 1 成，单次成败都证明不了什么。归一化 = 解码→按 Orientation 旋转像素→
  长边 >2048 降采样→重编码无元数据 JPEG；JSON 引用图（`readImageRefBytes`）与
  客户端 multipart 直传两条路都要挂，只挂一条会在客户换 SDK 时复发。
  容器级 MPF 净化（`image_container.go`）只是解码失败时的降级兜底。
- **MiniMax 图像异步任务**（`images_async_minimax.go`）：账号凭证 `images_async=true`
  时提交带 `X-Async` 头、按 `/v1/content/images/tasks/{id}` 轮询（契约见
  `docs/minimax-canvas20-images-async-api.md`）。提交接口**没有幂等键**：轮询失败
  只能重试查询、绝不能重新提交（会创建并计费新任务）；任务终态失败必须走
  `asyncImageTaskFailedError` 按同步失败分类，不能判 transient（transient 会触发
  failover 重新提交）。

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
