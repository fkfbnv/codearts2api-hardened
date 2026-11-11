package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/cli"
	"codearts2api/internal/pool"
	"codearts2api/internal/scheduler"
	"codearts2api/internal/server"
	"codearts2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("WARNING: no accounts found. Run `codearts2api login` first.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	p, err := pool.New(auths, cfg.ToPoolConfig(), cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}

	c := upstream.New(120 * time.Second)
	c.SetBenefitAutoClaim(cfg.BenefitAutoClaim)

	// 模型目录落盘缓存：重启后先用磁盘里的目录，过期由后台单飞刷新。
	upstream.SetCatalogCacheFile(cfg.CatalogCacheFile)
	if err := upstream.LoadCatalogCache(cfg.CatalogCacheFile); err != nil {
		if os.IsNotExist(err) {
			log.Printf("catalog cache: none yet (%s)", cfg.CatalogCacheFile)
		} else {
			log.Printf("catalog cache: load failed: %v", err)
		}
	} else {
		log.Printf("catalog cache loaded from %s", cfg.CatalogCacheFile)
	}

	// CLI 后端：确保 codearts serve 在线（不在线就用 AK/SK 拉起）。
	var cliClient *cli.Client
	if cfg.CLIBackend.Enabled {
		serveCfg := cli.ServeConfig{
			Exe:         cfg.CLIBackend.CodeartsExe,
			Endpoint:    cfg.CLIBackend.Endpoint,
			Credentials: cfg.CLIBackend.CredentialsFile,
			LogPath:     cfg.StateFile + ".cli-serve.log",
		}
		if err := cli.EnsureServe(context.Background(), serveCfg); err != nil {
			log.Printf("cli backend unavailable: %v (falling back to direct models only)", err)
		} else {
			timeout := time.Duration(cfg.CLIBackend.TimeoutSeconds) * time.Second
			cliClient = cli.New(cfg.CLIBackend.Endpoint, timeout)
			log.Printf("cli backend ready: %s provider=%s models=%d",
				cfg.CLIBackend.Endpoint, cfg.CLIBackend.Provider, len(cfg.CLIBackend.Models))
		}
	}

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		CreditsAutoClaim:  cfg.CreditsAutoClaim,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     c,
		APIKey:       cfg.APIKey,
		MaxRotate:    3,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
		QueueRetryDelay: func() time.Duration {
			if cfg.QueueRetrySeconds > 0 {
				return time.Duration(cfg.QueueRetrySeconds) * time.Second
			}
			return 10 * time.Second
		}(),
		QueueMaxAttempts: cfg.QueueMaxAttempts,
		MaxBodyBytes:     int64(cfg.MaxBodyMB) << 20,
		ConvStateFile:    cfg.StateFile + ".chats.json",
		WatchInfo: map[string]any{
			"enabled":              cfg.Watch.Enabled,
			"credits_auto_claim":   cfg.CreditsAutoClaim,
			"poll_minutes":         cfg.Watch.PollMinutes,
			"refresh_skew_minutes": cfg.Watch.RefreshSkewM,
			"keepalive_interval":   cfg.Watch.KeepaliveInterval,
			"max_concurrent":       cfg.MaxConcurrent,
			"keepalive_window":     "10m",
		},
		AuthDir:           cfg.AuthDir,
		Listen:            cfg.Listen,
		OAuthClient:       upstream.New(15 * time.Second),
		LoginConfig:       loginConfig(cfg),
		OAuthCallbackHost: cfg.OAuthCallbackHost,
		CLI:               cliClient,
		CLIProvider:       cfg.CLIBackend.Provider,
		CLIAgent:          cfg.CLIBackend.Agent,
		CLIModels:         cfg.CLIBackend.Models,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	h.StartAvailabilityProber(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("codearts2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// loginConfig 决定 WebUI 登录使用的 OAuth 配置。
//
// client_id 优先取已有账号记录的取值（refresh_token 与 client_id 绑定，混用会让
// 老账号刷新失败），其次取配置项，最后才是内置默认值。其余字段必须来自
// DefaultLoginConfig（否则 authorize 链接会缺 host / plugin 信息）。
func loginConfig(cfg *Config) upstream.LoginConfig {
	lc := upstream.DefaultLoginConfig()
	if cfg.LoginClientID != "" {
		lc.ClientID = cfg.LoginClientID
	}
	if auths, err := auth.LoadDir(cfg.AuthDir); err == nil {
		for _, a := range auths {
			if id := a.ClientIDOr(""); id != "" {
				lc.ClientID = id
				break
			}
		}
	}
	return lc
}
