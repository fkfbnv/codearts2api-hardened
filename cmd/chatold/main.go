// chatold 诊断工具：用官方 IDE 的旧聊天端点 /v1/chat/chat 发一条最小请求，
// 对比 /api/v2/chat/completions 的模型权限结果（只发一次，消耗 <1 积分）。
//
// 用法：chatold -auth-dir E:/网关/codearts2api/auths -model DeepSeek-V4.1-Flash-1017 -user fkfbnv144
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	model := flag.String("model", "DeepSeek-V4.1-Flash-1017", "model id")
	user := flag.String("user", "", "user_id (username) to send in body")
	endpoint := flag.String("endpoint", "", "chat endpoint (empty = default for mode)")
	agentType := flag.String("agent-type", "PromptCenter", "Agent-Type header (old endpoint requires it)")
	mode := flag.String("mode", "legacy", "legacy (old endpoint) or v2")
	benefit := flag.Bool("benefit", false, "send maas_type: benefit (v2 mode)")
	agentID := flag.String("agent-id", "", "agent_id to include in v2 body")
	otUID := flag.String("ot-uid", "", "OpenThrottle session user id (adds x-ot-session-id/user-session-id)")
	client := flag.String("client", "", "client/task fields (IDE)")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}
	acct := auths[0]

	ep := *endpoint
	if ep == "" {
		if *mode == "v2" {
			ep = "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions"
		} else {
			ep = "https://snap-access.cn-north-4.myhuaweicloud.com/v1/chat/chat"
		}
	}
	c := upstream.NewWithChatEndpoint(120*time.Second, ep)
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	chatID := hex.EncodeToString(buf)

	body := map[string]any{
		"chat_id": chatID,
		"client":  "IDE",
		"task":    "chat",
		"messages": []any{
			map[string]any{"type": "text", "text": "hi"},
		},
		"task_parameters":          map[string]any{"ide": "CodeArts Agent"},
		"batch_task_parameters":    []any{},
		"attempt":                  1,
		"not_allow_external_model": true,
	}
	if *user != "" {
		body["user_id"] = *user
	}
	body["model"] = *model

	ak, sk := acct.AccessKeyID, acct.SecretAccessKey
	tok := acct.CloudDragonTok
	// 允许用环境变量覆盖为永久 AK/SK（CLI 授权用的那一对），用于对比凭据类型的影响。
	if v := os.Getenv("CODEARTS_CLI_AK"); v != "" {
		ak = v
	}
	if v := os.Getenv("CODEARTS_CLI_SK"); v != "" {
		sk = v
		tok = "" // 永久 AK/SK 不带 security token
	}
	cred := upstream.SignCredential{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   tok,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Printf("mode=%s endpoint=%s model=%s chat_id=%s benefit=%v\n", *mode, ep, *model, chatID, *benefit)
	var rc io.ReadCloser
	var err2 error
	if *mode == "v2" {
		v2 := map[string]any{
			"model": *model, "stream": true,
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"prompt_cache_key": chatID, "tool_stream": true, "chat_id": chatID,
		}
		if *user != "" {
			v2["user_id"] = *user
		}
		if *agentID != "" {
			v2["agent_id"] = *agentID
		}
		if *client != "" {
			v2["client"] = *client
			v2["task"] = "chat"
			v2["not_allow_external_model"] = true
			v2["task_parameters"] = map[string]any{"ide": "CodeArts Agent"}
		}
		if *otUID != "" {
			rc, err2 = c.SendChatV2OT(ctx, v2, "diag", cred, tok, *otUID, *benefit)
		} else {
			rc, err2 = c.SendChatV2(ctx, v2, "diag", cred, tok, *benefit)
		}
	} else {
		rc, err2 = c.SendChatLegacy(ctx, body, "diag", cred, acct.CloudDragonTok, *agentType)
	}
	err = err2
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	defer rc.Close()
	raw, _ := io.ReadAll(io.LimitReader(rc, 8192))
	fmt.Printf("RESPONSE:\n%s\n", string(raw))
}
