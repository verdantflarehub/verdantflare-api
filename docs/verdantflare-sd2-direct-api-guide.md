# VerdantFlare SD2 直连 API 测试指南

本文面向需要直接调用 VerdantFlare API 测试 `verdantflare-sd2` 模型的开发者。调用方不需要安装 `verdantflare-video` Skill，也不需要访问 JD 上游接口。

> 新的 Submission 幂等契约随 `SD2_SUBMISSION_LEDGER_ENABLED` 受控启用；本文不代表生产环境已经发布，也不授权付费测试。

## 1. 接口信息

| 项目         | 内容                                 |
| ------------ | ------------------------------------ |
| API Base URL | `https://api.verdantflarehub.com/v1` |
| 创建任务     | `POST /videos`                       |
| 查询任务     | `GET /videos/{task_id}`              |
| 恢复提交     | `GET /video-submissions/{request_id}` |
| 模型标识     | `verdantflare-sd2`                   |
| 鉴权方式     | `Authorization: Bearer <API Key>`    |
| 任务类型     | 异步视频生成                         |

公开 API 只使用 `verdantflare-sd2`。JD、wxmaas 和上游模型名称属于 VerdantFlare 服务端内部路由，不作为调用参数。

## 2. 准备环境

测试前准备以下信息：

- 一枚已开通 `verdantflare-sd2` 权限的 VerdantFlare API Key。
- macOS/Linux 的 `curl`，或 Windows PowerShell 5.1+。
- 如果使用参考素材，准备可被服务端访问、解析到公网地址且使用 443 端口的 HTTPS URL。生产素材应来自私有输入桶的只读签名 URL，并遵守第 6 节的 7 天 hard TTL。

### macOS/Linux

```bash
export VERDANTFLARE_API_BASE_URL="https://api.verdantflarehub.com/v1"
export VERDANTFLARE_API_KEY="<your-api-key>"
```

### Windows PowerShell

```powershell
$env:VERDANTFLARE_API_BASE_URL = "https://api.verdantflarehub.com/v1"
$env:VERDANTFLARE_API_KEY = "<your-api-key>"
```

API Key 只用于发送到 `api.verdantflarehub.com`，不要写入提交文件或日志。青焰同源结果代理需要该鉴权；任何跨域 URL 或重定向都按协议／配置异常停止，不能携带 `Authorization` 继续请求。

## 3. 创建文本生视频任务

创建请求必须包含非空文本。建议先保存为 `request.json`：

```json
{
  "model": "verdantflare-sd2",
  "messages": [
    {
      "role": "user",
      "content": [
        {
          "type": "text",
          "text": "一支 8 秒的智能手表广告，夜跑者穿过雨后霓虹街区，突出运动数据和心率监测，商业广告质感，镜头稳定。"
        }
      ]
    }
  ],
  "duration": 8,
  "ratio": "9:16",
  "generate_audio": false,
  "watermark": false
}
```

### macOS/Linux curl

每个新的生成意图先生成并持久化一个 UUID，同时作为 `Idempotency-Key` 与兼容 Header `X-Request-ID`。POST 结果不明时复用这个 UUID 查询 Submission，不能生成新 UUID 重投。如果系统没有 `uuidgen`，可以使用 Python 生成 UUID。

同一 UUID 只能绑定一个逐字节固定的首次请求体；并发调用必须共用这一份 payload，恢复则只执行 GET、绝不再 POST。媒体 URL 的签名 query 也参与请求身份摘要，刷新 query、替换素材 URL 或修改任一参数后继续使用原 UUID，会返回 `idempotency_conflict`。

```bash
REQUEST_ID="$(uuidgen)"

curl --fail-with-body --silent --show-error \
  "${VERDANTFLARE_API_BASE_URL}/videos" \
  -H "Authorization: Bearer ${VERDANTFLARE_API_KEY}" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ${REQUEST_ID}" \
  -H "X-Request-ID: ${REQUEST_ID}" \
  --data-binary @request.json
```

Python 生成 UUID 的替代写法：

```bash
REQUEST_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
```

### Windows PowerShell

```powershell
$requestId = [guid]::NewGuid().ToString()
$headers = @{
    Authorization = "Bearer $env:VERDANTFLARE_API_KEY"
    "Idempotency-Key" = $requestId
    "X-Request-ID" = $requestId
}

$response = Invoke-RestMethod `
    -Method Post `
    -Uri "$env:VERDANTFLARE_API_BASE_URL/videos" `
    -Headers $headers `
    -ContentType "application/json" `
    -Body (Get-Content .\request.json -Raw)

