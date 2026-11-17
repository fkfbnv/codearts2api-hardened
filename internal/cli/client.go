// Package cli 把官方码道 CLI（codearts serve，opencode 内核）当作模型后端。
//
// 为什么需要它：官方对 snap-access 的裸 chat/completions 调用做了传输层加密
// （CLI 日志 "Global fetch replaced with encrypting fetch"），用 OAuth 的 STS
// 或永久 AK/SK 直调都会拿到 TM.000010051「无权调用该模型」；而 CLI 内部走加密
// 通道，11 个模型都能正常用。因此反代不再复刻请求，而是把 CLI 的 HTTP API 当作
// 上游——CLI 负责鉴权与加密，我们只做 OpenAI ⇄ opencode 的协议转换。
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client 访问 codearts serve（opencode API）。
type Client struct {
	base string
	http *http.Client

	mu       sync.Mutex
	sessions map[string]string // conversationID -> CLI session id
	// lastPrompt 记录每个会话上次发过的完整拍平 prompt。CLI session 自己维护
	// 历史，而客户端每轮都重发全量 messages；不裁剪的话上下文每轮翻倍
	// （token 双倍烧、模型看到重复历史容易胡来）。下轮 prompt 若以上次为
	// 前缀，就只发增量。
	lastPrompt map[string]string
}

// New 构造客户端。base 例如 http://127.0.0.1:4096。
func New(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &Client{
		base:       strings.TrimRight(base, "/"),
		http:       &http.Client{Timeout: timeout},
		sessions:   map[string]string{},
		lastPrompt: map[string]string{},
	}
}

// Healthy 检查 serve 是否在线。
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/session", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK
}

// SetMultimodalModels 把多模态模型列表写进 serve 的全局配置（PATCH /v1/config）。
//
// 为什么必须这么做：CLI 的 ImageAnalyzePlugin 默认把图片换成 ImageId，交给
// analyzeImage 工具，由它再找一个"多模态模型"；这个列表为空时工具直接报
// 「未获取到可用的多模态模型」，图片等于白传。列表里写了某个模型后，
// analyzeImage 就用它做视觉推理（2026-11-17 实测：kimi-k3.1-pro 能读出图中文字）。
func (c *Client) SetMultimodalModels(ctx context.Context, models []string) error {
	if len(models) == 0 {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"multimodalModels": models})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.base+"/v1/config", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("patch /v1/config %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return nil
}

// ChatRequest 一次对话请求。
type ChatRequest struct {
	ConversationID string
	Provider       string
	Model          string
	Agent          string
	Prompt         string
	// Images 是随本轮 prompt 一起发送的图片附件（data: URL）。
	//
	// Offset 记录该图片所属消息在完整 prompt 里的起始字节偏移。会话复用时
	// 客户端只发增量文本，图片也按同一把尺子过滤：只发 Offset 落在增量区间
	// 里的，避免每轮把历史图片重传一遍（token 与带宽都会翻倍）。
	Images []Image
}

// Image 一张以 data: URL 内联的图片附件（opencode file part）。
type Image struct {
	MIME     string
	Filename string
	URL      string
	Offset   int
}

// ChatResult 一次对话结果。
type ChatResult struct {
	Text     string
	ModelID  string
	Provider string
	Parts    []Part
}

