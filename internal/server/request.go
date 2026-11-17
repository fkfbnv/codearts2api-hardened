// OpenAI chat.completions 请求解析与会话指纹。
//
// 支持完整 OpenAI 消息模型：system/user/assistant/tool 四种角色、
// string 与分片 content、assistant 的 tool_calls、tool 结果的 tool_call_id。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxUpstreamOutputTokens 上游 InferHub 接受的 max_tokens 上限。
// 超过即 InferHub.001001005.400（2026-11 多个第三方实现一致实测）。
const MaxUpstreamOutputTokens = 65536

// openAIToolCall 一次函数调用（assistant 历史或响应里用）。
type openAIToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串（OpenAI 线格式）
}

// openAIMessage 拍平后的一条消息。
type openAIMessage struct {
	Role             string
	Text             string
	Content          any // 含图片时保留原始 OpenAI 分片，纯文本为 nil
	ReasoningContent string
	ToolCalls        []openAIToolCall
	ToolCallID       string // role=tool 时对应的 call id
	Name             string // role=tool 时的函数名（部分客户端会带）
}

// toolChoiceOpenAI 归一化后的 tool_choice。
type toolChoiceOpenAI struct {
	Mode     string // "" / "none" / "auto" / "required" / "function"
	Function string // Mode=="function" 时指定的函数名
}

// chatRequest 完整请求体。
type chatRequest struct {
	Model           string
	Stream          bool
	IncludeUsage    bool
	ReasoningEffort string
	MaxTokens       *int
	Temperature     *float64
	TopP            *float64
	Messages        []openAIMessage
	Tools           []map[string]any
	ToolChoice      toolChoiceOpenAI
	ConversationID  string
}

// parseChatRequest 解析并校验请求体。
func parseChatRequest(body []byte) (*chatRequest, error) {
	var raw struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		ReasoningEffort     string            `json:"reasoning_effort"`
		MaxTokens           *int              `json:"max_tokens"`
		MaxCompletionTokens *int              `json:"max_completion_tokens"`
		Temperature         *float64          `json:"temperature"`
		TopP                *float64          `json:"top_p"`
		ConversationID      string            `json:"conversation_id"`
		Messages            []json.RawMessage `json:"messages"`
		Tools               []map[string]any  `json:"tools"`
		ToolChoice          json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, errors.New("messages is empty")
	}
	req := &chatRequest{
		Model:           raw.Model,
		Stream:          raw.Stream,
		IncludeUsage:    raw.StreamOptions.IncludeUsage,
		ReasoningEffort: normalizeReasoningEffort(raw.ReasoningEffort),
		MaxTokens:       raw.MaxTokens,
		Temperature:     raw.Temperature,
		TopP:            raw.TopP,
		ConversationID:  raw.ConversationID,
		Tools:           raw.Tools,
		ToolChoice:      parseToolChoice(raw.ToolChoice),
	}
	if raw.MaxCompletionTokens != nil {
		req.MaxTokens = raw.MaxCompletionTokens
	}
	if req.MaxTokens != nil && *req.MaxTokens <= 0 {
		return nil, errors.New("max_tokens must be greater than 0")
	}
	// 上游实测输出上限 65536，超过即 InferHub.001001005.400「param is invalid」。
	// 客户端（含 opencode 之类）常传 131072/262144，直接透传会让一次普通请求
	// 变成参数错误。这里统一封顶，而不是回 400：封顶后请求能正常完成。
	// 另外上游只认 max_tokens，不认 max_completion_tokens——后者已在上面折叠
	// 进 MaxTokens，发往上游的 body 里不会出现（见 client.go chatBodyV2）。
	if req.MaxTokens != nil && *req.MaxTokens > MaxUpstreamOutputTokens {
		v := MaxUpstreamOutputTokens
		req.MaxTokens = &v
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return nil, errors.New("top_p must be between 0 and 1")
	}
	for i, rm := range raw.Messages {
		m, err := parseMessage(rm)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, m)
	}
	// 至少要有一条 user 或 tool 消息（否则没有可发送的内容）。
	hasSendable := false
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "tool" {
			hasSendable = true
			break
		}
	}
	if !hasSendable {
		return nil, errors.New("no user or tool message found")
	}
	return req, nil
}

// normalizeReasoningEffort 把客户端的思考档位归一成上游认识的值。
//
// 上游（InferHub）对「始终思考」的模型只接受 low / high / max。客户端用
// none、off、disabled 之类表示关闭思考时，上游直接回
//   502 InferHub.ModelArts.81001.400 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。
// 这个 502 曾把单账号池整池冷却 10 分钟（2026-11-15 22:23 实测）。关闭思考在这类
// 模型上物理做不到，因此把「关闭」降级成最低档 low；无法识别的档位同样按 low 处理，
// 只有 medium 归到 high（上游不接受 medium）。
func normalizeReasoningEffort(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return ""
	case "low", "high", "max":
		return v
	case "medium":
		return "high"
	default:
		return "low"
	}
}

