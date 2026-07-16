# VerdantFlare SD2 直连 API 测试指南

本文面向需要直接调用 VerdantFlare API 测试 `verdantflare-sd2` 模型的开发者。调用方不需要安装 `verdantflare-video` Skill，也不需要访问 JD 上游接口。

## 1. 接口信息

| 项目         | 内容                                 |
| ------------ | ------------------------------------ |
| API Base URL | `https://api.verdantflarehub.com/v1` |
| 创建任务     | `POST /videos`                       |
| 查询任务     | `GET /videos/{task_id}`              |
| 模型标识     | `verdantflare-sd2`                   |
| 鉴权方式     | `Authorization: Bearer <API Key>`    |
| 任务类型     | 异步视频生成                         |

公开 API 只使用 `verdantflare-sd2`。`jd-seedance-sd2` 和上游模型名称属于 VerdantFlare 服务端内部路由，不作为调用参数。

## 2. 准备环境

测试前准备以下信息：

- 一枚已开通 `verdantflare-sd2` 权限的 VerdantFlare API Key。
- macOS/Linux 的 `curl`，或 Windows PowerShell 5.1+。
- 如果使用参考素材，准备可被服务端访问的公网 HTTPS URL。

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

API Key 只用于发送到 `api.verdantflarehub.com`，不要写入提交文件、日志或最终视频下载请求。

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

每次创建请求都生成新的 `X-Request-ID`。如果系统没有 `uuidgen`，可以使用 Python 生成 UUID。

```bash
REQUEST_ID="$(uuidgen)"

curl --fail-with-body --silent --show-error \
  "${VERDANTFLARE_API_BASE_URL}/videos" \
  -H "Authorization: Bearer ${VERDANTFLARE_API_KEY}" \
  -H "Content-Type: application/json" \
  -H "X-Request-ID: ${REQUEST_ID}" \
  --data-binary @request.json
```

Python 生成 UUID 的替代写法：

```bash
REQUEST_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
```

### Windows PowerShell

```powershell
$headers = @{
    Authorization = "Bearer $env:VERDANTFLARE_API_KEY"
    "X-Request-ID" = [guid]::NewGuid().ToString()
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
    "url": "https://cache.example.com/signed-result.mp4",
    "duration": 8,
    "ratio": "9:16",
    "generate_audio": false
  }
}
```

使用 `metadata.url` 下载视频。该 URL 可能是短期有效的签名地址，下载时不要附带 API `Authorization` 请求头。

### macOS/Linux curl

```bash
RESULT_URL="<metadata.url>"
curl --fail-with-body --silent --show-error --location \
  --output "${TASK_ID}.mp4" \
  "${RESULT_URL}"
```

### Windows PowerShell

```powershell
$resultUrl = "<metadata.url>"
Invoke-WebRequest -Uri $resultUrl -OutFile "$taskId.mp4"
```

如果结果 URL 返回 `403` 或 `410`，重新查询同一个任务一次并获取新的 `metadata.url`，不要重新创建视频任务。

## 6. 使用参考图片、视频或音频

直连 API 不能读取调用方的本地路径。参考素材必须先放到服务端可访问的 HTTPS 地址，然后作为独立的 `content` 项传入。

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
- 当前测试契约支持最多 9 张图片、1 个视频参考和 1 个音频参考。
- 使用 `messages[].content[]` 传递内容，不要同时构造 `metadata.content`。

## 7. 参数参考

| 参数             | 类型    | 要求                                      |
| ---------------- | ------- | ----------------------------------------- |
| `model`          | string  | 固定为 `verdantflare-sd2`                 |
| `messages`       | array   | 至少一个 `role=user` 消息，并包含非空文本 |
| `duration`       | integer | `1` 至 `15` 秒                            |
| `ratio`          | string  | `16:9`、`9:16`、`1:1`、`4:3` 或 `3:4`     |
| `generate_audio` | boolean | 默认建议显式传入；静音时传 `false`        |
| `watermark`      | boolean | 默认建议显式传入；不需要水印时传 `false`  |

## 8. 常见错误

| 现象                                 | 原因与处理                                                                                         |
| ------------------------------------ | -------------------------------------------------------------------------------------------------- |
| `401 Unauthorized`                   | API Key 缺失、错误或已失效；检查请求是否只发送到 API Host                                          |
| `403 Forbidden`                      | API Key 没有模型权限、账户额度或访问权限不足；联系 VerdantFlare 管理员确认 `verdantflare-sd2` 权限 |
| `missing_model`                      | 请求没有传 `model`；固定使用 `verdantflare-sd2`                                                    |
| `content is required`                | `messages` 中没有有效内容；增加非空 `text`                                                         |
| `prompt or text content is required` | 只有媒体没有文本；增加说明媒体用途的提示词                                                         |
| `invalid ratio`                      | 画幅不在允许列表中；使用 `16:9`、`9:16`、`1:1`、`4:3` 或 `3:4`                                     |
| 长时间 `queued` / `in_progress`      | 继续查询同一个任务 ID，不要重复创建                                                                |
| `completed` 没有结果 URL             | 服务端响应不完整；保留任务 ID并联系接口维护方                                                      |
| 下载 URL 返回 `403` / `410`          | 签名地址过期；重新查询同一任务并下载新地址                                                         |
