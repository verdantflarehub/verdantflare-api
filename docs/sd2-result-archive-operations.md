# SD2 私有结果归档运行手册

API 启动时只构造 S3-compatible 客户端，不执行连桶或写对象。完全不设置下列变量时归档保持禁用；只要设置了其中任意一项，缺项或非法值都会使启动失败：

```text
SD2_RESULT_ARCHIVE_S3_ENDPOINT=https://...
SD2_RESULT_ARCHIVE_S3_REGION=...
SD2_RESULT_ARCHIVE_S3_BUCKET=...
SD2_RESULT_ARCHIVE_S3_ACCESS_KEY_ID=...
SD2_RESULT_ARCHIVE_S3_SECRET_ACCESS_KEY=...
SD2_RESULT_ARCHIVE_S3_SESSION_TOKEN=...             # 可选
SD2_RESULT_ARCHIVE_S3_PREFIX=sd2-results            # 可选，默认 sd2-results
SD2_RESULT_ARCHIVE_S3_USE_PATH_STYLE=true           # 可选，默认 true
SD2_RESULT_ARCHIVE_S3_EXPECTED_BUCKET_OWNER=...     # 可选
SD2_RESULT_ARCHIVE_S3_SSE=AES256                    # 或 aws:kms
SD2_RESULT_ARCHIVE_S3_KMS_KEY_ID=...                # aws:kms 时必填；固定 key ID/ARN，禁止 alias
SD2_RESULT_ARCHIVE_IDENTITY_HMAC_KEY=...            # 必填，至少 32 bytes，所有实例长期一致
SD2_RESULT_ARCHIVE_TEMP_DIR=/encrypted-ephemeral/sd2-result-archive
SD2_RESULT_ARCHIVE_S3_PRIVATE_BUCKET_VERIFIED=true
SD2_RESULT_ARCHIVE_S3_VERSIONING_VERIFIED=true
SD2_RESULT_ARCHIVE_S3_LIFECYCLE_VERIFIED=true
SD2_RESULT_ARCHIVE_RETENTION_DAYS=30
SD2_RESULT_ARCHIVE_HOST_ALLOWLIST_JSON={"wxmaas-seedance":{"19":["result.example"]}}
```

`HOST_ALLOWLIST_JSON` 的两级 key 是精确 `provider → channel_id`，不支持通配、默认渠道或后缀匹配。凭据只来自进程环境，不写入普通日志、数据库或 opaque result ref。

对象键由 owner／公开 Task 身份和去掉签名 query 的 source identity 分别 HMAC 后确定；上传使用 `If-None-Match: *`、SSE、512 MiB 流式上限及 SHA-256 元数据复核。归档成功后，`schema_version / ref / version_id / sha256 / size / content_type / etag` 组成完整不可变 descriptor，并与用户结算、Task completed 在同一数据库事务内同时冻结到 Submission 和 Task snapshot。任一字段缺失或两份快照不一致都失败关闭。同一归档在数据库事务提交失败后会复用完全相同且元数据匹配的孤儿对象，不会再次覆盖。

桶必须保持 `Versioning=Enabled`，不得为 `Suspended`。归档要求 PUT 返回非空且不是字面值 `null` 的 VersionId；下载始终按冻结的 `VersionId + If-Match` 读取并复核 descriptor。桶还必须对配置 prefix 同时启用与 `SD2_RESULT_ARCHIVE_RETENTION_DAYS` 一致的 current-version expiration、noncurrent-version expiration 和 expired delete-marker cleanup，避免被版本引用的历史对象超出保留期。应用不在结算热路径删除对象，未被账本引用的孤儿对象由该 lifecycle 最终清理。

`SD2_RESULT_ARCHIVE_S3_PRIVATE_BUCKET_VERIFIED`、`SD2_RESULT_ARCHIVE_S3_VERSIONING_VERIFIED` 与 `SD2_RESULT_ARCHIVE_S3_LIFECYCLE_VERIFIED` 只是运维人员完成只读核验后给出的显式证明。代码只校验这三个值是否明确为 `true`，启动时不会连接对象存储，也不会自动证明桶策略、版本控制或 lifecycle 正确。每次新环境或策略变更后，存储管理员必须先完成以下不创建付费视频的核验：