// parseMessage 解析单条消息：content 支持 string 与 [{type:text,text}] 分片；
// 带图片分片时额外保留原始线格式，交给上游而不是被拍平成纯文本。
func parseMessage(rm json.RawMessage) (openAIMessage, error) {
	var probe struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		Name             string          `json:"name"`
	}
	if err := json.Unmarshal(rm, &probe); err != nil {
		return openAIMessage{}, err
	}
	m := openAIMessage{
		Role:             strings.ToLower(strings.TrimSpace(probe.Role)),
		Text:             flattenContent(probe.Content),
		Content:          imageContentParts(probe.Content),
		ReasoningContent: probe.ReasoningContent,
		ToolCallID:       probe.ToolCallID,
		Name:             probe.Name,
	}
	if len(probe.ToolCalls) > 0 && string(probe.ToolCalls) != "null" {
		var calls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(probe.ToolCalls, &calls); err == nil {
			for _, c := range calls {
				m.ToolCalls = append(m.ToolCalls, openAIToolCall{
					ID:        c.ID,
					Name:      c.Function.Name,
					Arguments: rawJSONString(c.Function.Arguments),
				})
			}
		}
	}
	return m, nil
}

// flattenContent 拍平 string / 分片 content 为纯文本。
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if t, ok := p["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// imageContentParts 在 content 含图片/视频时保留原始 OpenAI 分片。
// 纯文本消息返回 nil，走原来的 Text 路径，避免改动既有行为。
func imageContentParts(raw json.RawMessage) any {
	var parts []map[string]any
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	hasMedia := false
	for _, p := range parts {
		typ, _ := p["type"].(string)
		switch typ {
		case "image_url", "image":
			img := p["image_url"]
			if img == nil {
				img = p["imageUrl"]
			}
			if img == nil {
				img = p["image"]
			}
			// ZCode 的图片分片：{"type":"image","mediaType":"image/png","dataUrl":"data:image/png;base64,..."}
			if img == nil {
				for _, k := range []string{"dataUrl", "url"} {
					if s, ok := p[k].(string); ok && s != "" {
						img = map[string]any{"url": s}
						break
					}
				}
			}
			// 裸 base64：data + mediaType/mimeType
			if img == nil {
				if data, ok := p["data"].(string); ok && data != "" {
					mt, _ := p["mediaType"].(string)
					if mt == "" {
						mt, _ = p["mimeType"].(string)
					}
					if mt == "" {
						mt = "image/png"
					}
					img = map[string]any{"url": "data:" + mt + ";base64," + data}
				}
			}
			// Anthropic 风格：{"type":"image","source":{"type":"base64","media_type":...,"data":...}}
			// 以及 source 里直接带 dataUrl 的变体。
			if img == nil {
				if src, ok := p["source"].(map[string]any); ok {
					if s, ok := src["dataUrl"].(string); ok && s != "" {
						img = map[string]any{"url": s}
					} else if s, ok := src["url"].(string); ok && s != "" {
						img = map[string]any{"url": s}
					} else if data, ok := src["data"].(string); ok && data != "" {
						mt, _ := src["media_type"].(string)
						if mt == "" {
							mt, _ = src["mediaType"].(string)
						}
						if mt == "" {
							mt, _ = src["mimeType"].(string)
						}
						if mt == "" {
							mt = "image/png"
						}
						img = map[string]any{"url": "data:" + mt + ";base64," + data}
					}
				}
			}
			if s, ok := img.(string); ok {
				img = map[string]any{"url": s}
			}
			if img == nil {
				continue
			}
			wire := map[string]any{"type": "image_url", "image_url": img}
			if d, ok := p["detail"].(string); ok {
				wire["detail"] = d
			}
			out = append(out, wire)
			hasMedia = true
		// 视频分片必须转成上游唯一认识的 video_url 形状：
		//   - 上游对 {"type":"video",...} 直接回 502「invalid part type: video」；
		//   - 以前 hasMedia 只由图片置位，纯视频消息会整体退回拍平文本
		//     （kimi 实测回 "I don't see a video attached"）。
		// ZCode 的形状是 {"type":"video","mediaType":"video/mp4","dataUrl":"data:video/mp4;base64,..."}。
		case "video_url", "video", "input_video":
			url := inlineVideoURL(p)
			if url == "" {
				continue
			}
			out = append(out, map[string]any{
				"type":      "video_url",
				"video_url": map[string]any{"url": url},
			})
			hasMedia = true
		case "text", "input_text":
			txt := p["text"]
			if txt == nil {
				txt = p["content"]
			}
			if txt != nil {
				out = append(out, map[string]any{"type": "text", "text": txt})
			}
		default:
			out = append(out, p)
		}
	}
	if !hasMedia {
		return nil
	}
	return out
}

// inlineVideoURL 从各种视频分片形状里取出可用的 URL（data: 或 http）。
// 兼容 OpenAI 的 video_url、ZCode 的 {mediaType,dataUrl}、裸 base64 与
// Anthropic 风格的 source 包装。取不到返回空串，该分片丢弃。
func inlineVideoURL(p map[string]any) string {
	for _, k := range []string{"video_url", "videoUrl", "dataUrl", "url"} {
		switch v := p[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if s, _ := v["url"].(string); s != "" {
				return s
			}
		}
	}
	if data, ok := p["data"].(string); ok && data != "" {
		mt, _ := p["mediaType"].(string)
		if mt == "" {
			mt, _ = p["mimeType"].(string)
		}
		if mt == "" {
			mt = "video/mp4"
		}
		return "data:" + mt + ";base64," + data
	}
	if src, ok := p["source"].(map[string]any); ok {
		for _, k := range []string{"dataUrl", "url"} {
			if s, _ := src[k].(string); s != "" {
				return s
			}
		}
		if data, ok := src["data"].(string); ok && data != "" {
			mt, _ := src["media_type"].(string)
			if mt == "" {
				mt, _ = src["mediaType"].(string)
			}
			if mt == "" {
				mt, _ = src["mimeType"].(string)
			}
			if mt == "" {
				mt = "video/mp4"
			}
			return "data:" + mt + ";base64," + data
		}
	}
	return ""
}

// rawJSONString 把 arguments 统一成 JSON 字符串（兼容字符串与对象两种线格式）。
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "{}"
	}
	out, err := json.Marshal(v) // Go map marshal 按 key 排序，天然归一化
	if err != nil {
		return "{}"
	}
	return string(out)
}

