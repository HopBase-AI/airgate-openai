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
- **首字前双发对冲**(`hedge.go`,2026-09-03,默认关闭):账号凭证 `hedge_after`(如 `10s`)或插件 config
  开启后,SSE token 流在该时长内无真实输出就对同一上游再发一份,谁先出字谁写客户端(`hedgeGate` 独占门闸,
  首字前两路都在缓冲态所以可二选一),另一路取消;每请求最多一次,全局在飞对冲 ≤8。输家上游可能仍计费,
  是用费用换首字确定性。生图/非流式不对冲。
- **首字看门狗按请求体与重试次序分档**(`stream.go` `firstOutputTimeoutForBody` / `firstOutputTimeoutForAttempt`,
  2026-09-02,取两者较大):请求体 <1MB 30s、1~2MB 60s、≥2MB 90s;core 经 `X-Airgate-Attempt` 传 failover
  序号,第 2 次 60s、第 3 次起 90s——换号后缓存必然未命中,合法首字更慢,同样的 30s 会把「慢而活着」连杀三次。24 万 token 的 Codex 上下文合法首字就要 20~55s,一刀切 30s 会把它们误杀成 failover 循环
  (换号后缓存未命中只会更慢),三次穷尽后 502;卡死判定对 99% 的小请求不变。
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

## 长上下文阶梯（计价）

旗舰模型对超长 prompt 换档计价。本仓是这套规则的**权威实现**，改动前先读这段。

- **语义**：`withLongCtx(s)` 给 Spec 附上 OpenAI 旗舰系阶梯——阈值 `272_000`、
  input ×2、cached ×2、output ×1.5。`grokChat` 给 xAI 系附另一组——阈值 `200_000`、
  三轴全 ×2。其余模型四个 `LongContext*` 字段保持零值 = **无阶梯**，
  `applyLongContextPricing` 直接返回短档价。
- **阈值口径**：比的是 `input_tokens + cached_input_tokens`（未缓存输入 + 缓存命中输入），
  **不含输出、不含 reasoning**；且 `<= threshold` 走短档，必须**严格大于**才进长档——
  与 OpenAI 原文 "Prompts with **more than** 272K input tokens" 逐字对应。
  ⚠️ xAI 官方措辞是 "**≥** 200k prompt tokens"，恰好 200,000 token 的请求我方按短档收，
  这是已登记的 code-vs-vendor 偏差（见根仓 `deploy/current-model-pricing.md`）。
- **整笔换档**：进了长档，该次请求全部 token（含阈值以下那部分）按长档单价结算，不分段。
  官方 "for the full request" / "for the full session"。
- **与服务档位相乘**：`pricesForServiceTier` 先取档位基准（standard / `priority` = ×2 /
  `flex` = ×0.5），`applyLongContextPricing` 再乘阶梯倍率，两者是相乘关系。
  官方现在把 ×2 那档叫 **Fast**，我方键名仍是 `priority`；`normalizeOpenAIServiceTier`
  只认 `priority` / `flex`，客户端传 `service_tier=fast` 会被剥掉并按标准档计
  （不上送上游，所以与上游实收自洽，但别名不通）。
- **覆盖层**：后台模型目录条目可用 `long_context`
  （`threshold` / `input_multiplier` / `cached_multiplier` / `output_multiplier`）显式声明。
  只写 `pricing` 的条目**不会**抹掉内置阶梯（`applyOverlay` 逐字段覆盖）；
  但 `inferNewModelBase` 对没匹配到任何内置 GPT 系列的新模型会主动清零阶梯——
  不能让 GLM 之类第三方模型从 `DefaultSpec` 继承 GPT 的 272K 倍率。

### 🚫 纪律：新旗舰上架必须核对官方阶梯

**上架任何旗舰型号时，把官方长上下文阶梯当成价格的一部分一起核，核不到就写清楚"官方无阶梯"，
不能默认留空。** 官方阶梯规则常常**不在定价表里**，而是写在模型页正文或定价页脚注
（`gpt-5.5` 的 272K 阶梯就只在模型页正文，至今没跟上）——只抄定价表必然漏。

2026-09 的教训（PR #66）：`gpt-6-astra` 2026-09-03 上架时官方定价页还没有 GPT-6 行，
registry 按"不虚构倍率"刻意把阶梯留空，**之后官方出行了却没人回头补**。
生产 12 天（09-05 ~ 09-16）16,160 笔里有 **1,162 笔超过 272K 输入**（7 个用户，
平均 prompt 53 万 token、最大 1,101,045），全部按基准价结算，
**少收 ¥518.67**（已收 ¥526.62，应收 ¥1,045.29）。
留空本身是对的——错在没给"官方出行后回来补"留下任何钩子。

所以上新旗舰时三件事一起做：

1. 逐项核官方（基准价 / 缓存价 / 阶梯阈值与三轴倍率 / 上下文窗口 / 最大输出 /
   Batch·Flex·Fast 倍率），出处与核对日期写进 registry 注释；
2. 官方当时确实没公布的，注释里写明"待官方出行后补"，并在根仓
   `deploy/current-model-pricing.md` 登记一行——那份文档是对内报价与核价的基线，
   **生产在跑的模型当天就必须有行**；
3. 阶梯边界补测：阈值 −1 / 恰好等于 / +1 三例，且缓存 token 计入阈值。

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
