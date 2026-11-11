package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// promptFromMessages 把 OpenAI 消息序列拍成 CLI 能理解的单段 prompt。
func promptFromMessages(msgs []openAIMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Text == "" && m.Content == nil && len(m.ToolCalls) == 0 {
			continue
		}
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
		// CLI 通道带不了图片（prompt 是纯文本）。以前图片被静默丢弃，模型
		// 看到用户在谈截图却收不到图，就会去找/发明读图工具（实测复现：
		// LocalToolSearch{"query":"select:analyzeImage"} → 连环执行失败）。
		// 显式声明看不到图，让它直接用文本回答。
		if m.Content != nil {
			sb.WriteString("[图片：用户发送了图片附件，本通道无法传图，你看不到图片内容。请基于已有文字直接回答，或用文字向用户说明需要贴出文字内容；不要寻找或调用读图工具。]\n")
		}
		for _, c := range m.ToolCalls {
			sb.WriteString("调用工具 " + c.Name + "(" + c.Arguments + ")\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
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
