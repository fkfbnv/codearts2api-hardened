package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/cli"
)

// CLI 通道的图片附件上限。超过时在 prompt 里明说「未发送」，不静默丢图
// （静默丢图会让模型去找读图工具，历史上因此连环翻车）。
const (
	cliMaxImages     = 8
	cliMaxImageBytes = 12 << 20 // 单张 data URL 字符数
	cliMaxImageTotal = 24 << 20
)

// promptAndImages 把 OpenAI 消息序列拍成 CLI 能理解的单段 prompt，同时抽出
// 可以走 opencode file 分片直传的图片。
//
// 拍平文本与图片附件是同一条会话消息的两部分：文本进 prompt，图片按
// `{type:"file", mime, filename, url}` 附在同一个 POST /message 上（opencode
// 的 file part 认 data: URL）。Image.Offset 记录图片所属消息在 prompt 中的
// 起始偏移，供客户端做增量过滤。
func promptAndImages(msgs []openAIMessage) (string, []cli.Image) {
	var sb strings.Builder
	var images []cli.Image
	imgNo, attach, total := 0, 0, 0

	for _, m := range msgs {
		if m.Text == "" && m.Content == nil && len(m.ToolCalls) == 0 {
			continue
		}
		start := sb.Len()
		switch m.Role {
		case "system":
			sb.WriteString("[系统]\n")
		case "assistant":
			sb.WriteString("[助手]\n")
		case "tool":
			sb.WriteString("[工具结果]\n")
		default:
			sb.WriteString("[用户]\n")
		}
		if m.Text != "" {
			sb.WriteString(m.Text)
			if !strings.HasSuffix(m.Text, "\n") {
				sb.WriteString("\n")
			}
		}
		for _, p := range messageParts(m.Content) {
			typ, _ := p["type"].(string)
			switch typ {
			case "image_url", "image":
				imgNo++
				mime, url, ok := imageDataURL(p)
				if !ok {
					fmt.Fprintf(&sb, "[图片 %d：不是可内联发送的图片（需要 data: URL），本通道未发送。]\n", imgNo)
					continue
				}
				if attach >= cliMaxImages {
					fmt.Fprintf(&sb, "[图片 %d：超出单次附件数量上限（%d 张），未发送。]\n", imgNo, cliMaxImages)
					continue
				}
				if len(url) > cliMaxImageBytes || total+len(url) > cliMaxImageTotal {
					fmt.Fprintf(&sb, "[图片 %d：超出附件大小上限，未发送；请要求用户压缩后再发。]\n", imgNo)
					continue
				}
				attach++
				total += len(url)
				images = append(images, cli.Image{
					MIME:     mime,
					Filename: imageFilename(mime, attach),
					URL:      url,
					Offset:   start,
				})
				fmt.Fprintf(&sb, "[图片 %d：已随本消息以附件发送。]\n", imgNo)
			case "video_url", "video", "input_video":
				sb.WriteString("[视频：本通道无法接收视频，未发送；请用文字描述关键画面。]\n")
			}
		}
		for _, c := range m.ToolCalls {
			sb.WriteString("调用工具 " + c.Name + "(" + c.Arguments + ")\n")
		}
		sb.WriteString("\n")
	}
	return sb.String(), images
}

// messageParts 取消息的原始分片数组（解析阶段存的是 []map[string]any）。
func messageParts(v any) []map[string]any {
	if v == nil {
		return nil
	}
	if parts, ok := v.([]map[string]any); ok {
		return parts
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	return parts
}

// imageDataURL 从各种图片分片形状里取出内联 data: URL 与 MIME。
// 覆盖 OpenAI image_url、ZCode {mediaType,dataUrl}、裸 base64、
// Anthropic source 包装；http(s) 远程图返回 ok=false（CLI 通道不代下载）。
func imageDataURL(p map[string]any) (mime, url string, ok bool) {
	url = imageURL(p)
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	mime = dataURLMime(url)
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		return "", "", false
	}
	return mime, url, true
}

