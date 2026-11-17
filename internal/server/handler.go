// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status、WebUI。
package server

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/cli"
	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string
	MaxRotate    int
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	// CodeArts 上游把并发会话/TPM 排队既可能返回 HTTP 400/429，
	// 也可能嵌在 HTTP 200 SSE 内。默认每 10s 重试一次、最多 30 次（约 5 分钟）。
	QueueRetryDelay  time.Duration
	QueueMaxAttempts int
	// MaxBodyBytes 单个 chat 请求体上限。0 表示用 DefaultMaxBodyBytes。
	// 上游长会话 + 图片 base64 很容易顶破 8MB（1107 事故），32MB 是实测可用值；
	// 但不同部署环境（前置 nginx / 网关）承受力不同，所以做成配置。
	MaxBodyBytes int64
	// SessionHeartbeat 直连路径启用并发会话心跳（busy/idle，见 upstream/session.go）。
	// 上游每账号 3 路并发、只认心跳不认请求结束；测试/无上游环境关闭。
	SessionHeartbeat bool
	// DefaultModel 未指定模型时的默认值。
	DefaultModel  string
	ConvStateFile string
	WatchInfo     map[string]any
	AuthDir       string
	Listen        string
	// OAuthClient 短超时客户端，用于 WebUI 登录换取 token（可注入测试端点）。
	OAuthClient *upstream.Client
	// LoginConfig WebUI 登录配置（client_id 以官方客户端为准）。
	LoginConfig upstream.LoginConfig
	// OAuthCallbackHost 可选：覆盖授权链接回调 host（如 https://oneapi.example.com/codearts）。
	OAuthCallbackHost string

	// CLI 官方客户端后端（codearts serve / opencode API）。
	//
	// 背景：官方对 snap-access 裸 chat 调用加了传输层加密，直调已用模型一律
	// TM.000010051；CLI 内部走加密通道可用全部模型。因此命中 CLIModels 的模型
	// 转给 CLI，其余仍走原直连路径。
	CLI         *cli.Client
	CLIProvider string            // 例如 huaweicloud-maas
	CLIAgent    string            // 例如 build
	CLIModels   map[string]string // 小写 OpenAI 模型 ID -> CLI modelID
	// CLIEnabled 配置里是否开启 CLI 后端（区分「没开」与「开了但没起来」）。
	CLIEnabled bool
	// CLIEndpoint 配置里的 serve 地址（面板展示用）。
	CLIEndpoint string
	// CLIMultimodal serve 侧配置的多模态模型列表（面板展示用）。
	CLIMultimodal []string
}

var dynamicModelsCache struct {
	sync.RWMutex
	ids        []upstream.ModelInfo
	fetched    time.Time
	lastFail   time.Time
	refreshing bool
}

// staticModel 静态兜底模型：动态发现失败时 /v1/models 仍能列出实测可用模型。
type staticModel struct {
	ID            string
	ContextWindow int64
	Benefit       bool
}

// staticModels 静态兜底表：**只放实测确认可用的模型**。
//
// 上游的模型发现（agent-center / builtin）会列出当前账号未注册的模型，静态表
// 若照抄发现结果，就等于把 404 写进了兜底路径。以下清单来自 2026-09-15 真实账号
// 逐个调用验证（见 docs/reverse-engineering.md §7）：
//
//	✅ GLM-5.2、glm-5.2-sft-harmony、Qwen3-VL-235B
//	❌ GLM-5.2-ArkTS-SPARK、OpenPangu-2.0-Pro、OpenPangu-2.0-Flash（002002009.404）
var staticModels = []staticModel{
	{ID: "GLM-5.2", ContextWindow: 202752},
	{ID: "glm-5.2-sft-harmony", ContextWindow: 131072},
	{ID: "Qwen3-VL-235B", ContextWindow: 131072},
	// 限时福利（需领取；领取后实测可用，聊天自动带 maas_type: benefit）
	{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, Benefit: true},
	{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, Benefit: true},
	{ID: "glm-5.3-flash", ContextWindow: 1048576, Benefit: true},
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute

	// authRetryBudget 凭据被拒后的「刷新 → 等待 → 重试」次数上限。
	//
	// 0301 decrypt token fail 集中在 token 到期前的刷新窗口：实测刷新成功后立刻
	// 重试仍可能再被拒一次（APIG 侧新 token 生效有延迟），隔约 1s 就通。给 3 次
	// 足够覆盖这个窗口，又不至于对着真正失效的凭据空转。
	authRetryBudget = 3

	// authRetryBackoff 每次刷新后重试前的等待。上游新 AK/SK+security_token 在
	// APIG 侧不是瞬时生效的，立即重试会继续吃 0301。
	authRetryBackoff = 1200 * time.Millisecond

	// authRefreshFreshWindow 判定「别人刚刷过、不必再刷」的时间窗。
	// refresh_token 用过即换，并发请求各自刷新会让先到手的 token 立刻作废。
	authRefreshFreshWindow = 30 * time.Second
)

// DefaultMaxBodyBytes 请求体默认上限（1107 实测 8MB 会 413，32MB 够长会话+图）。
const DefaultMaxBodyBytes int64 = 32 << 20

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth *oauthStore

	convMu sync.Mutex
	chats  map[string]string // account → 最近 chat_id
	// 黏性路由：conversation_id → account_name（多轮续接锁定同一账号，减少上游并发会话占用）。
	convAcct map[string]string

	loginMu sync.Mutex
	logins  map[string]*pendingLogin

	// 面板数据：最近请求、请求体统计、CLI 健康缓存。
	recent *requestLog
	stats  requestStats

	cliMu      sync.Mutex
	cliHealthy bool
	cliChecked time.Time
	cliErr     string
}

// pendingLogin WebUI 登录中间状态。
type pendingLogin struct {
	TicketID string
	Secret   string
	Verifier string
	Port     int
	Done     bool
	Err      string
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.QueueRetryDelay <= 0 {
		cfg.QueueRetryDelay = 10 * time.Second
	}
	if cfg.QueueMaxAttempts <= 0 {
		// 上游并发/TPM 排队是瞬时的，但 180×10s = 30 分钟对交互式调用等于挂死；
		// 取 30 次（约 5 分钟）作为默认，可用 queue_max_attempts 调整。
		cfg.QueueMaxAttempts = 30
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "glm-5.2"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Upstream == nil {
		cfg.Upstream = upstream.New(120 * time.Second)
	}
	if cfg.OAuthClient == nil {
		cfg.OAuthClient = upstream.New(15 * time.Second)
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	h := &Handler{
		cfg: cfg, mux: http.NewServeMux(), oauth: newOAuthStore(),
		chats: map[string]string{}, logins: map[string]*pendingLogin{},
		convAcct: map[string]string{},
		recent:   newRequestLog(recentRequestsCap),
	}
	h.loadChats()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 避免面板控制台刷 404
	})
	// WorkBuddy 风格控制台（页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("GET /admin/api/recent", h.withAuth(h.adminRecent))
	h.mux.HandleFunc("POST /admin/api/probe", h.withAuth(h.adminProbe))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAuth(h.adminCredits))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAuth(h.adminCheckin))
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("POST /admin/api/oauth/import-callback", h.withAuth(h.adminOAuthImportCallback))
	h.mux.HandleFunc("GET /oauth/callback", h.oauthCallback)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// oauthCallback 本地回调：浏览器同机时由 portal 携带 code 跳到这里。
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	secret := r.URL.Query().Get("secret")
	redirect := r.URL.Query().Get("redirect")
	log.Printf("oauth callback hit: has_code=%t has_secret=%t has_redirect=%t", code != "", secret != "", redirect != "")
	// portal 第一次回调：带 secret + redirect，要求 307 跳转（登录页链路的一部分）。
	if secret != "" && redirect != "" {
		// 用 redirect 里的 ticket_id 定位 pending login，并把华为云下发的 secret 换进去
		// （ticket 轮询必须用 portal 的 secret，而不是本地生成的）。
		if u, err := url.Parse(redirect); err == nil {
			tid := u.Query().Get("ticket_id")
			if tid != "" {
				h.oauth.enableTicketFallbackByTicket(tid, secret)
				h.loginMu.Lock()
				if p, ok := h.logins[tid]; ok {
					p.Secret = secret
					log.Printf("oauth callback: updated secret for ticket=%s", shortID(tid))
				}
				h.loginMu.Unlock()
			}
		}
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<h3>登录失败：缺少 code</h3><p>请回到 WebUI 重新发起登录。</p>"))
		return
	}
	candidates := h.oauth.codeSessionCandidates(r.URL.Query().Get("ticket_id"), secret)
	if len(candidates) == 0 {
		// 没有匹配（浏览器在远端时 code 通道不可用），提示用 ticket 通道
		_, _ = w.Write([]byte("<h3>登录已提交，请回到 WebUI 等待结果。</h3>"))
		return
	}
	// portal 的回调可能不带任何配对信息（实测只带 code），因此逐个候选尝试。
	// 授权码与 PKCE verifier 一对一校验：用错只会 STS5.1805，不会消耗授权码。
	var (
		tok     *upstream.TokenResponse
		sess    *oauthSession
		lastErr error
	)
	for _, cand := range candidates {
		t, err := h.cfg.OAuthClient.ExchangeCode(r.Context(), h.cfg.LoginConfig, code, cand.Verifier, cand.Port, cand.DPoPPrivateKey)
		if err == nil {
			tok, sess, lastErr = t, cand, nil
			break
		}
		lastErr = err
	}
	if lastErr != nil {
		// STS5.1805 = 授权码与服务端保存的 PKCE verifier 对不上，最常见的成因是
		// 链接生成后服务重启过（会话只在内存里）或重复使用了旧链接。
		hint := ""
		if strings.Contains(lastErr.Error(), "STS5.1805") || strings.Contains(lastErr.Error(), "invalid authorization code") {
			hint = "<p>授权码已失效：请回到 WebUI 重新点「登录」获取新链接，" +
				"并确保从生成到完成授权期间服务没有重启。</p>"
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>换取凭证失败：" + lastErr.Error() + "</h3>" + hint))
		return
	}
	if err := h.saveLoginResult(tok, sess.Verifier, sess.DPoPPrivateKey); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<h3>保存账号失败：" + err.Error() + "</h3>"))
		return
	}
	h.oauth.complete(sess.ID)
	_, _ = w.Write([]byte("<h3>登录成功，可关闭此页面并回到 WebUI。</h3>"))
}