$response | ConvertTo-Json -Depth 10
```

创建成功后保存响应中的 `id`。该值就是后续查询使用的 VerdantFlare 公共任务 ID。

典型响应：

```json
{
  "id": "task_xxx",
  "task_id": "task_xxx",
  "object": "video",
  "model": "verdantflare-sd2",
  "status": "queued",
  "progress": 0
}
```

如果 POST 超时、断线、返回 `408`、`429`、临时 `5xx`、无法解析的响应、`submission_in_progress` 或 `submission_unknown`，结果都不能安全判定，绝不自动再次 POST。查询原 UUID：

```bash
curl --fail-with-body --silent --show-error \
  "${VERDANTFLARE_API_BASE_URL}/video-submissions/${REQUEST_ID}" \
  -H "Authorization: Bearer ${VERDANTFLARE_API_KEY}"
```

`CONFIRMED` 时读取 `public_task_id`；`PREPARED`／`SENDING` 继续查询；`REJECTED` 报告安全错误；`UNKNOWN` 停止自动执行并交由 API Center 对账。后两种状态都不能靠重投“探测”。

## 4. 查询任务状态

将 `task_xxx` 替换为创建响应中的 `id`。

### macOS/Linux curl

```bash
export TASK_ID="task_xxx"

curl --fail-with-body --silent --show-error \
  "${VERDANTFLARE_API_BASE_URL}/videos/${TASK_ID}" \
  -H "Authorization: Bearer ${VERDANTFLARE_API_KEY}"
```

### Windows PowerShell

```powershell
$taskId = "task_xxx"
$headers = @{ Authorization = "Bearer $env:VERDANTFLARE_API_KEY" }

Invoke-RestMethod `
    -Method Get `
    -Uri "$env:VERDANTFLARE_API_BASE_URL/videos/$taskId" `
    -Headers $headers |
    ConvertTo-Json -Depth 10
```

状态处理规则：

| 状态                 | 含义       | 处理方式                                               |
| -------------------- | ---------- | ------------------------------------------------------ |
| `queued`             | 任务排队中 | 继续查询同一个任务                                     |
| `in_progress`        | 正在生成   | 继续查询，可读取 `progress`                            |
| `completed`          | 已完成     | 必须读取非空的 `metadata.url`                          |
| `failed` / `failure` | 终态失败   | 停止查询，记录 `error.code` 和脱敏后的 `error.message` |

`completed` 但没有 `metadata.url` 时，应视为接口协议异常，不能当作成功。

## 5. 下载生成结果

完成响应示例：

```json
{
  "id": "task_xxx",
  "status": "completed",
  "progress": 100,
  "metadata": {
    "url": "https://api.verdantflarehub.com/v1/videos/task_xxx/content",
    "duration": 8,
    "ratio": "9:16",
    "generate_audio": false
  }
}
```

使用 `metadata.url` 下载视频。Ledger 管理的任务只会在成片已经进入青焰私有结果存储后公开 `completed`；该 URL 是稳定的青焰同源 `/v1/videos/{task_id}/content` 归档代理，不是供应商签名 URL，也不应发生重定向。只有精确匹配 API origin 与该路径时才发送 API `Authorization`。

### macOS/Linux curl

```bash
RESULT_URL="<metadata.url>"
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer ${VERDANTFLARE_API_KEY}" \
  --output "${TASK_ID}.mp4" \
  "${RESULT_URL}"
```

### Windows PowerShell

```powershell
$resultUrl = "<metadata.url>"
Invoke-WebRequest -Uri $resultUrl -Headers $headers -MaximumRedirection 0 -OutFile "$taskId.mp4"
```

结果入口返回 `401/403` 表示鉴权或访问策略失败，`409 result_not_ready` 表示私有归档尚未处于可交付状态，`502` 表示归档存储不可用或完整性校验失败。保留原 Task ID 并报告维护方；重新查询不会刷新成另一条供应商签名 URL，更不能重新创建视频。任何重定向或非同源 URL 都是协议／配置异常，应立即停止，不能使用 `--location-trusted`。

## 6. 使用参考图片、视频或音频

直连 API 不能读取调用方的本地路径。参考素材必须先放到服务端可访问的 HTTPS 地址，然后作为独立的 `content` 项传入。直连调用方负责输入对象的所有权与生命周期：生产环境使用私有输入桶的只读签名 URL，确保 URL 有效期覆盖排队、生成和恢复窗口，在任务终态后尽快删除对象，并以存储层 lifecycle 保证对象最晚 7 天删除。Submission 为 `UNKNOWN` 也不能延长该 hard TTL。

