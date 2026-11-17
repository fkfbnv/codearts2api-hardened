# Changelog

本文件记录本维护分支相对上游 [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api) 的改动。

## v1.0.3 — 2026-11-17

### 修复

- Windows 一键启动生成的 `config.json` 在 PowerShell 5.1 下会把 UTF-8 中文注释
  读成乱码，导致 JSON 解析失败（`invalid character '"' after object key:value pair`）。
  `make-config.ps1` 改为显式 UTF-8 读写；已用干净目录解压 v1.0.2 包复现并验证修复。

## v1.0.2 — 2026-11-17

### 上手体验

- Windows 一键启动 `start.cmd`：首次自动生成 `config.json` 与 `api_key.txt`
  （监听 `127.0.0.1:7866`），引导华为云登录，隐藏启动网关并打开面板。
- Release 增加 `codearts2api-windows.zip`：四个 exe + 一键启动脚本 + 示例配置 + 文档。
- README 增加「给 AI agent 的入口」，列出 clone/编译、配置、登录、验证与边界。

### 面板

- 最近请求 / 错误：内存环形记录最近 100 条（时间、模型、账号、来源、状态码、
  错误分类、耗时、请求体大小），新增 `GET /admin/api/recent`；面板每 5 秒刷新。
- 模型 × 账号健康矩阵：展示可用性缓存的 `usable/unusable/unknown` 与原因、时间；
  新增 `POST /admin/api/probe` 手动探活单个格子（会真实调用上游一次；不做批量、
  不新增自动探测）。
- 账号表补 token 剩余、到期时间、并发占用、冷却倒计时与 `cool_kind`。
- 运行状态卡：本地 32MB / 上游 10MiB 两个 413 计数、最大请求体积、CLI 后端
  开关/地址/模型数/多模态模型/健康状态（15 秒缓存）。
- 真实积分余额未实现：实测 `opengw` 余额端点返回 `4000 invalid channel`，
  snap 同名端点 `404 APIG.0101`，面板明确显示「未查询」，不展示假数据。

## v1.0.1 — 2026-11-17

### 直连路径

- 请求体预检：上游 `/api/v2/chat/completions` 的硬上限实测正好是 10MiB
  （10,484,871 字节 200 / 10,485,871 字节 400 `InferHub.001001005`），而该错误
  文案是误导性的「参数非法」。网关现在在 `json.Marshal` 之后、发上游之前按
  `upstream_max_body_mb`（默认 10）量一次实际字节数；超限直接回
  `413 request_too_large` 并写明上游限制与上游错误码，不轮换、不冷却账号。
  长会话 + 内联图片（每张 Read 截图约 150–200KB base64）很容易累积过线。

### CLI 后端

- 图片附件：CLI 通道把图片作为 opencode `file` 分片（`data:` URL）随消息发送，
  模型能真正看图；此前只能发一句「本通道无法传图」的占位文字。
- 图片与文本共用同一条增量边界（按所属消息在 prompt 中的偏移过滤），历史图片
  不再每轮重传；同一请求内重复图片去重。
- 单张 12MB（data URL 长度）/ 单次 8 张 / 合计 24MB 上限；超限、远程 http(s)
  图片、视频都会在 prompt 里明确写出「未发送」，不静默丢附件。
- `cli_backend.multimodal_models`：启动时把这些模型 PATCH 进 serve 的
  `/v1/config`（`multimodalModels`）。CLI 默认把图片换成 ImageId 交给
  `analyzeImage` 工具，该工具靠这个列表挑视觉模型；列表为空时实测报
  「未获取到可用的多模态模型」，图片等于白传。

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