package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServeConfig codearts serve 的启动参数。
type ServeConfig struct {
	Exe         string
	Endpoint    string
	Credentials string // 含 {"ak","sk"} 的 JSON 文件
	LogPath     string // 子进程 stdout/stderr 落盘
}

type credentials struct {
	AK string `json:"ak"`
	SK string `json:"sk"`
}

// EnsureServe 确保 codearts serve 在线：已在线直接返回；否则读取 AK/SK 启动它，
// 并轮询到就绪（最多 40s）。CLI 未安装时返回错误，调用方据此退回直连路径。
func EnsureServe(ctx context.Context, cfg ServeConfig) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("cli endpoint not configured")
	}
	c := New(cfg.Endpoint, 10*time.Second)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ok := c.Healthy(probeCtx)
	cancel()
	if ok {
		return nil
	}
	if cfg.Exe == "" {
		return fmt.Errorf("codearts cli 路径未配置：请在 config.json 写 cli_backend.codearts_exe，" +
			"或设置环境变量 CA2A_CLI_EXE")
	}
	if _, err := os.Stat(cfg.Exe); err != nil {
		return fmt.Errorf("codearts cli not found at %s: %w", cfg.Exe, err)
	}
	cred, err := loadCredentials(cfg.Credentials)
	if err != nil {
		return err
	}

	env := append(os.Environ(), "CODEARTS_CLI_AK="+cred.AK, "CODEARTS_CLI_SK="+cred.SK)
	logPath := cfg.LogPath
	if logPath == "" {
		logPath = filepath.Join(os.TempDir(), "codearts-serve.log")
	}
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open cli log: %w", err)
	}

	port := portFromEndpoint(cfg.Endpoint)
	args := []string{"serve", "--port", port}
	if err := startProcess(cfg.Exe, args, env, logFile, logFile); err != nil {
		logFile.Close()
		return fmt.Errorf("start codearts serve: %w", err)
	}

	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if c.Healthy(ctx) {
			return nil
		}
	}
	return fmt.Errorf("codearts serve did not become ready on %s (log: %s)", cfg.Endpoint, logPath)
}

func loadCredentials(path string) (credentials, error) {
	var c credentials
	if path == "" {
		return c, fmt.Errorf("cli credentials file not configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read cli credentials: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse cli credentials: %w", err)
	}
	if c.AK == "" || c.SK == "" {
		return c, fmt.Errorf("cli credentials missing ak/sk")
	}
	return c, nil
}

func portFromEndpoint(endpoint string) string {
	// endpoint 形如 http://127.0.0.1:4096
	if i := strings.LastIndex(endpoint, ":"); i >= 0 && i+1 < len(endpoint) {
		p := strings.TrimRight(endpoint[i+1:], "/")
		if p != "" {
			return p
		}
	}
	return "4096"
}