func tokName(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.UserName
}

func tokToken(t *upstream.TokenResponse) string {
	if t == nil {
		return ""
	}
	return t.Credentials.SecurityToken
}

// saveLoginResult 落盘 auth 并加入账号池。
func (h *Handler) saveLoginResult(tok *upstream.TokenResponse, codeVerifier string, dpopPrivateJWK ...upstream.DPoPPrivateJWK) error {
	if h.cfg.AuthDir == "" {
		return errors.New("auth_dir not configured")
	}
	cred := tok.Credentials
	var privateJWK map[string]string
	if len(dpopPrivateJWK) > 0 {
		privateJWK = map[string]string(dpopPrivateJWK[0])
	}
	a := auth.New(tok.UserID, tok.UserName, tok.DomainID,
		cred.SecurityToken, cred.AccessKeyID, cred.SecretAccessKey,
		cred.Expiration, tok.RefreshToken, codeVerifier)
	a.SetClientID(h.cfg.LoginConfig.ClientID)
	a.SetDPoPPrivateKey(privateJWK)

	// portal 回调只回传 code，不带身份；user_id 为空会让 auth.FileName() 退化成
	// codearts-unknown.json，账号池按 UserID 建索引就会加载不到（或与既有账号重复）。
	// 因此落盘前把身份补全：先问 STS（caller-identity / current/user），打不通就
	// 从 refresh_token 的 user_profile 解——后者是本地可算的，永远有兜底。
	if a.UserID == "" || a.UserName == "" || a.DomainID == "" {
		uid, uname, domain := lookupIdentity(a)
		if uid == "" || uname == "" || domain == "" {
			// 兜底：身份就在 refresh_token 的 user_profile 里（JWT 的 payload）。
			// 注意 JWT 没有 sub claim，旧 fallback 解 sub 永远得到空串。
			id := auth.IdentityFromRefreshToken(tok.RefreshToken)
			if !id.Empty() {
				uid = firstNonEmpty(uid, id.UserID)
				uname = firstNonEmpty(uname, id.UserName)
				domain = firstNonEmpty(domain, id.DomainID)
				log.Printf("webui login: identity from refresh_token user_id=%q name=%q", uid, uname)
			} else {
				log.Printf("webui login: identity lookup failed and refresh_token carries none")
			}
		}
		a.UserID = firstNonEmpty(a.UserID, uid)
		a.UserName = firstNonEmpty(a.UserName, uname)
		a.DomainID = firstNonEmpty(a.DomainID, domain)
	}
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		return err
	}
	if p := h.cfg.Pool.AddAccount(a); p != nil && p.Auth != nil {
		// 刚登录的账号先不探测：用户很可能马上就要用，探测占用的上游会话
		// 槽位释放很慢，会把第一个真实请求挤到排队。
		noteTraffic(a.UserID)
		log.Printf("webui login success user_id=%s name=%s", a.UserID, a.UserName)
	} else {
		log.Printf("webui login saved user_id=%s name=%s (pool add failed)", a.UserID, a.UserName)
	}
	return nil
}

// lookupIdentity 用刚拿到的 STS 凭证查身份（先 caller-identity，再 current/user）。
func lookupIdentity(a *auth.Auth) (uid, name, domain string) {
	cred := upstream.SignCredential{
		AccessKeyID:     a.AccessKeyID,
		SecretAccessKey: a.SecretAccessKey,
		SecurityToken:   a.CloudDragonTok,
	}
	if uid, name, domain, err := upstream.CallerIdentity(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: caller-identity: %v", err)
	}
	if uid, name, domain, err := upstream.CurrentUser(cred); err == nil && uid != "" {
		return uid, name, domain
	} else if err != nil {
		log.Printf("webui login: current/user: %v", err)
	}
	return "", "", ""
}

// identityFromRefreshToken 已删除：refresh_token 的 JWT 里没有 sub claim，
// 取 sub 只会返回空串（本机 codearts-unknown.json 就是这条路的产物）。
// 身份改从 payload 的 user_profile 解，见 auth.IdentityFromRefreshToken。

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表（仅精确模型名）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式。
func (h *Handler) modelList() []map[string]any {
	entries := modelEntries(h.fetchDynamicModels())
	// CLI 后端能用的模型单独补进列表：它们不在直连可用目录里（直连对这些模型
	// 只会回 TM.000010051），但对客户端必须可见。
	if len(h.cfg.CLIModels) > 0 {
		have := make(map[string]bool, len(entries))
		for _, e := range entries {
			if id, ok := e["id"].(string); ok {
				have[strings.ToLower(id)] = true
			}
		}
		extra := make([]string, 0, len(h.cfg.CLIModels))
		for id := range h.cfg.CLIModels {
			if !have[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		for _, id := range extra {
			entries = append(entries, map[string]any{
				"id": id, "object": "model", "owned_by": "codearts-cli",
			})
		}
	}
	// 过滤掉「已知不可用」的模型：这些条目在客户端点开就是 404 / benefit not
	// found，列出来只会误导。必须作用在最终条目上——静态兜底表也会补位，
	// 只在动态结果上过滤会让被下架的模型又从静态表冒出来。
	// 未知（还没探测出结论）的保留：宁可让用户试，也别把可用模型藏起来。
	if len(entries) > 0 {
		kept := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			id, _ := e["id"].(string)
			if !h.modelKnownUnusable(id) {
				kept = append(kept, e)
			}
		}
		if len(kept) > 0 {
			entries = kept
		}
	}
	// 刚发现到的模型还没有可用性结论：后台补一轮探测，供后续请求过滤。
	// （首次请求返回的是「发现结果」，之后才收敛到「真实可用」。）
	h.probeUnknownAsync()
	return entries
}

// probeUnknownAsync 后台探测「还没有结论」的模型，不阻塞请求。
func (h *Handler) probeUnknownAsync() {
	availability.Lock()
	busy := availability.sweeping
	if !busy {
		availability.sweeping = true
	}
	availability.Unlock()
	if busy {
		return
	}
	go func() {
		defer func() {
			availability.Lock()
			availability.sweeping = false
			availability.Unlock()
		}()
		h.runProbes(h.pendingProbes())
	}()
}

// isHeartbeatUnavailable 报告心跳端点在上游环境不存在（APIG.0101 路由未发布）。
// 此时并发闸没启用，应降级为无心跳直连而不是换号。
func isHeartbeatUnavailable(err error) bool {
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "apig.0101") || strings.Contains(low, "does not exist or has not been published")
}