// Part 消息片段（只保留有意义的字段）。
type Part struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Chat 发送一条消息并等待完整回复。
//
// 会话复用：conversationID 非空且已有 CLI session 时只发送本轮内容，历史由 CLI
// 维护；否则新建 session 并把完整上下文作为一条 prompt 发送（调用方负责拼装）。
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	sid, _ := c.session(req.ConversationID)
	prompt := req.Prompt
	images := req.Images
	if sid != "" {
		// 已有 CLI session：尝试只发增量。历史被改写（前缀不匹配，例如工具
		// 集变化或客户端重排消息）时弃旧会话、按全量重建。
		c.mu.Lock()
		prev, ok := c.lastPrompt[req.ConversationID]
		c.mu.Unlock()
		if ok && strings.HasPrefix(req.Prompt, prev) {
			cutoff := len(prev)
			if delta := strings.TrimSpace(req.Prompt[cutoff:]); delta != "" {
				prompt = "\n" + delta
			}
			// 图片跟着文本走同一条增量边界：只发本轮新增消息里的图。
			images = imagesFrom(images, cutoff)
		} else if ok {
			c.dropSession(req.ConversationID)
			sid = ""
			prompt = req.Prompt
		}
	}
	if sid == "" {
		var err error
		sid, err = c.createSession(ctx, req.ConversationID)
		if err != nil {
			return nil, err
		}
	}

	parts := make([]any, 0, 1+len(images))
	parts = append(parts, map[string]any{"type": "text", "text": prompt})
	for _, img := range uniqueImages(images) {
		parts = append(parts, map[string]any{
			"type":     "file",
			"mime":     img.MIME,
			"filename": img.Filename,
			"url":      img.URL,
		})
	}
	body := map[string]any{
		"model": map[string]any{"providerID": req.Provider, "modelID": req.Model},
		"parts": parts,
	}
	if req.Agent != "" {
		body["agent"] = req.Agent
	}
	raw, _ := json.Marshal(body)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/session/"+sid+"/message", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		// 会话可能已被 CLI 回收：清掉映射，让下次重建（连带 lastPrompt，
		// 否则增量会发进一个没有历史的全新 session）。
		c.dropSession(req.ConversationID)
		return nil, err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		c.dropSession(req.ConversationID)
		return nil, fmt.Errorf("codearts serve %d: %s", resp.StatusCode, truncate(string(reply), 300))
	}
	c.mu.Lock()
	c.lastPrompt[req.ConversationID] = req.Prompt
	c.mu.Unlock()

	var parsed struct {
		Info struct {
			ModelID    string `json:"modelID"`
			ProviderID string `json:"providerID"`
		} `json:"info"`
		Parts []Part `json:"parts"`
	}
	if err := json.Unmarshal(reply, &parsed); err != nil {
		return nil, fmt.Errorf("parse serve reply: %w", err)
	}
	var sb strings.Builder
	for _, p := range parsed.Parts {
		if p.Type == "text" && p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return &ChatResult{
		Text:     sb.String(),
		ModelID:  parsed.Info.ModelID,
		Provider: parsed.Info.ProviderID,
		Parts:    parsed.Parts,
	}, nil
}

func (c *Client) createSession(ctx context.Context, conversationID string) (string, error) {
	body := map[string]any{"title": "codearts2api"}
	raw, _ := json.Marshal(body)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/session", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create session %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("create session: bad reply %s", truncate(string(data), 200))
	}
	if conversationID != "" {
		c.mu.Lock()
		c.sessions[conversationID] = out.ID
		c.mu.Unlock()
	}
	return out.ID, nil
}

func (c *Client) session(conversationID string) (string, bool) {
	if conversationID == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sid, ok := c.sessions[conversationID]
	return sid, ok
}

func (c *Client) dropSession(conversationID string) {
	if conversationID == "" {
		return
	}
	c.mu.Lock()
	delete(c.sessions, conversationID)
	delete(c.lastPrompt, conversationID)
	c.mu.Unlock()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// imagesFrom 取增量区间内的图片（Offset >= cutoff）。
func imagesFrom(images []Image, cutoff int) []Image {
	out := make([]Image, 0, len(images))
	for _, img := range images {
		if img.Offset >= cutoff {
			out = append(out, img)
		}
	}
	return out
}

// uniqueImages 同一请求里重复的内联图片只发一次（客户端常把同图贴两次）。
func uniqueImages(images []Image) []Image {
	if len(images) < 2 {
		return images
	}
	seen := make(map[string]struct{}, len(images))
	out := make([]Image, 0, len(images))
	for _, img := range images {
		if _, ok := seen[img.URL]; ok {
			continue
		}
		seen[img.URL] = struct{}{}
		out = append(out, img)
	}
	return out
}
