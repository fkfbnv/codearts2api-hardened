// 并发会话心跳：上游对每个账号只放 3 路并发对话，且闸门不认「请求结束」、
// 只认 PUT /snap-manager/v1/chat-session/heartbeat?status=busy|idle 记账——
// 对话前 busy 登记 user-session-id（对话请求带同一个头），结束 idle 注销。
// 不做这套，连续快发几个请求就稳定撞 TM.00001041（HTTP 400）。
//
// 实现照 Agent2Providers session.rs / NextTokenCenter wb_codearts_wire 的
// 五条硬规则（各有测试）：
//  1. 周期性 busy 续期失败不能释放槽位——一次心跳超时 ≠ 对话结束；
//  2. 首次 busy 非 200 也要注销自己刚造的 id（传输失败可能发生在上游占槽之后）；
//  3. 只注销自己造的 id——调用方传来的 session id 一律不用；
//  4. idle 恰好一次，且在最后一次 busy 完成之后（同一 goroutine 顺序保证）；
//  5. 上层把「抢不到会话」当请求级拥塞回 409，不冷却健康账号。
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// EpChatSessionHeartbeat 并发会话心跳端点（相对 snap-manager）。
	EpChatSessionHeartbeat = "/snap-manager/v1/chat-session/heartbeat"
	// heartbeatInterval busy 续期间隔；上游槽位默认租期约 60s，30s 续一次足够。
	heartbeatInterval = 30 * time.Second
	// heartbeatTimeout 单次心跳请求超时。
	heartbeatTimeout = 30 * time.Second
)

// ChatSession 一次成功开始的并发会话。用完必须 Stop（否则槽位挂着直到租期过期）。
type ChatSession struct {
	client *Client
	cred   SignCredential
	id     string
	stop   chan struct{}
	done   chan struct{}
}

// BeginChatSession 向上游登记 busy 会话。失败时先尽力 idle 自己刚造的 id
// 再把错误交回去（规则 2），调用方应视为「该账号并发已满」。
func (c *Client) BeginChatSession(ctx context.Context, cred SignCredential) (*ChatSession, error) {
	s := &ChatSession{
		client: c,
		cred:   SignCredential{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.SecretAccessKey, SecurityToken: cred.SecurityToken},
		id:     randHexN(32),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if err := s.beat(ctx, "busy"); err != nil {
		_ = s.beat(context.Background(), "idle")
		return nil, err
	}
	go s.keepalive()
	return s, nil
}

// ID 返回会话 id（对话请求要带同一个 User-Session-Id 头）。
func (s *ChatSession) ID() string { return s.id }

// Stop 停止续期并等 idle 真正发出（在途 busy 先落地，规则 4）。幂等；nil 安全。
func (s *ChatSession) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stop:
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
		return
	default:
	}
	close(s.stop)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
	}
}

// keepalive 每 30s 续一次 busy；收到 stop 后发 idle 恰好一次再退出。
// busy 与 idle 在同一 goroutine 里顺序执行，天然满足规则 4。
func (s *ChatSession) keepalive() {
	defer close(s.done)
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
			defer cancel()
			if err := s.beat(ctx, "idle"); err != nil {
				// 客户端可能已断开，idle 发不出去只能记账，没有别的补救。
				Log("chat-session idle 未被确认: %v", err)
			}
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
			// 规则 1：续期失败 ≠ 对话结束，槽位保持，等 Stop 的 idle 收尾。
			if err := s.beat(ctx, "busy"); err != nil {
				Log("chat-session busy 续期未确认（槽位保持）: %v", err)
			}
			_ = cancel
		}
	}
}

// beat 发一次心跳：PUT 端点 + {} 体，头族照官方客户端（x-client-type: kernel）。
// 签名带 Host——signRequest 恒定包含 Host，与该内部端点的官方签名一致。
func (s *ChatSession) beat(ctx context.Context, status string) error {
	endpoint := s.client.snapManagerBase + EpChatSessionHeartbeat + "?status=" + status
	body := []byte("{}")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Language", "zh-cn")
	req.Header.Set("x-client-type", "kernel")
	req.Header.Set("user-session-id", s.id)
	req.Header.Set("x-snap-traceid", randHexN(32))
	signRequest(req, body, s.cred)
	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("chat-session %s transport: %w", status, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chat-session %s rejected: HTTP %d %s", status, resp.StatusCode, truncateStr(string(raw), 160))
	}
	return nil
}