// heartbeatUnavailableLogGate 压掉「心跳不可用」告警的重复输出。
//
// 这条降级原来每个请求打一条（1112 实测几秒一条，一天几百行），把真正有用的
// 上游错误全淹了。降级本身是安全的（无心跳直连仍然出字），所以按账号限流：
// 每个账号每 24h 最多一条，并带累计次数，便于判断是偶发还是持续。
var heartbeatUnavailableLogGate = struct {
	sync.Mutex
	last    map[string]time.Time
	suppres map[string]int
}{last: map[string]time.Time{}, suppres: map[string]int{}}

// noteHeartbeatUnavailable 记录一次降级，必要时打一条日志。
func noteHeartbeatUnavailable(account string, err error) {
	const window = 24 * time.Hour

	heartbeatUnavailableLogGate.Lock()
	if heartbeatUnavailableLogGate.last == nil {
		heartbeatUnavailableLogGate.last = map[string]time.Time{}
		heartbeatUnavailableLogGate.suppres = map[string]int{}
	}
	now := time.Now()
	last, seen := heartbeatUnavailableLogGate.last[account]
	if seen && now.Sub(last) < window {
		heartbeatUnavailableLogGate.suppres[account]++
		heartbeatUnavailableLogGate.Unlock()
		return
	}
	skipped := heartbeatUnavailableLogGate.suppres[account]
	heartbeatUnavailableLogGate.suppres[account] = 0
	heartbeatUnavailableLogGate.last[account] = now
	heartbeatUnavailableLogGate.Unlock()

	if skipped > 0 {
		log.Printf("chat-session heartbeat unavailable (proceeding without) account=%s: %v (suppressed %d similar in last %s)",
			account, err, skipped, window)
		return
	}
	log.Printf("chat-session heartbeat unavailable (proceeding without) account=%s: %v", account, err)
}

// hasFreshCandidate 报告是否还有没试过、健康、且未被判定「该模型不可用」的
// 账号（上游 #11：模型级错误先换号，全部候选判死才 404）。
func (h *Handler) hasFreshCandidate(model string, tried map[string]bool) bool {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if st, _ := modelAvailability(acct.UID, model); st == availUnusable {
			continue
		}
		return true
	}
	return false
}

// modelKnownUnusable 报告该模型是否在**所有**健康账号上都被判定为不可用。
// 只要有一个账号没结论或可用，就返回 false（列表保留该模型）。
func (h *Handler) modelKnownUnusable(model string) bool {
	anyKnown, allUnusable := false, true
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		st, _ := modelAvailability(acct.UID, model)
		if st == availUnknown {
			return false // 还没结论，别急着下架
		}
		anyKnown = true
		if st != availUnusable {
			allUnusable = false
		}
	}
	return anyKnown && allUnusable
}

// isJunkModelID 过滤上游模型发现里的脏条目：'speed'（智速模式入口，不可直接调用）
// 与非 ASCII 的 ID——1107 实测发现过 GBK 误解码产生的乱码条目（「智速模式」的
// UTF-8 字节被当 GBK 解出的汉字串），这些是合法 UTF-8 但纯属上游展示别名，
// 混进 /v1/models 会污染客户端选择器。现有真实模型 ID 全部为 ASCII。
func isJunkModelID(id string) bool {
	if strings.EqualFold(id, "speed") {
		return true
	}
	for _, r := range id {
		if r > 0x7F {
			return true
		}
	}
	return false
}

// modelEntries 把账号模型目录 + 静态兜底包装成 OpenAI /v1/models 条目。
//
// 上游模型 ID 区分大小写：既列精确 ID（GLM-5.2），也补一条小写别名（glm-5.2，
// 老客户端习惯用小写），聊天时由 CanonicalModel 归一回精确 ID。
// 静态表只在动态目录缺失该 ID 时补位，顺序固定（动态已排序，静态按表序）。
func modelEntries(infos []upstream.ModelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(infos)+len(staticModels))
	seen := map[string]bool{}
	add := func(id string, ctx, maxOut int64, benefit bool) {
		if id == "" || seen[id] || isJunkModelID(id) {
			return
		}
		seen[id] = true
		if ctx == 0 {
			ctx = 131072 // 兜底
		}
		entry := map[string]any{
			"id":             id,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "codearts",
			"context_length": ctx,
		}
		if maxOut > 0 {
			entry["max_output_tokens"] = maxOut
		}
		if benefit {
			entry["benefit"] = true
		}
		out = append(out, entry)
	}
	// addAlias 精确 ID + 就近补一条小写别名（已是小写则跳过）。
	addAlias := func(id string, ctx, maxOut int64, benefit bool) {
		add(id, ctx, maxOut, benefit)
		if lower := strings.ToLower(id); lower != id {
			add(lower, ctx, maxOut, benefit)
		}
	}
	for _, mi := range infos {
		addAlias(mi.ID, mi.ContextWindow, mi.MaxTokens, mi.Benefit)
	}
	for _, sm := range staticModels {
		addAlias(sm.ID, sm.ContextWindow, 0, sm.Benefit)
	}
	return out
}

// fetchDynamicModels 返回模型展示列表，缓存 1h。
//
// 策略（2026-11-11）：有缓存就立即返回，过期后由**后台单飞**刷新，不阻塞调用方。
// 三路发现要串行打 4-5 个上游接口，同步等完会让 /v1/models 和首个聊天请求一起
// 卡住（第三方实现同样结论：模型目录刷新不能阻塞聊天首字）。冷启动没有内存缓存，
// 但 main 会先从 models-cache.json 恢复目录，所以正常情况不会走到同步发现。
//
// 目录按账号存放（限时福利按账号授予，账号之间不能互相污染）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	ids := dynamicModelsCache.ids
	fetched := dynamicModelsCache.fetched
	lastFail := dynamicModelsCache.lastFail
	refreshing := dynamicModelsCache.refreshing
	dynamicModelsCache.RUnlock()

	if len(ids) > 0 && time.Since(fetched) < dynamicModelsTTL {
		return ids
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !lastFail.IsZero() && time.Since(lastFail) < modelsFetchFailCooldown {
		return ids
	}
	// 已有刷新在跑：先返回旧目录，等它写完。
	if refreshing {
		return ids
	}

	dynamicModelsCache.Lock()
	if dynamicModelsCache.refreshing {
		dynamicModelsCache.Unlock()
		return ids
	}
	dynamicModelsCache.refreshing = true
	dynamicModelsCache.Unlock()

	if len(ids) > 0 {
		// 有旧目录可展示：后台刷新，调用方立即拿到旧值。
		go func() {
			defer h.finishModelRefresh()
			h.refreshDynamicModels()
		}()
		return ids
	}
	// 冷启动且无缓存：同步拉一次（否则客户端看不到任何模型）。
	h.refreshDynamicModels()
	h.finishModelRefresh()
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	return dynamicModelsCache.ids
}

// finishModelRefresh 清掉「正在刷新」标记。
func (h *Handler) finishModelRefresh() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.refreshing = false
	dynamicModelsCache.Unlock()
}

// refreshDynamicModels 真正执行发现并更新缓存（调用方负责 refreshing 标记）。
func (h *Handler) refreshDynamicModels() []upstream.ModelInfo {
	var sets [][]upstream.ModelInfo
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct.Auth == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			if infos, ok := upstream.AccountModels(acct.UID); ok {
				sets = append(sets, infos)
				continue
			}
		}
		infos, err := h.cfg.Upstream.FetchModels(acct.Auth) // 成功即写入该账号目录
		if err != nil {
			log.Printf("model discovery failed for account %s: %v", acct.Name, err)
			continue
		}
		if len(infos) == 0 {
			log.Printf("model discovery returned no models for account %s", acct.Name)
			continue
		}
		sets = append(sets, infos)
	}
	infos := upstream.MergeModels(sets...)
	if len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// pickAccount 挑一个「目录里登记了这个模型」的健康账号，没有则退回普通轮转。