// parseToolChoice 归一化 tool_choice：字符串或 {"type":"function","function":{"name":...}}。
func parseToolChoice(raw json.RawMessage) toolChoiceOpenAI {
	if len(raw) == 0 || string(raw) == "null" {
		return toolChoiceOpenAI{Mode: "auto"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(s) {
		case "none":
			return toolChoiceOpenAI{Mode: "none"}
		case "required":
			return toolChoiceOpenAI{Mode: "required"}
		default:
			return toolChoiceOpenAI{Mode: "auto"}
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return toolChoiceOpenAI{Mode: "function", Function: obj.Function.Name}
	}
	return toolChoiceOpenAI{Mode: "auto"}
}

// ---------------------------------------------------------------------------
// 会话指纹（无状态客户端的历史对齐）
// ---------------------------------------------------------------------------

// canonical 消息归一化串：指纹链的输入。client 回发的 assistant 消息
// 由我们生成（tool_calls id 也是我们发的），因此可逐字节复算。
func canonical(m openAIMessage) string {
	var sb strings.Builder
	sb.WriteString(m.Role)
	sb.WriteString("\x00")
	sb.WriteString(m.Text)
	for _, c := range m.ToolCalls {
		sb.WriteString("\x00")
		sb.WriteString(c.ID)
		sb.WriteString("\x00")
		sb.WriteString(c.Name)
		sb.WriteString("\x00")
		sb.WriteString(normalizeArgs(c.Arguments))
	}
	if m.ToolCallID != "" {
		sb.WriteString("\x00tool:")
		sb.WriteString(m.ToolCallID)
	}
	return sb.String()
}

// normalizeArgs 归一化 arguments（key 排序、去空白）。
func normalizeArgs(args string) string {
	if args == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(out)
}

// chainFingerprint 链式指纹：fp_i = sha256(fp_{i-1} + canonical(m_i))。
func chainFingerprint(prev string, m openAIMessage) string {
	sum := sha256.Sum256([]byte(prev + "\x01" + canonical(m)))
	return hex.EncodeToString(sum[:16])
}

// fingerprintOf 计算整段消息列表的链式指纹。
func fingerprintOf(msgs []openAIMessage) string {
	fp := ""
	for _, m := range msgs {
		fp = chainFingerprint(fp, m)
	}
	return fp
}

// conversationIDFor 为不带会话 ID 的 OpenAI 客户端提供稳定的 CodeArts chat_id。
// Agent 工具链的历史会持续增长，但首条 user 消息不变，因此可作为会话锚点。
func conversationIDFor(req *chatRequest, headerValue string) string {
	for _, explicit := range []string{req.ConversationID, headerValue} {
		explicit = strings.TrimSpace(explicit)
		if explicit == "" {
			continue
		}
		if validChatID(explicit) {
			return strings.ToLower(explicit)
		}
		sum := sha256.Sum256([]byte(explicit))
		return hex.EncodeToString(sum[:16])
	}
	for _, message := range req.Messages {
		if message.Role == "user" {
			anchor := canonical(message)
			if len(req.Tools) > 0 {
				// OpenCode 会用同一用户消息先请求无工具的会话标题，再启动工具 Agent。
				// 两者必须分开，否则标题生成会污染内置 Agent 会话。
				anchor += "\x00tools"
			}
			// 模型也进锚点：同一句「say ok」发给 5 个模型，如果 chat_id 只由首条
			// user 消息决定，5 个模型会共用同一个上游会话（实测五个模型返回的
			// chat_id 全是同一个 7478dcf9…），多模型并发会互相污染 prompt cache
			// 与上下文。显式 conversation_id 仍然优先（那是续聊语义，不该被拆）。
			if m := strings.TrimSpace(req.Model); m != "" && m != "auto" {
				anchor += "\x00model=" + strings.ToLower(m)
			}
			sum := sha256.Sum256([]byte(anchor))
			return hex.EncodeToString(sum[:16])
		}
	}
	return ""
}
