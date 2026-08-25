# MiniMax canvas-20 图片异步任务 API（上游提供，2026-08-21）

> 上游未公开文档，微信原件转存。实现见 `backend/internal/gateway/images_async_minimax.go`；账号凭证 `images_async=true` 启用。

图片生成和图片编辑均支持异步模式。请求路径和 body 参数与同步 Images API 保持一致，只需在提交请求中增加 X-Async 请求头。服务会先同步完成鉴权、额度/限流、参数校验和 Prompt 安全检查，校验通过后返回 task_id，实际生成在后台执行。Responses API 不使用本节的 task_id 轮询协议。
- 异步生成：POST https://api.minimax.io/v1/content/models/canvas-20/generations
- 异步编辑：POST https://api.minimax.io/v1/content/models/canvas-20/edits
- 查询任务：GET https://api.minimax.io/v1/content/images/tasks/{task_id}

10.1 请求头与使用限制
- Authorization：提交和查询都必须携带 Bearer Token。任务只能由提交任务时所属的同一 Group 查询；跨 Group 查询会按 task not found 处理。
- X-Async：取值 true 或 1 时启用异步。未设置、false 或其他值时仍走同步接口。
- X-Timeout：异步任务默认 300 秒，支持 300～900 秒；超出范围会被服务端限制到边界值。
- task_id：响应中是字符串形式的 64 位 ID。JavaScript 客户端不要转换为 Number，直接按字符串保存和拼接查询 URL。

10.2 异步图片生成

curl --location 'https://api.minimax.io/v1/content/models/canvas-20/generations' \
  --header 'Authorization: Bearer ${API_KEY}' \
  --header 'Content-Type: application/json' \
  --header 'X-Async: true' \
  --header 'X-Timeout: 500' \
  --data '{
    "model": "canvas-20",
    "prompt": "A cute cat sitting on a windowsill",
    "n": 1,
    "stream": false,
    "size": "1024x1024",
    "output_format": "jpeg",
    "quality": "low"
  }'
提交成功返回 HTTP 200。此时只表示任务已经入队，不表示图片已经生成完成：

{
  "task_id": "418218374308096",
  "status": "queued",
  "trace_id": "06a000274fca81dc0530d7504d91ce36",
  "base_resp": {
    "status_code": 0,
    "status_msg": "success"
  }
}

10.3 异步图片编辑
异步 edit 同时支持 multipart/form-data 和 application/json。multipart 可使用本地文件，也可在 image/image[]/mask 表单字段中传入 HTTP(S) URL、data URL 或 base64；JSON 可在 images[].image_url、images[].b64、mask.image_url 或 mask.b64 中传入图片。服务会在返回 task_id 前冻结输入图片快照，避免源 URL 后续变化导致任务不可复现。

multipart/form-data 示例

curl --location 'https://api.minimax.io/v1/content/models/canvas-20/edits' \
  --header 'Authorization: Bearer ${API_KEY}' \
  --header 'X-Async: true' \
  --header 'X-Timeout: 500' \
  --form 'model="canvas-20"' \
  --form 'prompt="Combine these items into a gift basket"' \
  --form 'image[]=@"./photo.png"' \
  --form 'image[]="https://example.com/style-reference.png"' \
  --form 'mask=@"./mask.png"' \
  --form 'size="1024x1024"' \
  --form 'quality="medium"' \
  --form 'stream="false"'

application/json 示例

curl --location 'https://api.minimax.io/v1/content/models/canvas-20/edits' \
  --header 'Authorization: Bearer ${API_KEY}' \
  --header 'Content-Type: application/json' \
  --header 'X-Async: true' \
  --header 'X-Timeout: 500' \
  --data '{
    "model": "canvas-20",
    "prompt": "Add decorative text to the image",
    "images": [
      {
        "type": "input_image",
        "image_url": "https://example.com/input.png"
      },
      {
        "type": "input_image",
        "b64": "<BASE64_DATA>"
      }
    ],
    "mask": {
      "type": "input_image",
      "image_url": "data:image/png;base64,<MASK_BASE64>"
    },
    "size": "1024x1024",
    "quality": "medium",
    "stream": false
  }'

mask 仍需满足同步 edit 的要求：PNG 格式、包含 alpha 通道，并与待编辑图片尺寸匹配。单张输入图片最大 50 MiB，整个 HTTP 请求体最大 128 MiB。输入图片读取、下载或快照保存失败时，提交请求会直接返回错误，不会创建 task_id。

10.4 查询任务
curl --location \
  'https://api.minimax.io/v1/content/images/tasks/418218374308096' \
  --header 'Authorization: Bearer ${API_KEY}'


queued / in_progress
任务仍在处理中时返回 HTTP 200，并携带 status。queued 表示已经入队，in_progress 表示正在调用模型或保存结果：
{
  "status": "in_progress",
  "trace_id": "06a000274fca81dc0530d7504d91ce36",
  "base_resp": {
    "status_code": 0,
    "status_msg": "success"
  }
}

completed
任务完成后直接返回与同步接口一致的响应体，不再携带 status。默认响应中的图片位于 data[].b64_json，usage 可用于成本统计：
{
  "created": 1783916400,
  "data": [
    {
      "b64_json": "<BASE64_IMAGE>",
      "revised_prompt": "..."
    }
  ],
  "usage": {
    "input_tokens": 120,
    "output_tokens": 4096,
    "total_tokens": 4216,
    "input_tokens_details": {
      "text_tokens": 120,
      "image_tokens": 0
    }
  },
  "trace_id": "06a000274fca81dc0530d7504d91ce36",
  "base_resp": {
    "status_code": 0,
    "status_msg": "success"
  }
}

failed
任务失败后直接返回与同步接口一致的 HTTP 状态码和错误体，不再携带 status。下游可安全透出的参数错误会保留原错误信息，例如：
{
  "created": 0,
  "trace_id": "06a000274fca81dc0530d7504d91ce36",
  "base_resp": {
    "status_code": 400,
    "status_msg": "Invalid mask image format - mask image missing alpha channel"
  }
}

10.5 状态、轮询与保留时间
- queued：任务已创建，等待后台执行。
- in_progress：任务正在生成、编辑或保存结果。
- completed：查询接口返回完整同步响应体，不返回 status。
- failed：查询接口返回同步错误体和对应 HTTP 状态码，不返回 status，失败任务不计费。
- 建议每 2～5 秒查询一次，并使用指数退避；不要高频毫秒级轮询。
- completed/failed 终态可查询 24 小时。超过保留时间后返回 task not found，请在 24 小时内拉取并持久化结果。
- 成功任务在结果可查询后完成计费和 Kafka 审计；查询接口本身不会重复计费。
- 当前提交接口没有客户端幂等键。查询暂时失败时应重试查询，不要立即重新提交，否则可能创建并计费多个独立任务。

10.6 客户端终态判断
不要只依赖 status 判断终态：status 只出现在 queued 和 in_progress 响应中。推荐按以下顺序判断：
1. HTTP 非 2xx，或 base_resp.status_code != 0：任务失败或查询失败，按错误处理。
2. 响应包含 status=queued/in_progress：继续轮询。
3. 响应包含 data，且 base_resp.status_code=0：任务完成，保存图片和 usage。
4. 其他响应：记录 trace_id，并按未知响应处理。

排障时请同时记录提交响应和每次查询响应中的 trace_id；查询请求会生成新的 trace_id。