1. 使用匿名身份和无权测试身份对 bucket、prefix 及已知对象执行 `LIST/HEAD/GET`，全部必须拒绝；确认无 public policy、public ACL 或公开 CDN 回源权限。
2. 只读检查 bucket versioning，确认精确状态是 `Enabled` 而非 `Suspended`，并记录配置证据与核验时间。
3. 只读检查 lifecycle 配置，确认规则精确覆盖配置 prefix，current 与 noncurrent expiration 天数均与 `SD2_RESULT_ARCHIVE_RETENTION_DAYS` 一致，并启用 expired delete-marker cleanup；记录规则 ID 与核验时间。
4. 对由存储管理员在独立受控流程中预置的一段无敏感信息小型 MP4 fixture 执行只读 `HEAD`；确认它有非 `null` VersionId，实际 `ServerSideEncryption` 为配置的 `AES256` 或 `aws:kms`，KMS 模式还需确认返回了 KMS key ID。API 启动和本核验步骤都不创建对象。
5. 使用应用运行身份按该 VersionId 和 ETag 对 fixture 执行只读 `HEAD/GET`，确认 metadata 中的 schema、size、video MIME、SHA-256、SSE 与 retention 均匹配；下载后本地复算 SHA-256。再用匿名／无权身份重复按版本 `HEAD/GET`，仍必须拒绝。
6. 记录 fixture key、VersionId、ETag、桶策略版本、versioning 状态、lifecycle 规则 ID、`HEAD/GET` 结果和核验时间。只有全部只读证据通过后才能把三个 `*_VERIFIED` 门设为 `true`；这不构成生产启用、对象写入、付费任务或发布授权。

应用运行身份只授予配置 bucket/prefix 下的 `PutObject`、`GetObject` 和 `GetObjectVersion`（含按版本 `HEAD`）权限，不授予 bucket policy、public access、ACL、lifecycle 或跨 prefix 权限；应用热路径不需要 `ListBucket`、`ListBucketVersions` 或 `DeleteObject`。KMS 模式只授予该 key 所需的 Encrypt／Decrypt／GenerateDataKey，并由 key policy 同时限制应用身份和目标存储。只读策略／versioning／lifecycle 核验使用独立运维身份，不能把其管理权限给应用。

目标 S3-compatible 服务还必须先在隔离测试 prefix 用无敏感 fixture 完成一次条件写兼容认证：相同 key 的第一次 `If-None-Match: *` PUT 成功并返回非 `null` VersionId，第二次用不同 body 必须返回 precondition failure（通常为 HTTP 412），随后按第一份 VersionId 与 ETag 执行 `HEAD/GET`，SHA-256 必须仍是第一次内容。另用独立管理身份在该 fixture key 写入一个新的 latest version，再确认按冻结 VersionId + If-Match 仍只能读到原内容。当前 SDK 明确设为仅在协议要求时计算 request／response checksum，避免可选 CRC32／`aws-chunked` 破坏兼容；仍需保存目标服务实测的请求／响应证据。这个认证不调用供应商、不开启付费视频，也不代表生产发布授权。

配置的 endpoint、bucket、prefix、SSE、KMS key 与 retention profile 在仍有有效 result ref 时视为不可原地变更；`SD2_RESULT_ARCHIVE_IDENTITY_HMAC_KEY` 必须在所有实例和重启间长期一致，否则同一重试会产生不同对象键。如需迁移或轮换，必须先实现带 profile ID 的双读、验证历史对象可读，再停止旧 profile 写入并至少保留到其最长 retention 到期；不能直接改变量让旧结果失效或失去幂等复用。

`SD2_RESULT_ARCHIVE_TEMP_DIR` 必须是专用绝对目录，放在容量至少能覆盖单任务 512 MiB 上限及并发余量的加密 ephemeral volume。服务会以 `0700` 管理目录、以 `0600` 创建成片临时文件；目录内必须存在服务生成的 ownership marker，启动和每次归档前只清理超过 2 小时且匹配 `sd2-result-*.video` 的普通文件，不递归删除、不处理其他文件。异常退出遗留文件因此会在下次启动／归档时清理；运维仍需对 volume 设置容量与节点销毁策略。

供应商结果下载固定拒绝所有 redirect。JD 与 wxmaas 各自进入候选前，必须用供应商提供的非付费 fixture／书面接口证据确认最终结果 URL 可直接 `200` 下载且不依赖 `301/302/307/308`；如供应商只能重定向，需要先设计逐跳 exact allowlist、DNS pinning 和重新校验，不能直接开启自动跟随。