//
// 混合账号池里各账号的套餐不同：/v1/models 展示的是并集，若把请求先发给目录里
// 没有该模型的账号，上游会按「未注册模型」报 400，既浪费一次轮转（MaxRotate 只有
// 3 次，池子超过 3 个账号时可能永远轮不到有能力的那台），又会给健康账号记错误、
// 累计到阈值就冷却 10 分钟。所以按能力优先挑号。
func (h *Handler) pickAccount(tried map[string]bool, model string) *pool.Account {
	want := strings.ToLower(upstream.CanonicalModel(model))
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	// 目录里没人登记这个模型。两种可能：① 谁都没有；② 目录还没发现
	// （客户端只调 /v1/chat/completions、从没调过 /v1/models 就属于这种）。
	// 直接退回轮转的话，混合池里没有该模型的账号会先吃一次 400，白耗 MaxRotate
	// （默认 3 次，账号多于 3 个时可能永远轮不到有能力的那台），还会给健康账号
	// 记错误、到阈值冷却 10 分钟。所以先补齐缺失的目录再挑一次。
	h.discoverMissingCatalogs(tried)
	if acct := h.pickByCatalog(tried, want); acct != nil {
		return acct
	}
	return h.cfg.Pool.PickExcluding(tried)
}

// pickByCatalog 在已发现目录的健康账号里挑第一个能服务该模型的。
func (h *Handler) pickByCatalog(tried map[string]bool, wantLower string) *pool.Account {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if models, known := upstream.AccountModels(acct.UID); known && catalogHas(models, wantLower) {
			if st, _ := modelAvailability(acct.UID, wantLower); st == availUnusable {
				continue // 该账号实测调不通这个模型，别选它
			}
			return acct
		}
	}
	return nil
}

// discoverMissingCatalogs 为「目录未知或已过期」的健康账号补一次模型发现。
//
// 只在聊天挑号失败时调用（不是热路径），且有 CatalogTTL 与失败退避兜底：
// 刚发现过的账号不会被重复打，上游某一路挂了也不会每次请求都空等超时。
func (h *Handler) discoverMissingCatalogs(tried map[string]bool) {
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || acct.Auth == nil || tried[acct.Name] || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		if !upstream.AccountCatalogStale(acct.UID) {
			continue
		}
		if _, err := h.cfg.Upstream.FetchModels(acct.Auth); err != nil {
			log.Printf("lazy model discovery failed for account %s: %v", acct.Name, err)
		}
	}
}

// accountCanServe 报告账号目录里是否登记了该模型；账号不存在或目录未知时乐观放行
// （交给上游判定，不凭缺失的目录拒请求）。
func (h *Handler) accountCanServe(acct *pool.Account, model string) bool {
	if acct == nil {
		return true
	}
	models, known := upstream.AccountModels(acct.UID)
	if !known {
		return true
	}
	return catalogHas(models, strings.ToLower(upstream.CanonicalModel(model)))
}

