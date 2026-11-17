# CodeArts2API

> 华为云 CodeArts Agent（盘古助手/码道）的 OpenAI 兼容代理。**无需运行 CodeArts Agent
> 客户端**，纯 Go 直连华为云 API，多账号轮转 + token 自动续期 + WebUI。

## 参考项目

本项目是 [Sliverkiss](https://github.com/Sliverkiss) 同系列开源项目的延伸实现，架构与运维形态参考了以下仓库：

- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy CN OpenAI 兼容反代（账号池 / 轮转 / 签到架构）
- [traework2api](https://github.com/Sliverkiss/traework2api) — TRAE Work OpenAI 兼容反代（零依赖 Go 骨架）
- [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) — QoderWork CN OpenAI 兼容反代（OAuth 授权流程）

感谢原作者的开源与优秀设计。

## 本分支说明

本仓库是 [HITZY2002/codearts2api](https://github.com/HITZY2002/codearts2api)（MIT）的**维护分支**，
在其基础上做了以下改动（相对上游，详见 [CHANGELOG.md](CHANGELOG.md)）：

- **官方 CLI 后端**：官方对裸 chat 调用加传输层加密后，部分模型直连返回 `TM.000010051`；
  `internal/cli` 把命中模型的请求转给本地 `codearts serve`。默认关闭，需要时按
  `cli_backend` 配置开启（`codearts_exe` 必填，或用 `CA2A_CLI_EXE`）。
  图片以 opencode `file` 分片（`data:` URL）随消息发送；CLI 需要
  `cli_backend.multimodal_models` 指定 serve 侧视觉模型，否则 CLI 的
  `analyzeImage` 会报「未获取到可用的多模态模型」。视频与远程 http(s) 图片不发送，
  prompt 里会明确说明。
- **会话心跳**：直连路径按账号发送 busy/idle 心跳与稳定会话头，缓解上游并发闸。
- **错误分类与账号池保护**：模型无权 / 参数非法 / 内容审查 / 网关超时分类处理，
  不再因为一次 504 或一次 `81001` 冷却整池；成功请求会自动清掉早先的冷却与错误记录。
- **预流错误返回真实状态码**：上游先发角色帧再拒绝时，不再退化成 `200 + SSE error`。
- **请求体上限 8MB → 32MB**，图片/视频分片原样透传，`/v1/models` 过滤上游脏条目。
- **签到**覆盖 PORTAL 通道并按活动 type 领取，模型目录缓存落盘。

上游自 2026-11-11 后无新提交，本分支独立维护；具体差异以 `git log` 为准。

## 快速开始

```bash
make linux            # bin/ 下 4 个 Linux 静态二进制
make windows          # 或 Windows：4 个 exe
make test
```

### 登录（华为云账号）

```bash
# 本机有浏览器
./login.sh

# 服务器（无浏览器）：打印链接，任意机器浏览器打开，ticket 轮询下发
./login.sh -print-only

# 凭证落盘 auths/codearts-{user_id}.json
```

### 启动

```bash
cp config.example.json config.json
export CA2A_API_KEY=你的随机密钥
./bin/codearts2api -config config.json
```

### 监听地址

`listen` 默认 `:7866`（所有网卡）。WebUI 页面本身不做鉴权，登录框填的就是 `api_key`，
所以只在本机使用时应改成 `127.0.0.1:7866`，避免同网段其他人打开面板。
Docker 或前置反向代理场景保留 `:7866`，由代理层做访问控制。

### 验证 + WebUI

```bash
curl http://127.0.0.1:7866/healthz
curl http://127.0.0.1:7866/v1/models -H "Authorization: Bearer $CA2A_API_KEY"
curl -X POST http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $CA2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"snap-chat","messages":[{"role":"user","content":"你好"}]}'
```

浏览器打开 **http://127.0.0.1:7866/** 即 WebUI：账号/token 状态、对话测试（流式/非流式）。多轮上下文按账号自动续接（chat_id 分组）；也可用请求头
`X-Codearts-Chat-Id: <chatId>` 或 body 里 `conversation_id` 显式指定会话。

面板（`/`、`/panel`、`/admin` 同一页）还包含：

- **最近请求 / 错误**：内存保留最近 100 条（模型、账号、来源、状态码、错误分类、
  耗时、请求体大小），5 秒自动刷新。
- **模型 × 账号健康**：可用性缓存的可用/不可用/未知与原因；每格带「探活」按钮，
  按需真实调用上游一次，不做批量或自动探测。
- **账号详情**：token 剩余、到期时间、并发占用、冷却倒计时、`cool_kind`。
- **运行状态**：本地 32MB / 上游 10MiB 的 413 计数、最大请求体积、CLI 后端
  （开关、地址、模型数、多模态模型、健康状态）。
- 真实积分余额不可查：上游福利余额端点已下线（`4000 invalid channel` / `404`），
  面板显示「未查询」，不展示推测值。

流式请求设置 `"stream_options":{"include_usage":true}` 后，会在 `[DONE]` 前返回一次
`choices: []` 的 usage chunk；其余 chunk 的 `usage` 为 `null`。未启用时不返回 usage。
流式和非流式均优先使用上游 usage；上游未提供时按文本长度估算，估算值不是精确 token 计数。

工具调用支持 `tool_choice` 的 `auto` / `none` / `required` 及指定函数对象。
指定函数时，网关只向上游发送该函数的定义，并转换为上游支持的字符串 `required`；
指定函数不存在或重复定义时返回 400。上游 `InferHub.001001005` 参数错误也返回 400，
不轮换、不冷却账号；若流式响应已开始，则用 SSE 错误事件结束，同样不处罚账号。

请求体另有独立上限：上游 `/api/v2/chat/completions` 实测硬上限是 **10MiB**，超过后
上游回 `400 InferHub.001001005`（文案却是「参数非法」）。网关在发上游前按
`upstream_max_body_mb`（默认 10）预检，超限直接回 `413 request_too_large` 并写明
实际上限与字节数，不再把误导性文案透传给客户端。长会话累积多张内联图片时最容易触发
（每次 `Read` 一张截图约 150–200KB base64，且不会自动出上下文）。

### 模型列表与限时福利

`/v1/models` 返回上游**精确模型 ID**（区分大小写，如 `GLM-5.2`、`Qwen3-VL-235B`），
同时为含大写的 ID 补一条小写别名（`glm-5.2`），两者都能用于聊天。限时福利
（免费套餐）模型额外带 `benefit: true` 标记，聊天时服务端会自动追加上游要求的
`maas_type: benefit` 请求头（按发起请求的账号判定，多账号套餐不同也不会串——
列表是各账号可用模型的并集，实际路由会优先挑目录里真有这个模型的账号）。

福利模型列表随免费套餐轮换，用下面这条命令核对当前账号实际可用的模型：

```bash
go run ./cmd/models                 # 账号可用模型（内置 + 福利）
go run ./cmd/models -json           # 机器可读
go run ./cmd/models -claim          # 先领取限时福利再查询（幂等，属写操作）
```

限时福利模型**必须先领取才能调用**：未领取时上游一律返回
`InferHub.4004.200 benefit not found`（实测）。领取是幂等操作，官方客户端打开模型
菜单时也会调用，因此服务默认会领取（`benefit_auto_claim: true`）。不想让服务写账号
可显式关闭：

```jsonc
"benefit_auto_claim": false   // 或 CA2A_BENEFIT_AUTO_CLAIM=0
```

关闭后福利模型仍会出现在 `/v1/models`，但调用必然失败。

### 模型可用性探测

`/v1/models` 只列**真实可用**的模型：上游的模型发现（agent-center / builtin / 福利
网关）会列出当前账号根本没注册的模型（实测 `GLM-5.2-ArkTS-SPARK`、`OpenPangu-2.0-Pro`
等返回 `InferHub.002002009.404`）。服务会周期性用一条最小请求探测并缓存结论，
真实请求失败也会被学习（`not registered` / `benefit not found` 记为账号能力问题，
不会给健康账号记错误冷却）。探测结果缓存 30 分钟。

### API Key 必填

服务没有 API Key 会**拒绝启动**（旧版本会回退到公开的 `dummy-key-for-codearts`，
等于把接口暴露给任何人）。三选一：

```bash
# 1) env（推荐，配合 .env / systemd EnvironmentFile）
export CA2A_API_KEY=$(openssl rand -hex 24)
# 2) config.json 的 "api_key" 字段
# 3) docker compose 的 .env
```

### 账号续期

`refresh_token` 与登录时的 `client_id`、DPoP 私钥绑定，因此凭证文件会保存
`client_id` 与 `dpop_private_key`；刷新时原样复用并写回轮转后的新 refresh_token。
若手工换过 `login_client_id`，老账号仍按自己记录的 client_id 刷新。

### 每日签到与新手积分

服务默认在启动和每次 watchdog 轮询时检查活动，自动领取每日签到积分及
上游返回的可领取新用户积分。新增账号会在下一次轮询处理；也可在 WebUI
点击「签到」/「全员签到」，或鉴权调用 `POST /admin/api/checkin`（可传 `{"uid":"账号 ID"}`）。
领取后必须确认；已领取未确认时只补确认，已确认不重复领取。资格、金额、
每日刷新和活动起止时间均以上游返回为准，失败会在下一次轮询重试，
不会因活动接口故障冷却或禁用推理账号。

`credits_auto_claim: false` 或 `CA2A_CREDITS_AUTO_CLAIM=0` 可关闭自动领取，
`watch.enabled: false` 会关闭整个轮询。手动签到仍可使用。
这项活动与 `benefit_auto_claim` 的模型 Token 福利独立；面板显示最近领取结果，
不把领取量或健康账号数当作积分余额。`credit.sh` 查登录态，`apply.sh` 仅续期。

当前[官方每日签到活动](https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0004.html)
为已升级积分计费的用户每日发放 1000 积分，30 天有效，活动截止于
2026-12-31 00:00（UTC+8）；[新用户注册奖励](https://support.huaweicloud.com/price-codeartsagent/codeartsagent_billing_0029.html)
为 4000 积分。无资格或已自动发放的新手奖励不会再次领取。

## 部署（systemd / Docker）

```bash
sudo mkdir -p /opt/codearts2api && sudo cp -r bin config.example.json auths /opt/codearts2api/
sudo cp deploy/codearts2api.service /etc/systemd/system/
# 编辑 /opt/codearts2api/.env 写 CA2A_API_KEY，改好 config.json
sudo systemctl daemon-reload && sudo systemctl enable --now codearts2api

# 或 Docker
export CA2A_API_KEY=你的随机密钥
mkdir -p auths data
docker compose up -d --build
```

## 环境变量配置

除了 `config.json`，还支持以下环境变量覆盖：

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `CA2A_API_KEY` | API 访问密钥 | - |
| `CA2A_LISTEN` | 监听地址 | `:7866` |
| `CA2A_AUTH_DIR` | 凭证目录 | `./auths` |
| `CA2A_STATE_FILE` | 状态文件 | `./data/state.json` |
| `CA2A_DEFAULT_MODEL` | 默认模型 | `glm-5.2` |
| `CA2A_OAUTH_CALLBACK_HOST` | OAuth 回调主机 | - |
| `CA2A_WATCH_ENABLED` | 调度器开关 | `true` |
| `CA2A_WATCH_POLL_MINUTES` | 轮询间隔（分钟） | `30` |
| `CA2A_WATCH_REFRESH_SKEW` | 提前刷新时间（分钟） | `30` |
| `CA2A_WATCH_KEEPALIVE_INTERVAL` | 保活间隔（分钟） | `15` |
| `CA2A_MAX_CONCURRENT` | 单账号最大并发 | `5` |
| `CA2A_KEEPALIVE_WINDOW` | 保活窗口 | `10m` |
| `CA2A_BENEFIT_AUTO_CLAIM` | 发现模型时自动领取限时福利（幂等；关闭则福利模型不可用） | `true` |
| `CA2A_CREDITS_AUTO_CLAIM` | watchdog 自动领取每日签到及新用户积分 | `true` |
| `CA2A_LOGIN_CLIENT_ID` | WebUI 登录使用的 OAuth client_id | 已有账号的取值，否则 `codearts-agent` |
| `CA2A_MAX_BODY_MB` | 请求体上限（MB） | `32` |
| `CA2A_CLI_EXE` | 官方 `codearts` CLI 可执行文件路径 | 空（CLI 后端无法启动） |

## 目录结构

```
cmd/server/        HTTP 服务（config + main）
cmd/login/         华为云 OAuth2 PKCE 登录
cmd/credit/        账号登录态日报（新增并发信息）
cmd/apply/         批量 token 续期（使用 pool 包）
cmd/models/        查看账号可用模型（内置 + 限时福利，可选 -claim 领取）
internal/auth/     auth 文件读写
internal/cli/      官方 CLI 后端（codearts serve / opencode API）
internal/upstream/ 云端客户端（登录/聊天/SSE/模型发现）+ 逆向常量
internal/pool/     账号池（token 校验/自动刷新/冷却/并发控制）
internal/scheduler/ token 续期看门狗（新增保活机制）
internal/server/   OpenAI 兼容路由
internal/webui/    内嵌 WebUI 控制台
deploy/            systemd unit 样例
docs/              逆向过程与接口清单
```

## 使用边界与免责声明

本项目初衷是方便个人管理本人自愿授权的账号，不是为批量运营或商业牟利设计。

允许的用途：

- 管理你本人或亲属明确授权给你的华为云账号；
- 在自己的设备或内网中学习、研究和自用。

禁止的用途：

- 批量注册、代注册、接码，以及买卖、出租或交易账号；
- 盗号、撞库、绕过实名认证、风控或其他平台安全机制；
- 将本项目或其派生服务用于对外收费、转售、引流或其他盈利性用途；
- 任何违反华为云服务条款、当地法律法规或侵犯他人权益的用途。

使用者应自行确认账号授权与使用方式合法合规。因滥用、误用或违反上述边界导致的
封号、数据丢失、服务中断或法律责任，由使用者自行承担；项目作者与贡献者不对
使用者的行为负责，也不提供任何形式的担保。

本项目与华为云、CodeArts 官方无隶属或合作关系；相关商标归其权利人所有。
项目按 MIT 许可「按现状」提供，上游服务变更可能导致本项目随时失效。

## License

MIT，见 [LICENSE](LICENSE)。原始版权归 HITZY2002；本分支改动版权归各提交作者。