```json
{
  "model": "verdantflare-sd2",
  "messages": [
    {
      "role": "user",
      "content": [
        {
          "type": "text",
          "text": "以图片1保持产品外观，以视频1参考镜头节奏，以音频1作为背景音乐，生成一支竖屏产品广告。"
        },
        {
          "type": "image_url",
          "image_url": {
            "url": "https://assets.example.com/product.png"
          }
        },
        {
          "type": "video_url",
          "video_url": {
            "url": "https://assets.example.com/reference.mp4"
          }
        },
        {
          "type": "audio_url",
          "audio_url": {
            "url": "https://assets.example.com/music.mp3"
          }
        }
      ]
    }
  ],
  "duration": 10,
  "ratio": "9:16",
  "generate_audio": false,
  "watermark": false
}
```

约束：

- `messages[0].content[0]` 必须是非空 `text`。
- 图片、视频和音频分别使用 `image_url`、`video_url` 和 `audio_url`。
- URL 必须是公网 HTTPS 地址，不能使用本机路径、`localhost` 或需要登录态的地址。
- API 只验证 HTTPS、端口与公网 DNS/IP 安全性，不会替调用方创建或证明输入桶 lifecycle；直连方必须在提交前完成只读核验。
- 同一 Client Request UUID 下必须保留完全相同的 URL（包括签名 query）；签名刷新应作为新的请求 payload，不得复用旧 UUID。
- 当前公共契约支持最多 9 张图片、3 个视频参考和 1 个音频参考。
- 使用 `messages[].content[]` 传递内容，不要同时构造 `metadata.content`。

## 7. 参数参考

| 参数             | 类型    | 要求                                      |
| ---------------- | ------- | ----------------------------------------- |
| `model`          | string  | 固定为 `verdantflare-sd2`                 |
| `messages`       | array   | 至少一个 `role=user` 消息，并包含非空文本 |
| `duration`       | integer | 可省略，默认 `10`；显式值为 `1` 至 `15` 秒 |
| `ratio`          | string  | `16:9`、`9:16`、`1:1`、`4:3` 或 `3:4`     |
| `generate_audio` | boolean | 默认建议显式传入；静音时传 `false`        |
| `watermark`      | boolean | 默认建议显式传入；不需要水印时传 `false`  |
| `resolution`     | string  | 第一阶段固定 `720p`；可省略，不能请求其他值 |

## 8. 常见错误

| 现象                                      | 原因与处理                                                                                         |
| ----------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `401 Unauthorized`                        | API Key 缺失、错误或已失效；检查请求是否只发送到 API Host                                          |
| `403 Forbidden`                           | API Key 没有模型、用户或访问策略权限；联系 VerdantFlare 管理员确认 `verdantflare-sd2` 权限         |
| `missing_idempotency_key`                 | 未提供 UUID；持久化 UUID 后同时作为 `Idempotency-Key` 和 `X-Request-ID` 发送                       |
| `invalid_idempotency_key`                 | 幂等 Header 不是 UUID；修正后使用同一个已持久化值                                                   |
| `conflicting_idempotency_keys`            | 两个幂等 Header 不相等；改为完全相同的 UUID                                                        |
| `idempotency_conflict`                    | 同一 UUID 对应的请求语义已变化；停止，不要覆盖或再次 POST                                          |
| `submission_in_progress`                  | 同一创建意图仍在处理；查询原 Submission，不能再次 POST                                             |
| `submission_unknown`                      | 供应商创建结果无法安全判定；停止自动执行并交由 API Center 对账                                      |
| `invalid_model`                           | 模型不是 `verdantflare-sd2`；固定使用公开模型名                                                    |
| `invalid_messages` / `invalid_content`    | `messages` 或 `content` 结构非法；按第 6 节的有序数组格式构造                                      |
| `missing_prompt`                          | 第一项不是非空文本；把说明素材用途的 `text` 放在首项                                               |
| `invalid_duration` / `conflicting_duration` | 时长越界、类型错误，或 `duration` 与兼容别名 `seconds` 冲突；只传一个 `1–15` 的整数              |
| `invalid_ratio`                           | 画幅不在允许列表中；使用 `16:9`、`9:16`、`1:1`、`4:3` 或 `3:4`                                     |
| `unsupported_resolution`                  | 第一阶段只支持 `720p`；省略该字段或传 `720p`                                                       |
| `media_limit_exceeded`                    | 图片、视频或音频超出 `9 / 3 / 1` 上限                                                             |
| `invalid_media_url`                       | URL 不是安全的公网 HTTPS 地址；检查 scheme、443 端口和 DNS/IP                                     |
| `conflicting_content`                     | 同时提供了 `messages` 和 `metadata.content`；只保留 `messages`                                     |
| 长时间 `queued` / `in_progress`           | 继续查询同一个任务 ID，不要重复创建                                                                |
| `completed` 没有结果 URL                  | 服务端响应不完整；保留任务 ID 并联系接口维护方                                                     |
| 下载返回 `409 result_not_ready` / `502`   | 私有归档未就绪、不可用或完整性失败；保留 Task ID 并报告维护方，不重新创建                          |