// catalogHas 按小写 ID 在模型目录里查模型。
func catalogHas(models []upstream.ModelInfo, wantLower string) bool {
	for _, mi := range models {
		if strings.ToLower(mi.ID) == wantLower {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ri := &requestInfo{start: time.Now()}
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	r = r.WithContext(withRequestInfo(r.Context(), ri))
	defer func() { h.recent.add(ri.entry(sw.code())) }()

	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	size := int64(len(body))
	if r.ContentLength > size {
		size = r.ContentLength // 413 时读取会被截断，长度以声明为准
	}
	h.noteBodySize(size)
	ri.body = int(size)
	if err != nil {
		ri.note("invalid_request", "read body: "+err.Error())
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > h.cfg.MaxBodyBytes {
		msg := fmt.Sprintf("request body exceeds %dMB limit", h.cfg.MaxBodyBytes>>20)
		h.note413(false)
		ri.note("request_too_large", msg)
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", msg)
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		ri.note("invalid_request", err.Error())
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ri.stream = req.Stream
	tools, err := nativeTools(req.Tools, req.ToolChoice)
	if err != nil {
		ri.note("invalid_request", err.Error())
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	affinity := r.Header.Get("X-Codearts-Chat-Id")
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Affinity")
	}
	if affinity == "" {
		affinity = r.Header.Get("X-Session-Id")
	}
	// 命中 CLI 后端的模型走 codearts serve；其余保持原直连路径。
	if cliModel, ok := h.cliModelFor(req.Model); ok {
		ri.source = "cli"
		ri.model = req.Model
		h.chatViaCLI(w, r, req, cliModel, affinity)
		return
	}
	req.ConversationID = conversationIDFor(req, affinity)

	toolsOn := toolsActive(req)
	model := h.cfg.DefaultModel
	if req.Model != "" && req.Model != "auto" {
		model = req.Model
	}
	ri.model = model

	// OpenAI 线格式本身不携带会话 ID；conversationIDFor 用首条 user 消息
	// 为整条 Agent 工具链固定 CodeArts 内置 chat_id 与账号亲和。V2 接口
	// 不保留语义上下文，所以每轮仍重放完整 OpenAI 历史。
	explicitChat := validChatID(req.ConversationID)
	stickyAcct := ""
	if explicitChat {
		h.convMu.Lock()
		stickyAcct = h.convAcct[req.ConversationID]
		h.convMu.Unlock()
	}
	msgs := buildUpstreamMessages(req, toolsOn)

	// 已经确认「所有健康账号都用不了这个模型」时直接快速失败：不必再打上游、
	// 也不必把账号轮转一遍（轮转只会拿到同样的 not registered / benefit not found）。
	if h.modelKnownUnusable(model) {
		ri.note("model_not_available", "model "+model+" is not available on any configured account")
		writeOpenAIError(w, http.StatusNotFound, "model_not_available",
			"model "+model+" is not available on any configured account")
		return
	}

	tried := map[string]bool{}
	// 上游 #11：已判定「该账号无权调用该模型」的账号直接跳过，不浪费一次
	// 上游往返去复现同样错误。
	for _, acct := range h.cfg.Pool.Accounts() {
		if st, reason := modelAvailability(acct.UID, model); st == availUnusable && reason == modelAccessDeniedReason {
			tried[acct.Name] = true
		}
	}
	if tried[stickyAcct] {
		stickyAcct = ""
	}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *pool.Account
		if stickyAcct != "" {
			// 续接会话：锁定原账号（若仍健康）。
			acct = h.cfg.Pool.Get(stickyAcct)
			if acct == nil || !h.cfg.Pool.Healthy(stickyAcct) {
				stickyAcct = ""
				acct = h.cfg.Pool.PickExcluding(tried)
			} else {
				tried[acct.Name] = true
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcluding(tried)
		}
		if acct == nil {
			break
		}
		tried[acct.Name] = true
		ri.account = acct.Name

		// 阻塞等待并发槽位（上游单账号并发会话释放慢，串行最稳）。
		// 超时 180s 排队（5 个请求 × ~30s 上限），避免高并发立即失败跳号。
		if !h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second) {
			log.Printf("account %s concurrent limit reached after wait, trying next", acct.Name)
			continue
		}

		ok, verr := h.cfg.Pool.Validate(acct)
		if verr == nil && !ok {
			h.cfg.Pool.ReleaseLock(acct.Name)
			lastErr = errors.New("token invalid")
			continue
		}
		if verr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name)
			lastErr = verr
			continue
		}

		// 检查是否需要保活
		if h.cfg.Pool.NeedKeepalive(acct.Name) {
			h.cfg.Pool.PingKeepalive(acct.Name)
		}

		chatID := req.ConversationID
		// 上游要求 chat_id 为 32 位十六进制（UUID 去连字符）；无显式 id 时每次新建。
		if !validChatID(chatID) {
			chatID = randHex(32)
		}

		token, accessKeyID, secretAccessKey := acct.Auth.Credentials()
		cred := upstream.SignCredential{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SecurityToken:   token,
		}
		// 并发会话心跳（session.go）：先 busy 登记槽位，对话请求带同一个
		// User-Session-Id，结束时 idle 注销。抢不到会话 = 该账号并发真满，
		// 属请求级拥塞：换号，不冷却（规则 5）。
		var sess *upstream.ChatSession
		if h.cfg.SessionHeartbeat {
			s, sessErr := acct.Client.BeginChatSession(r.Context(), cred)
			if sessErr != nil {
				if isHeartbeatUnavailable(sessErr) {
					// 心跳接口在该上游环境回 APIG.0101（路由未发布）：这套并发闸
					// 没启用，静默降级为无心跳直连。日志按账号限流，见
					// noteHeartbeatUnavailable——原来每个请求一条，一天几百行。
					noteHeartbeatUnavailable(acct.Name, sessErr)
				} else {
					h.cfg.Pool.ReleaseLock(acct.Name)
					log.Printf("chat-session busy rejected account=%s model=%s: %v", acct.Name, model, sessErr)
					lastErr = sessErr
					continue
				}
			}
			sess = s
		}
		chatOpts := upstream.ChatOptions{
			ReasoningEffort: req.ReasoningEffort,
			MaxTokens:       req.MaxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			Tools:           tools,
			ToolChoice:      nativeToolChoice(req.ToolChoice),
		}
		if sess != nil {
			chatOpts.UserSessionID = sess.ID()
		}
		// 上游并发会话/TPM 排队是瞬时的。对齐 CodeArts Agent IDE
		// 参考实现：默认每 10s 重试同一账号，最多 180 次（30min）。
		// 等待期间释放并发锁，让排队的请求也能尝试（避免死锁式串行等待）。
		var rc io.ReadCloser
		var serr error
		authRetried := false
		authAttempts := 0
		// 限时福利路由按「本次实际使用的账号」判定：账号池里套餐可能不同。
		benefit := upstream.IsBenefitModel(acct.UID, model)
		noteTraffic(acct.UID) // 有真实流量：本轮探测整体避让
		for retry := 0; retry < h.cfg.QueueMaxAttempts; retry++ {
			rc, serr = acct.Client.ChatStreamWithOptions(r.Context(), chatID, msgs, "", cred, acct.UserName, model, chatOpts, benefit)
			if serr == nil {
				break
			}
			// 凭据/签名被拒（401、APIG.0301 decrypt token fail、APIG.0602）：
			// 立即强制刷新 token 再重试，而不是把账号扔进冷却。
			//
			// 判据从「HTTP 401」放宽到完整的 auth 分类：上游可能把 0301 包成
			// 其它状态码（200 内嵌 / 502），只认状态码会漏掉这些形态。刷新走
			// RefreshTokenIfStale 单飞——refresh_token 用过即换，并发请求各自
			// 刷新会让先到手的 token 立刻作废，那正是"刷新成功却仍被拒"的来源。
			if !authRetried && acct.Auth.Refresh() != "" && isAuthError(serr) {
				refreshed, rerr := h.cfg.Pool.RefreshTokenIfStale(acct.Name, authRefreshFreshWindow)
				if rerr == nil {
					token, accessKeyID, secretAccessKey = acct.Auth.Credentials()
					cred = upstream.SignCredential{
						AccessKeyID:     accessKeyID,
						SecretAccessKey: secretAccessKey,
						SecurityToken:   token,
					}
					authAttempts++
					if authAttempts >= authRetryBudget {
						authRetried = true
					}
					// 等一个极短窗口再重试：新 token 在 APIG 侧生效不是瞬时的
					// （实测刷新后立刻重试仍可能 0301，隔 ~1s 即通）。
					select {
					case <-time.After(authRetryBackoff):
					case <-r.Context().Done():
						writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
						return
					}
					if refreshed {
						log.Printf("upstream auth error account=%s: token refreshed (attempt %d/%d), retrying",
							acct.Name, authAttempts, authRetryBudget)
					} else {
						log.Printf("upstream auth error account=%s: credentials already refreshed by a concurrent request, retrying (attempt %d/%d)",
							acct.Name, authAttempts, authRetryBudget)
					}
					continue
				}
				log.Printf("upstream auth error account=%s: refresh before retry failed: %v", acct.Name, rerr)
			}
			var ae *upstream.ApiError
			if errors.As(serr, &ae) && isQueueLimitError(ae) {
				log.Printf("upstream queue limit retry=%d/%d account=%s, waiting %s", retry+1, h.cfg.QueueMaxAttempts, acct.Name, h.cfg.QueueRetryDelay)
				// 释放锁让其他请求有机会，等待后重新获取
				h.cfg.Pool.ReleaseLock(acct.Name)
				select {
				case <-time.After(h.cfg.QueueRetryDelay):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusServiceUnavailable, "client_cancelled", "client disconnected")
					return
				}
				if !h.cfg.Pool.AcquireLockWait(acct.Name, 30*time.Second) {
					lastErr = errors.New("concurrent limit: could not reacquire lock after wait")
					break
				}
				continue
			}
			break // 非并发错误，跳出重试
		}
		if serr != nil {
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放槽位再换号
			sess.Stop()                       // 心跳会话一并注销
			var bodyLimit *upstream.BodyLimitError
			if errors.As(serr, &bodyLimit) {
				// 本地预检：请求体超过上游 chat-completions 的 10MiB 硬上限。
				// 上游对这类请求回 400 InferHub.001001005（「参数非法」），换账号/
				// 换模型结果相同，所以不轮换、不冷却，直接给客户端一个说得清的 413。
				h.note413(true)
				ri.note("request_too_large", bodyLimit.Error())
				writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", bodyLimit.Error())
				return
			}
			if isParamInvalidError(serr) {
				// Client request-param problem: rotating accounts yields the same 400,
				// and cooling accounts takes down the pool for minutes.
				ri.note("invalid_request", "upstream rejected request params: "+truncateMsg(serr.Error(), 200))
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					"upstream rejected request params: "+truncateMsg(serr.Error(), 200))
				return
			}
			if isContentModerationError(serr) {
				// 上游内容审查拒绝：换账号、换措辞都未必过，但它是针对本次内容的
				// 结论，不是账号故障。直接回 400 让客户端看到真实原因，别再让整池
				// 陪跑 10 分钟冷却（1109 实测 81011.403 被当 5xx）。
				h.handleUpstreamError(ri, acct, model, serr) // 只记日志，不动账号
				writeOpenAIError(w, http.StatusBadRequest, "content_filter",
					"upstream content moderation rejected this request: "+truncateMsg(serr.Error(), 200))
				return
			}
			if isGatewayTimeoutError(serr) {
				// 网关后端超时（APIG.0203）：账号健康，只是这次请求上游 60s 没
				// 出首字节。不冷却账号；多账号时换号再试，单账号直接给 504。
				h.handleUpstreamError(ri, acct, model, serr) // 只记日志，不动账号
				if !h.hasFreshCandidate(model, tried) {
					w.Header().Set("Retry-After", "20")
					writeOpenAIError(w, http.StatusGatewayTimeout, "upstream_timeout",
						"upstream gateway backend timeout: "+truncateMsg(serr.Error(), 200))
					return
				}
				continue
			}
			lastErr = serr
			if h.handleUpstreamError(ri, acct, model, serr) {
				// 模型级错误：还有没试过的健康账号就先换号（多账号时 B 可能
				// 有权限），全判死才给客户端明确答复。
				if !h.hasFreshCandidate(model, tried) {
					writeOpenAIError(w, http.StatusNotFound, "model_not_available",
						"model "+model+" is not available: "+truncateMsg(serr.Error(), 200))
					return
				}
			}
			continue
		}
		// 真实调用成功：记下「这个账号能用这个模型」，供 /v1/models 过滤参考。
		markUsable(acct.UID, model)
		noteTraffic(acct.UID)

		w.Header().Set("X-Codearts-Chat-Id", chatID)

		storeChat := func() {
			// 仅缓存显式会话，便于同 conversation_id 续聊；不把一次性测连写进账号默认会话。
			if !explicitChat {
				return
			}
			h.convMu.Lock()
			h.chats[acct.Name] = chatID
			h.convAcct[req.ConversationID] = acct.Name // 黏性路由：续接锁定同账号
			h.convMu.Unlock()
			h.saveChats()
		}
		if req.Stream {
			var usage func(*upstream.RawCompletion) map[string]any
			if req.IncludeUsage {
				usage = func(comp *upstream.RawCompletion) map[string]any { return usageEstimate(msgs, comp) }
			}
			werr := upstream.StreamCaptureWithUsage(w, rc, model, func(comp *upstream.RawCompletion) {
				storeChat()
				// 流式传输中定期保活
				h.cfg.Pool.PingKeepalive(acct.Name)
			}, usage)
			rc.Close()
			h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
			sess.Stop()                       // 心跳会话注销（idle 在最后一次 busy 之后）
			if werr != nil {
				log.Printf("chat stream account=%s error: %v", acct.Name, werr)
				// 首帧就是错误：HTTP 头尚未写出，可以回真正的状态码 + JSON。
				// 这条路径覆盖「上游 200 但正文第一帧就带 error_code」——以前只能
				// 发 200 + SSE error 事件，客户端把模型不存在/参数非法当成流中断。
				var pre *upstream.PreStreamError
				if errors.As(werr, &pre) {
					kind, reason := classifyUpstreamError(pre.Err)
					h.handleUpstreamError(ri, acct, model, pre.Err)
					if kind == errKindModelUnavailable && h.hasFreshCandidate(model, tried) {
						// 响应头尚未写出，还有没试过的健康账号就直接换号重试
						//（多账号时 B 可能有权限）。
						continue
					}
					switch kind {
					case errKindModelUnavailable:
						writeOpenAIError(w, http.StatusNotFound, "model_not_available",
							"model "+model+" is not available: "+truncateMsg(pre.Err.Error(), 200))
					case errKindParamInvalid:
						writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
							"upstream rejected request params: "+truncateMsg(pre.Err.Error(), 200))
					case errKindContentModeration:
						writeOpenAIError(w, http.StatusBadRequest, "content_filter",
							"upstream content moderation rejected this request: "+truncateMsg(pre.Err.Error(), 200))
					case errKindAuthExpired:
						writeOpenAIError(w, http.StatusUnauthorized, "relogin_required", reason)
					case errKindAuth:
						writeOpenAIError(w, http.StatusBadGateway, "upstream_auth_error",
							"upstream rejected credentials: "+truncateMsg(pre.Err.Error(), 200))
					case errKindRateLimit, errKindConcurrentLimit:
						writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit",
							"upstream busy: "+truncateMsg(pre.Err.Error(), 200))
					case errKindGatewayTimeout:
						w.Header().Set("Retry-After", "20")
						writeOpenAIError(w, http.StatusGatewayTimeout, "upstream_timeout",
							"upstream gateway backend timeout: "+truncateMsg(pre.Err.Error(), 200))
					default:
						writeOpenAIError(w, http.StatusBadGateway, "upstream_error",
							truncateMsg(pre.Err.Error(), 300))
					}
					return
				}
				// 流式响应头已经发出去了，没法再改状态码；但必须把「这个模型对这个
				// 账号不可用」记进可用性缓存，否则错误会累计到阈值把账号冷却——
				// 上游把模型级错误塞进 200 的 SSE 正文（如 APIG.0101）时正是这条路。
				if reason := upstreamUnavailableReason(werr); reason != "" {
					markUnusable(acct.UID, model, reason)
					log.Printf("model unusable account=%s model=%s: %s", acct.Name, model, reason)
				} else if kind, _ := classifyUpstreamError(werr); kind != errKindParamInvalid &&
					kind != errKindContentModeration && kind != errKindConcurrentLimit &&
					kind != errKindGatewayTimeout {
					h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
			} else {
				h.cfg.Pool.NoteSuccess(acct.Name)
			}
			return
		}

		comp, aerr := upstream.AggregateRaw(rc)
		rc.Close()
		h.cfg.Pool.ReleaseLock(acct.Name) // 释放锁
		sess.Stop()                       // 心跳会话注销
		if aerr != nil {
			if isParamInvalidError(aerr) {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
					"upstream rejected request params: "+truncateMsg(aerr.Error(), 200))
				return
			}
			if isContentModerationError(aerr) {
				// 与直接上游错误路径保持一致：内容审查是「本次内容被拒」，不是账号
				// 故障。以前这里只认参数错误，81011 被塞进 lastErr，轮转结束后变成
				// 503 no_healthy_account，还把整池陪跑冷却。
				h.handleUpstreamError(ri, acct, model, aerr) // 只记日志，不动账号
				writeOpenAIError(w, http.StatusBadRequest, "content_filter",
					"upstream content moderation rejected this request: "+truncateMsg(aerr.Error(), 200))
				return
			}
			if isGatewayTimeoutError(aerr) {
				// 网关后端超时（APIG.0203）：见流式路径同名分支——不冷却账号，
				// 没有其它健康账号时直接回 504 让客户端稍后重试。
				h.handleUpstreamError(ri, acct, model, aerr) // 只记日志，不动账号
				if !h.hasFreshCandidate(model, tried) {
					w.Header().Set("Retry-After", "20")
					writeOpenAIError(w, http.StatusGatewayTimeout, "upstream_timeout",
						"upstream gateway backend timeout: "+truncateMsg(aerr.Error(), 200))
					return
				}
				continue
			}
			lastErr = aerr
			// 这里以前是「任何聚合错误一律冷却账号」。上游会把模型级错误（未注册 /
			// 未开通 / 福利过期 / APIG.0101 路由不存在）塞在 200 的 SSE 正文里，
			// 于是点一个坏模型就把单账号池冷 10 分钟（1109 glm-5.3-flash 实测）。
			// 统一走 handleUpstreamError 分类，和流式路径保持同一套判据。
			if h.handleUpstreamError(ri, acct, model, aerr) {
				// 模型级错误：还有没试过的健康账号就先换号（多账号时 B 可能
				// 有权限），全判死才给客户端明确答复。
				if !h.hasFreshCandidate(model, tried) {
					writeOpenAIError(w, http.StatusNotFound, "model_not_available",
						"model "+model+" is not available: "+truncateMsg(aerr.Error(), 200))
					return
				}
			}
			continue
		}

		content, finish := comp.Content, comp.Finish
		calls := fromUpstreamToolCalls(comp.ToolCalls)
		// 原生 tool_calls 是 GLM-5.2 的主路径。仅保留文本协议解析作为
		// 旧模型/异常输出的兼容兜底，并同时检查 reasoning_content，
		// 避免模型把工具 JSON 放到思考通道时错误以 stop 结束。
		if toolsOn && len(calls) == 0 {
			if c, cleanContent, found := toolResponse(comp.Content + "\n" + comp.Reasoning); found {
				calls = c
				assignCallIDs(calls)
				content = cleanContent
				finish = "tool_calls"
			}
		}
		if toolsOn && len(calls) > 0 {
			kept, dropped := filterKnownCalls(calls, req.Tools)
			if len(dropped) > 0 {
				log.Printf("dropped unknown tool names model=%s: %v", model, dropped)
			}
			if len(kept) > 0 {
				calls = kept
				finish = "tool_calls"
			} else {
				calls, finish = nil, "stop"
			}
		}
		if len(calls) > 0 {
			finish = "tool_calls"
		}
		storeChat()
		h.cfg.Pool.NoteSuccess(acct.Name)

		writeJSON(w, http.StatusOK, buildCompletion(model, comp.Reasoning, content, calls, finish, chatID, usageEstimate(msgs, comp)))
		return
	}
	msg := "all accounts unavailable (disabled/cooldown)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	ri.note("no_healthy_account", msg)
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// buildUpstreamMessages 把 OpenAI 消息原生映射为 CodeArts V2 chat-completions。
// Chat-Id/Session-Id 只做亲和与缓存；多轮语义仍由完整 messages 承载。
// wireContent 在消息带图片时改用原始分片，纯文本维持原来的字符串。
func wireContent(m openAIMessage) any {
	if m.Content != nil {
		return m.Content
	}
	return m.Text
}