func imageURL(p map[string]any) string {
	for _, k := range []string{"image_url", "imageUrl", "image"} {
		switch v := p[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			for _, kk := range []string{"url", "dataUrl"} {
				if s, _ := v[kk].(string); s != "" {
					return s
				}
			}
		}
	}
	for _, k := range []string{"dataUrl", "url"} {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	if data, ok := p["data"].(string); ok && data != "" {
		return "data:" + firstString(p["mediaType"], p["mimeType"], "image/png") + ";base64," + data
	}
	if src, ok := p["source"].(map[string]any); ok {
		for _, k := range []string{"dataUrl", "url"} {
			if s, _ := src[k].(string); s != "" {
				return s
			}
		}
		if data, ok := src["data"].(string); ok && data != "" {
			return "data:" + firstString(src["media_type"], src["mediaType"], src["mimeType"], "image/png") + ";base64," + data
		}
	}
	return ""
}

func firstString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// dataURLMime 取 data: URL 的媒体类型并归一（image/jpg → image/jpeg）。
func dataURLMime(u string) string {
	rest := strings.TrimPrefix(u, "data:")
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	} else if i := strings.IndexByte(rest, ','); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)
	if strings.EqualFold(rest, "image/jpg") {
		rest = "image/jpeg"
	}
	return rest
}

func imageFilename(mime string, n int) string {
	ext := ".png"
	switch strings.ToLower(mime) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "image/gif":
		ext = ".gif"
	}
	return fmt.Sprintf("image-%d%s", n, ext)
}

// streamCLIText 把完整文本按块伪流式返回（CLI 这条路径不提供增量事件）。
func streamCLIText(w http.ResponseWriter, model, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	send := func(delta map[string]any, fin string) bool {
		choice := map[string]any{"index": 0, "delta": delta}
		if fin != "" {
			choice["finish_reason"] = fin
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}

	if !send(map[string]any{"role": "assistant"}, "") {
		return
	}
	runes := []rune(text)
	const step = 32
	for i := 0; i < len(runes); i += step {
		end := i + step
		if end > len(runes) {
			end = len(runes)
		}
		if !send(map[string]any{"content": string(runes[i:end])}, "") {
			return
		}
	}
	send(map[string]any{}, "stop")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// usageEstimateText 粗略估算 token（CLI 路径拿不到可信 usage）。
func usageEstimateText(prompt, out string) map[string]any {
	pt := len([]rune(prompt)) / 2
	if pt < 1 {
		pt = 1
	}
	ot := len([]rune(out)) / 2
	if ot < 1 {
		ot = 1
	}
	return map[string]any{
		"prompt_tokens":     pt,
		"completion_tokens": ot,
		"total_tokens":      pt + ot,
	}
}

// toolPrompt 把 OpenAI 工具 schema 渲染进 prompt，并要求固定输出格式。
//
// 背景：官方 chat 接口现在对请求体做 SM4-GCM 加密（CLI 抓包：X-Encrypt-Algo:
// sm4-gcm + X-Encrypt-Key-Id），裸调无法复刻，只能走 CLI；而 CLI 是 agent 内核，
// 不返回 OpenAI tool_calls。官方自己对 DSML 系模型也是「不发 tools，把 schema 注入
// system 提示」，这里沿用它，再由 toolResponse 把模型输出解析回标准 tool_calls。
func toolPrompt(userPrompt string, tools []map[string]any, choice toolChoiceOpenAI) string {
	raw, _ := json.Marshal(tools)
	var sb strings.Builder
	sb.WriteString("【工具使用规则】可用工具（OpenAI function schema）：\n")
	sb.Write(raw)
	sb.WriteString("\n\n不要自己执行工具，不要读写文件、不要运行命令。需要调用工具时，只输出一个代码块：\n")
	sb.WriteString("```tool_call\n{\"name\":\"工具名\",\"arguments\":{...}}\n```\n")
	sb.WriteString("可以输出多个代码块。不需要工具时直接回答，不要输出代码块。\n")
	sb.WriteString("只能调用上面列出的工具，工具名必须与列表一字不差（区分大小写）。列表里没有的工具（例如图像识别）一律不要发明、不要猜测近似名字，直接用文本向用户说明。\n")
	if choice.Mode == "required" {
		sb.WriteString("本轮必须调用工具。\n")
	}
	if choice.Mode == "function" && choice.Function != "" {
		sb.WriteString("本轮必须调用工具 " + choice.Function + "。\n")
	}
	sb.WriteString("\n")
	sb.WriteString(userPrompt)
	return sb.String()
}

// streamCLIToolCalls 以标准 OpenAI 流式格式返回工具调用（一次性给完整参数）。
func streamCLIToolCalls(w http.ResponseWriter, model string, calls []openAIToolCall) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	write := func(delta map[string]any, fin string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if fin != "" {
			choice["finish_reason"] = fin
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}

	write(map[string]any{"role": "assistant"}, "")
	wire := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		wire = append(wire, map[string]any{
			"index": i, "id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
		})
	}
	write(map[string]any{"tool_calls": wire}, "")
	write(map[string]any{}, "tool_calls")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
