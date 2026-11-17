# Changelog

本文件记录本维护分支相对上游 [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api) 的改动。

## 未发布

### 直连路径

- 请求体预检：上游 `/api/v2/chat/completions` 的硬上限实测正好是 10MiB
  （10,484,871 字节 200 / 10,485,871 字节 400 `InferHub.001001005`），而该错误
  文案是误导性的「参数非法」。网关现在在 `json.Marshal` 之后、发上游之前按
  `upstream_max_body_mb`（默认 10）量一次实际字节数；超限直接回
  `413 request_too_large` 并写明上游限制与上游错误码，不轮换、不冷却账号。
  长会话 + 内联图片（每张 Read 截图约 150–200KB base64）很容易累积过线。

## v1.0.0 — 2026-11-17

首个整理发布版。相对上游 `ee754d8`（2026-11-11）：

### 上游同步

- 合并上游 PR #11：模型 ID 大小写自愈，模型级无权错误先换账号再 404。

### 官方 CLI 后端

- 新增 `internal/cli`：把命中模型的请求转给本地 `codearts serve`（opencode API），
  覆盖直连因传输层加密返回 `TM.000010051` 的模型。
- CLI 模型支持文本协议 `tool_calls`；会话复用时按增量历史发送，避免工具提示词回显。
- 默认关闭；`cli_backend.codearts_exe` 或 `CA2A_CLI_EXE` 未配置时给出明确错误。

### 直连路径

- 会话心跳：按账号发送 busy/idle，使用稳定 `User-Session-Id`，上游不存在心跳端点时静默降级。
- 便宜模型（DeepSeek / GLM-5.3 / Kimi）走直连，不再默认套 CLI 的 agent prompt。

### 错误分类与账号池保护

- 九类错误分类（模型不可用 / 鉴权 / 参数非法 / 内容审查 / 并发 / 限流 / 服务端 / 网关超时 / 未知）。
- `504 APIG.0203 Backend timeout` 视为瞬时错误：不冷却账号、不下架模型，回 `504 upstream_timeout`。
- `ModelArts.81001`（始终思考的模型拒绝关闭思考）归为参数非法，不冷却账号。
- 401 改为软冷却，只有 refresh token 终态才禁用账号。
- 成功请求 `NoteSuccess` 清空 `errCount` / `lastErr` / `coolUntil` 并落盘。
- 非流式聚合路径的内容审查错误映射为 `400 content_filter`。

### 流式

- 预流业务错误返回真实 HTTP 状态码：缓存只带 `role` 的首帧，首段正文前出现错误时
  由 `PreStreamError` 交给 handler，不再退化成 `200 + SSE error`。
- SSE 首帧错误归一化；`[DONE]` 前补发被缓存的 role 帧。

### 请求与模型目录

- 请求体上限 8MB → 32MB（可配 `max_body_mb` / `CA2A_MAX_BODY_MB`）。
- 图片、视频分片原样透传给上游，不再拍平成纯文本。
- `max_tokens` / `max_completion_tokens` 归一，封顶 65536。
- `reasoning_effort` 归一：`none`/`off`/`disabled`/未知 → `low`，`medium` → `high`。
- `/v1/models` 过滤上游脏条目（`speed`、非 ASCII 乱码）。
- 模型目录缓存落盘（`models-cache.json`），启动即读，过期后台刷新。

### 签到与积分

- 签到覆盖 `PORTAL` 通道（此前写死 `IDE`，每日签到领不到）。
- 按活动 `type` 领取（`USER_LOGIN` / `NEW_USER_REGISTER`），不再依赖会轮换的 campaignId。
- 福利（opengw benefit）通道上游下线后退役自动领取，配置键保留兼容。

### 运维

- 未设置 `api_key` 时拒绝启动，不再回退到公开的 dummy key。
- CI：`go test` / `-race` / `vet` / `build`，覆盖 `fix/2026-11-hardening` 分支；
  tag 触发 Windows 四件套构建并附到 Release。