func buildUpstreamMessages(req *chatRequest, toolsOn bool) []upstream.ChatMessage {
	_ = toolsOn // 保留参数以稳定内部调用面；工具 schema 由 ChatOptions 原生传递。
	keepCalls, keepResults := pairedToolMessages(req.Messages)
	out := make([]upstream.ChatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user":
			out = append(out, upstream.ChatMessage{Role: m.Role, Content: wireContent(m)})
		case "assistant":
			reasoning := m.ReasoningContent
			wire := upstream.ChatMessage{Role: "assistant", Content: wireContent(m), ReasoningContent: &reasoning}
			if keepCalls[i] {
				for _, c := range m.ToolCalls {
					wire.ToolCalls = append(wire.ToolCalls, upstream.ChatToolCall{
						ID: c.ID, Type: "function",
						Function: upstream.ChatToolFunction{Name: c.Name, Arguments: c.Arguments},
					})
				}
				if m.Text == "" && m.Content == nil {
					wire.Content = nil
				}
			}
			out = append(out, wire)
		case "tool":
			if keepResults[i] {
				content := m.Text
				if content == "" {
					content = "(no output)"
				}
				out = append(out, upstream.ChatMessage{
					Role: "tool", Content: content, ToolCallID: m.ToolCallID, Name: m.Name,
				})
			}
		}
	}
	return out
}

