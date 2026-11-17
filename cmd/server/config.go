package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codearts2api/internal/pool"
)

// Config 顶层配置。
type Config struct {
	Listen       string `json:"listen"`
	APIKey       string `json:"api_key"` // config.json 的 api_key 或 env CA2A_API_KEY（同时存在时 env 优先）
	AuthDir      string `json:"auth_dir"`
	StateFile    string `json:"state_file"`
	DefaultModel string `json:"default_model"`
	// OAuthCallbackHost 可选：远程授权时把回调指向公网地址，让浏览器回调经反向
	// 代理进入本服务，而不是 127.0.0.1。只有**显式写了端口**才会改写授权链接里的
	// port 参数（如 https://oneapi.example.com:443/codearts）；不写端口则保持本机
	// 端口不变。远端回调不通时会自动回退到 ticket 轮询通道，不影响登录完成。
	OAuthCallbackHost string `json:"oauth_callback_host,omitempty"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Watch struct {
		Enabled           bool `json:"enabled"`
		PollMinutes       int  `json:"poll_minutes"`
		RefreshSkewM      int  `json:"refresh_skew_minutes"`
		KeepaliveInterval int  `json:"keepalive_interval_minutes"` // 保活心跳间隔（新增）
	} `json:"watch"`

	MaxConcurrent   int    `json:"max_concurrent"`   // 单账号最大并发数（新增）
	KeepaliveWindow string `json:"keepalive_window"` // 保活窗口（新增）

	// LoginClientID WebUI 登录使用的 OAuth client_id（留空用默认 codearts-agent）。
	LoginClientID string `json:"login_client_id"`

	// QueueRetrySeconds 上游并发/TPM 排队时的重试间隔（秒，默认 10）。
	QueueRetrySeconds int `json:"queue_retry_seconds"`
	// QueueMaxAttempts 排队重试次数上限（默认 30，约 5 分钟）。
	QueueMaxAttempts int `json:"queue_max_attempts"`

	// MaxBodyMB 单个 chat 请求体上限（MB，默认 32）。1107 实测 8MB 会在长会话 +
	// 图片 base64 时 413；前置 nginx/网关承受力不同，故可配。
	MaxBodyMB int `json:"max_body_mb"`
	// UpstreamMaxBodyMB 发往上游 chat-completions 的请求体上限（MB，默认 10）。
	// 2026-11-17 实测上游硬上限正好 10MiB：超过回 400 InferHub.001001005，
	// 文案是「参数非法」，与体积无关。网关在 marshal 后按此值预检，超限直接回
	// 413 request_too_large 并说明原因，不再把误导性文案透传给客户端。
	// 调大不会让上游接受更大的请求，只影响本地拦截线。
	UpstreamMaxBodyMB int `json:"upstream_max_body_mb"`
	// SessionHeartbeat 直连路径启用并发会话心跳（默认 true）。上游每账号 3 路
	// 并发、闸门只认 chat-session/heartbeat 的 busy/idle；不开会话槽位泄漏，
	// 连发几个请求就撞 TM.00001041。测试环境可显式置 false。
	//
	// 用 *bool 而不是 bool：bool 分不清「没写」和「写了 false」，而默认值又是
	// true，导致 false 永远关不掉。这个字段还曾经被误写进
	// `if c.MaxBodyMB <= 0` 的块里（缩进错位），本机恰好没设 max_body_mb 才
	// 侥幸生效——一旦有人写 max_body_mb，心跳就会静默关闭。
	SessionHeartbeat *bool `json:"session_heartbeat,omitempty"`
	// CatalogCacheFile 模型目录缓存文件（默认与 state_file 同目录 models-cache.json）。
	// 落盘后重启可先读缓存，不必冷启动就打上游三路发现。
	CatalogCacheFile string `json:"catalog_cache_file"`

	// BenefitAutoClaim 已弃用（2026-11-11 福利通道上游下线）：仅保留配置键兼容，不再有任何效果。
	// 领取是幂等操作（官方客户端打开模型菜单即调用），不领取时福利模型一律
	// 返回 InferHub.4004.200 benefit not found。置 false 可关掉这个写操作。
	BenefitAutoClaim bool `json:"benefit_auto_claim"`
	// CreditsAutoClaim 每次 watchdog 轮询领取每日签到和新手活动积分（默认 true）。
	CreditsAutoClaim bool `json:"credits_auto_claim"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`

	// CLIBackend 官方 CLI 后端（codearts serve / opencode API）。
	// 官方对裸 chat 调用加了传输层加密，直调已用模型一律 TM.000010051；
	// CLI 内部走加密通道可用全部模型，因此把命中 models 的请求转给它。
	CLIBackend struct {
		Enabled         bool              `json:"enabled"`
		Endpoint        string            `json:"endpoint"`
		Provider        string            `json:"provider"`
		Agent           string            `json:"agent"`
		CodeartsExe     string            `json:"codearts_exe"`
		CredentialsFile string            `json:"credentials_file"`
		TimeoutSeconds  int               `json:"timeout_seconds"`
		Models          map[string]string `json:"models"` // 小写 OpenAI ID -> CLI modelID
		// MultimodalModels 是 serve 侧的多模态模型列表（写进 /v1/config 的
		// multimodalModels）。CLI 默认把图片转成 ImageId 交给 analyzeImage 工具，
		// 再由它找"可用的多模态模型"——列表为空时这一步必然失败。把要用的
		// 视觉模型写在这里，analyzeImage 就用它做视觉推理。
		MultimodalModels []string `json:"multimodal_models"`
	} `json:"cli_backend"`

	SoftRateDur    time.Duration
	ErrCooldownDur time.Duration
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7866",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Watch.Enabled = true
	c.Watch.PollMinutes = 30
	c.Watch.RefreshSkewM = 30
	c.Watch.KeepaliveInterval = 15 // 新增：保活间隔 15 分钟
	c.MaxConcurrent = 1            // 上游单账号最多 3 并发会话但释放慢，串行最稳
	c.KeepaliveWindow = "10m"      // 新增：保活窗口 10 分钟
	c.Upstream.TimeoutSeconds = 120
	c.BenefitAutoClaim = true // 已弃用，仅兼容旧配置文件
	c.CreditsAutoClaim = true

	// CLI 后端默认关闭：直连 + 小写模型 ID + tool_stream 已能调通便宜模型，
	// CLI（agent 内核）每次带约 1.4 万 token 的 system prompt，更贵且只出纯文本。
	// 需要时在 config.json 开 cli_backend.enabled 并配好 codearts_exe。
	c.CLIBackend.Enabled = false
	c.CLIBackend.Endpoint = "http://127.0.0.1:4096"
	c.CLIBackend.Provider = "huaweicloud-maas"
	c.CLIBackend.Agent = "build"
	// 官方 CLI 的安装路径因机器而异，默认留空：未配置时 CLI 后端无法启动，
	// 在 config.json 写 cli_backend.codearts_exe，或设置环境变量 CA2A_CLI_EXE。
	c.CLIBackend.CodeartsExe = ""
	c.CLIBackend.CredentialsFile = "./cli-credentials.json"
	c.CLIBackend.TimeoutSeconds = 300
	c.CLIBackend.Models = map[string]string{
		"deepseek-v4.1-flash":      "deepseek-v4.1-flash",
		"deepseek-v4.1-flash-1017": "deepseek-v4.1-flash-1017",
		"glm-5.3":                  "glm-5.3",
		"glm-5.3-flash":            "glm-5.3-flash",
		"kimi-k3":                  "kimi-k3",
		"kimi-k3.1-pro":            "kimi-k3.1-pro",
		"openpangu-2.0-flash":      "openpangu-2.0-flash",
		"openpangu-2.0-pro":        "openpangu-2.0-pro",
	}
	return c
}

// Load 加载配置 + CA2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(stripJSONComments(raw), c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	// 没有 API Key 就拒绝启动：旧版本回退到公开的 dummy-key-for-codearts，
	// 等于把 /v1/chat/completions 与 /v1/models 暴露给任何人（本机也可能被反代出去）。
	if c.APIKey == "" {
		return nil, fmt.Errorf("api_key 未设置：请在 config.json 写 api_key，" +
			"或设置环境变量 CA2A_API_KEY 后再启动（生成示例：openssl rand -hex 24）")
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// stripJSONComments 去掉配置里的 // 与 /* */ 注释。
//
// config.example.json 带注释（字段说明只在那一份里），README 又让人直接
// `cp config.example.json config.json`；标准 encoding/json 不接受注释，
// 因此这里先做一次剥离。字符串内的 // 与 /* 本身不动（oauth_callback_host
// 这类值就是 URL）。
func stripJSONComments(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			out = append(out, ch)
			switch ch {
			case '\\':
				if i+1 < len(raw) {
					i++
					out = append(out, raw[i])
				}
			case '"':
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(raw) {
			if raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
				// 保留换行，尽量不改变 offset。
				if i < len(raw) {
					out = append(out, '\n')
				}
				continue
			}
			if raw[i+1] == '*' {
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++ // 跳过结尾的 '/'
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

func applyEnv(c *Config) {
	if v := os.Getenv("CA2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("CA2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CA2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("CA2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("CA2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("CA2A_OAUTH_CALLBACK_HOST"); v != "" {
		c.OAuthCallbackHost = v
	}
	if v := os.Getenv("CA2A_WATCH_ENABLED"); v != "" {
		c.Watch.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_WATCH_POLL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.PollMinutes = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_REFRESH_SKEW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.RefreshSkewM = n
		}
	}
	if v := os.Getenv("CA2A_WATCH_KEEPALIVE_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Watch.KeepaliveInterval = n
		}
	}
	if v := os.Getenv("CA2A_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrent = n
		}
	}
	if v := os.Getenv("CA2A_KEEPALIVE_WINDOW"); v != "" {
		c.KeepaliveWindow = v
	}
	if v := os.Getenv("CA2A_LOGIN_CLIENT_ID"); v != "" {
		c.LoginClientID = v
	}
	if v := os.Getenv("CA2A_QUEUE_RETRY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueRetrySeconds = n
		}
	}
	if v := os.Getenv("CA2A_QUEUE_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.QueueMaxAttempts = n
		}
	}
	if v := os.Getenv("CA2A_BENEFIT_AUTO_CLAIM"); v != "" {
		c.BenefitAutoClaim = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_CREDITS_AUTO_CLAIM"); v != "" {
		c.CreditsAutoClaim = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("CA2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxBodyMB = n
		}
	}
	if v := os.Getenv("CA2A_CATALOG_CACHE_FILE"); v != "" {
		c.CatalogCacheFile = v
	}
	if v := os.Getenv("CA2A_CLI_EXE"); v != "" {
		c.CLIBackend.CodeartsExe = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7866"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if c.Watch.PollMinutes <= 0 {
		c.Watch.PollMinutes = 30
	}
	if c.Watch.RefreshSkewM <= 0 {
		c.Watch.RefreshSkewM = 30
	}
	if c.Watch.KeepaliveInterval <= 0 {
		c.Watch.KeepaliveInterval = 15 // 默认保活间隔 15 分钟
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 1 // 上游并发会话释放慢，单账号串行最稳
	}
	if c.MaxBodyMB <= 0 {
		c.MaxBodyMB = 32
	}
	if c.UpstreamMaxBodyMB <= 0 {
		c.UpstreamMaxBodyMB = 10
	}
	// 心跳默认开。必须与 MaxBodyMB 判断彻底分开：这一行曾经被缩进到上面的
	// `if c.MaxBodyMB <= 0` 块里，写 max_body_mb 就会静默关掉并发闸。
	if c.SessionHeartbeat == nil {
		on := true
		c.SessionHeartbeat = &on
	}
	if c.CatalogCacheFile == "" {
		dir := filepath.Dir(c.StateFile)
		if dir == "" || dir == "." {
			dir = "."
		}
		c.CatalogCacheFile = filepath.Join(dir, "models-cache.json")
	}
	return nil
}

// ToPoolConfig 转换为 pool.Config。
func (c *Config) ToPoolConfig() pool.Config {
	kw := c.KeepaliveWindow
	if kw == "" {
		kw = "10m"
	}
	dur, err := time.ParseDuration(kw)
	if err != nil {
		dur = 10 * time.Minute
	}
	return pool.Config{
		ErrThreshold:       c.Cooldown.ErrThresh,
		ErrCooldown:        c.ErrCooldownDur,
		SoftCooldown:       c.SoftRateDur,
		RefreshSkew:        time.Duration(c.Watch.RefreshSkewM) * time.Minute,
		MaxConcurrent:      c.MaxConcurrent,
		KeepaliveWindow:    dur,
		ChatBodyLimitBytes: int64(c.UpstreamMaxBodyMB) << 20,
	}
}