// pairedToolMessages 只保留完整成对的 assistant.tool_calls + tool results。
// CodeArts 会对孤儿或部分成对的工具历史返回 400，并使后续每轮持续失败。
func pairedToolMessages(messages []openAIMessage) (map[int]bool, map[int]bool) {
	resultIndex := make(map[string]int)
	for i, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultIndex[m.ToolCallID] = i
		}
	}
	keepCalls := make(map[int]bool)
	keepResults := make(map[int]bool)
	for i, m := range messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		complete := true
		for _, c := range m.ToolCalls {
			if c.ID == "" {
				complete = false
				break
			}
			if _, ok := resultIndex[c.ID]; !ok {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		keepCalls[i] = true
		for _, c := range m.ToolCalls {
			keepResults[resultIndex[c.ID]] = true
		}
	}
	return keepCalls, keepResults
}

// nativeTools preserves a named function choice by exposing only that function
// to the upstream, whose tool_choice accepts strings but not a named object.
func nativeTools(tools []map[string]any, choice toolChoiceOpenAI) ([]map[string]any, error) {
	if choice.Mode != "function" {
		return tools, nil
	}
	var selected map[string]any
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]any)
		if !ok || fn["name"] != choice.Function {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("tool_choice function %q is defined more than once", choice.Function)
		}
		selected = tool
	}
	if selected == nil {
		return nil, fmt.Errorf("tool_choice function %q is not defined in tools", choice.Function)
	}
	return []map[string]any{selected}, nil
}

func nativeToolChoice(choice toolChoiceOpenAI) any {
	switch choice.Mode {
	case "none", "required":
		return choice.Mode
	case "function":
		// CodeArts InferHub only accepts string-form tool_choice; the object form
		// is rejected with InferHub.001001005.400 param invalid (measured 2026-10-06).
		// nativeTools limits the tools to the named function, so "required"
		// preserves the client's forced-function constraint.
		return "required"
	default:
		return "auto"
	}
}

func fromUpstreamToolCalls(calls []upstream.ChatToolCall) []openAIToolCall {
	out := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openAIToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
	}
	assignCallIDs(out)
	return out
}

func usageEstimate(msgs []upstream.ChatMessage, comp *upstream.RawCompletion) map[string]any {
	if comp.Usage != nil {
		return comp.Usage
	}
	var pt int
	for _, m := range msgs {
		switch content := m.Content.(type) {
		case string:
			pt += len([]rune(content))/4 + 1
		default:
			raw, _ := json.Marshal(content)
			pt += len([]rune(string(raw)))/4 + 1
		}
	}
	ct := (len([]rune(comp.Content))+len([]rune(comp.Reasoning)))/4 + 1
	return map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
}

// buildCompletion 组装非流式响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, chatID string, usage map[string]any) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		message["tool_calls"] = toOpenAIToolCalls(calls)
		if content == "" {
			message["content"] = nil
		} else {
			message["content"] = content
		}
	} else {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"chat_id": chatID,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// handleUpstreamError 记录错误并决定是否处罚账号。
// 返回 true 表示这是「模型对这个账号不可用」的模型级错误：调用方应立即结束
// 轮转并向客户端返回明确错误（换账号也是同样的结果）。
//
// 分类见 availability.go 的 classifyUpstreamError；每个类别对账号状态的影响
// 都在下面的 switch 里显式写明，不再靠猜 HTTP 状态码。
func (h *Handler) handleUpstreamError(ri *requestInfo, acct *pool.Account, model string, err error) bool {
	kind, reason := classifyUpstreamError(err)
	ri.noteKind(kind.String(), reason)
	switch kind {
	case errKindModelUnavailable:
		markUnusable(acct.UID, model, reason)
		log.Printf("model unusable account=%s model=%s: %s", acct.Name, model, reason)
		return true

	case errKindParamInvalid:
		// 请求形状/模型能力问题：换账号结果相同，不轮转、不冷却、不记错误。
		log.Printf("upstream param invalid (no penalty) account=%s model=%s msg=%s",
			acct.Name, model, truncateMsg(upstreamErrorText(err), 160))
		return false

	case errKindContentModeration:
		log.Printf("upstream content moderation reject account=%s model=%s msg=%s",
			acct.Name, model, truncateMsg(err.Error(), 120))
		return false

	case errKindAuthExpired:
		// refresh_token 终态失效：只能重新登录，禁用是正确动作（不会自愈）。
		h.cfg.Pool.Disable(acct.Name, reason)
		log.Printf("account disabled (re-login required) account=%s: %s", acct.Name, reason)
		return false

	case errKindAuth:
		// 1110 事故：这里以前是 Disable，一次 401 就把账号永久禁用，watchdog
		// 跳过 disabled 账号 → 凭据不再刷新 → 整池 503 直到手工改 state.json。
		//
		// 请求路径在走到这里之前已经做过「立即刷新 token 再重试」（见
		// chatCompletions 的 auth 重试段）。还能走到这里说明刷新后重试仍被拒，
		// 或者根本没有 refresh_token。此时用**软**冷却而不是累计错误：
		// NoteError 在 err_threshold=3 时给 10m 冷却，比原来的 60s 更狠，
		// 而 0301 往往是刷新窗口的瞬态——60s 后自愈才是想要的行为。
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, reason)
		log.Printf("upstream auth error (refresh+retry exhausted, soft cooldown) account=%s: %s msg=%s",
			acct.Name, reason, truncateMsg(upstreamErrorText(err), 160))
		return false

	case errKindConcurrentLimit:
		// 并发会话上限是瞬时错误：上游槽位释放慢，冷却反而误伤后续请求。
		log.Printf("upstream concurrent limit (transient) account=%s msg=%s",
			acct.Name, truncateMsg(upstreamErrorText(err), 80))
		return false

	case errKindGatewayTimeout:
		// 上游网关后端超时（APIG.0203）：账号健康，紧接一个小请求即可 200。
		// 1116 事故：这类 504 落进 5xx 兜底，单账号池被冷 10 分钟，所有会话
		// 503 no_healthy_account。这里只记日志，不罚账号、不记模型不可用。
		log.Printf("upstream gateway timeout (transient, no penalty) account=%s model=%s msg=%s",
			acct.Name, model, truncateMsg(upstreamErrorText(err), 160))
		return false

	case errKindRateLimit:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, reason)
		return false

	case errKindServer:
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, upstreamErrorText(err))
		return false
	}

	// 未识别：仍按状态码兜底（5xx/429 走冷却，其余累计错误）。
	var ae *upstream.ApiError
	if errors.As(err, &ae) && ae.Status >= 500 {
		h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
		return false
	}
	h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	return false
}

// isContentModerationError 判断是否为上游的内容审查拒绝（InferHub.ModelArts.81011.403）。
//
// 这类响应是「这次请求的内容我不答」，跟账号健康、跟模型能力都无关：换账号逐字
// 重放同一段内容，上游还是拒。上游把它包成 502，于是落到 `ae.Status >= 500`
// 分支把单账号池冷 10 分钟——1109 实测长中文/敏感话题一句话就能打掉整池。
func isContentModerationError(err error) bool {
	kind, _ := classifyUpstreamError(err)
	return kind == errKindContentModeration
}

// isGatewayTimeoutError 判断是否为上游网关的后端超时（APIG.0203 Backend timeout）。
//
// 1116 实测：两个 800+ 消息的大请求分别卡了 61–63 秒，上游网关回 504
// APIG.0203，反代按 5xx 兜底把单账号池冷 10 分钟，两条会话 11 次重试全 503。
// 账号本身健康——冷却到期后同一个模型小请求 2.1s 就 200。
func isGatewayTimeoutError(err error) bool {
	kind, _ := classifyUpstreamError(err)
	return kind == errKindGatewayTimeout
}

// isParamInvalidError 判断是否为上游「请求参数/能力不匹配」类错误
// （InferHub.001001005 参数非法、001001020 多模态不匹配）。
// 这类错误是客户端请求形状问题：换账号结果相同，绝不能冷却账号（曾因此打停整池）。
// 判据集中在 availability.go 的 classifyUpstreamMessage，避免多份字符串表漂移。
func isParamInvalidError(err error) bool {
	kind, _ := classifyUpstreamError(err)
	return kind == errKindParamInvalid
}

// isAuthError 判断是否为「凭据/签名被上游拒绝」类错误，即值得刷新 token 后重试的情形
// （401、APIG.0301 decrypt token fail、APIG.0602、verify ak sk signature fail）。
//
// 以前判据只看 HTTP 401：上游把 0301 塞进 200 的 SSE 正文或包成 502 时，请求路径
// 就不会触发刷新，直接落到 handleUpstreamError 里给账号上一个 60s 软冷却——
// 2026-11-15 实测 12:54/12:56/12:58/13:00 四次冷却全是这种形态。
func isAuthError(err error) bool {
	kind, _ := classifyUpstreamError(err)
	return kind == errKindAuth
}

// isConcurrentLimitError 判断是否为上游并发会话上限错误（瞬时、可重试）。
func isConcurrentLimitError(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "tm.00001041") || strings.Contains(low, "并发会话")
}

func isQueueLimitError(err *upstream.ApiError) bool {
	if err == nil {
		return false
	}
	if err.Status == http.StatusTooManyRequests {
		return true
	}
	low := strings.ToLower(err.Message)
	return (err.Status == http.StatusBadRequest && isConcurrentLimitError(low)) ||
		strings.Contains(low, "inferhub.modelarts.81111.429") ||
		strings.Contains(low, "tpm limit")
}

func truncateMsg(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := crand.Read(b); err != nil {
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

func validChatID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (h *Handler) loadChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	if json.Unmarshal(raw, &h.chats) != nil {
		return
	}
	h.migrateAnonChats()
}

// migrateAnonChats 把历史遗留的空键 chat_id 迁到真实账号名下。
//
// 身份为空时 acct.Name 是空串，h.chats[""] = <chat_id>；身份修好后键变成
// user_id，那个空键就成孤儿（既不会被读也不会被清）。单账号部署下空键必然属于
// 当前唯一账号，直接迁移；多账号时也能确定归属（空键只可能来自身份缺失期）。
func (h *Handler) migrateAnonChats() {
	orphan, ok := h.chats[""]
	if !ok {
		return
	}
	delete(h.chats, "")
	var target string
	if accts := h.cfg.Pool.Accounts(); len(accts) == 1 {
		target = accts[0].Name
	}
	if target == "" {
		log.Printf("chats: dropped orphan chat_id=%s (account identity still unknown)", orphan)
		h.saveChats()
		return
	}
	if _, exists := h.chats[target]; !exists {
		h.chats[target] = orphan
	}
	h.saveChats()
	log.Printf("chats: migrated orphan chat_id=%s to account=%s", orphan, target)
}

func (h *Handler) saveChats() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, _ := json.MarshalIndent(h.chats, "", "  ")
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile)
}

// ---------------------------------------------------------------------------
// CLI 后端（codearts serve）
// ---------------------------------------------------------------------------

// isCLIModelName 只按配置判断模型是否属于 CLI 后端（与 CLI 客户端是否就绪无关）。
func (h *Handler) isCLIModelName(model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		key = strings.ToLower(h.cfg.DefaultModel)
	}
	_, ok := h.cfg.CLIModels[key]
	return ok
}

// cliModelFor 判断该模型是否应由 CLI 后端处理，并返回 CLI 侧 modelID。
func (h *Handler) cliModelFor(model string) (string, bool) {
	if h.cfg.CLI == nil || len(h.cfg.CLIModels) == 0 {
		return "", false
	}
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		key = strings.ToLower(h.cfg.DefaultModel)
	}
	id, ok := h.cfg.CLIModels[key]
	if !ok {
		return "", false
	}
	return id, true
}

// chatViaCLI 把 OpenAI 请求转成 codearts serve 的 session/message 调用。
//
// 说明：CLI 是 agent 内核，回复是纯文本（不返回 tool_calls）。因此 CLI 路径
// 不支持 OpenAI 工具调用，需要工具的场景请回退到直连模型（GLM-5.2）。
func (h *Handler) chatViaCLI(w http.ResponseWriter, r *http.Request, req *chatRequest, cliModel, affinity string) {
	ri := requestInfoFrom(r.Context())
	if ri != nil {
		ri.model = req.Model
		ri.source = "cli"
	}
	convID := conversationIDFor(req, affinity)
	prompt, images := promptAndImages(req.Messages)
	// 带工具时：CLI 是 agent 内核、不返回 OpenAI tool_calls，因此按官方对 DSML
	// 模型的做法把工具 schema 注入 prompt，要求模型输出 ```tool_call 代码块，
	// 再由 toolResponse 解析成标准 tool_calls。
	toolsOn := len(req.Tools) > 0 && req.ToolChoice.Mode != "none"
	if toolsOn {
		wrapped := toolPrompt(prompt, req.Tools, req.ToolChoice)
		// 工具规则是前置的：图片 Offset 是按未包装的 prompt 算的，要整体后移，
		// 否则增量过滤会误判图片是否属于本轮。
		shift := len(wrapped) - len(prompt)
		for i := range images {
			images[i].Offset += shift
		}
		prompt = wrapped
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	res, err := h.cfg.CLI.Chat(ctx, cli.ChatRequest{
		ConversationID: convID,
		Provider:       h.cfg.CLIProvider,
		Model:          cliModel,
		Agent:          h.cfg.CLIAgent,
		Prompt:         prompt,
		Images:         images,
	})
	if err != nil {
		log.Printf("cli chat failed model=%s: %v", cliModel, err)
		ri.note("upstream_error", "codearts cli: "+truncateMsg(err.Error(), 300))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "codearts cli: "+truncateMsg(err.Error(), 300))
		return
	}

	chatID := convID
	if !validChatID(chatID) {
		chatID = randHex(32)
	}
	w.Header().Set("X-Codearts-Chat-Id", chatID)

	content := res.Text
	finish := "stop"
	var calls []openAIToolCall
	if toolsOn {
		if c, rest, found := toolResponse(res.Text); found {
			kept, dropped := filterKnownCalls(c, req.Tools)
			if len(dropped) > 0 {
				log.Printf("cli dropped unknown tool names model=%s: %v", cliModel, dropped)
			}
			if len(kept) > 0 {
				calls = kept
				content = rest
				finish = "tool_calls"
				assignCallIDs(calls)
			} else {
				// 全是发明出来的工具名：不回 tool_calls（否则客户端执行失败并
				// 死循环），把剥掉调用块后的文本当普通回复。
				content = strings.TrimSpace(rest)
			}
		}
	}
	if req.Stream {
		if len(calls) > 0 {
			streamCLIToolCalls(w, req.Model, calls)
			return
		}
		streamCLIText(w, req.Model, content)
		return
	}
	writeJSON(w, http.StatusOK, buildCompletion(req.Model, "", content, calls, finish, chatID, usageEstimateText(prompt, res.Text)))
}
