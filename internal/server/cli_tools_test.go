package server

import (
	"strings"
	"testing"
)

func TestFilterKnownCalls(t *testing.T) {
	tools := []map[string]any{
		{"function": map[string]any{"name": "Bash"}},
		{"function": map[string]any{"name": "Read"}},
	}
	calls := []openAIToolCall{
		{Name: "Bash", Arguments: `{}`},
		{Name: "Analyzelmage", Arguments: `{}`},
		{Name: "Tool_describe", Arguments: `{}`},
	}
	kept, dropped := filterKnownCalls(calls, tools)
	if len(kept) != 1 || kept[0].Name != "Bash" {
		t.Fatalf("kept = %v", kept)
	}
	if len(dropped) != 2 || dropped[0] != "Analyzelmage" || dropped[1] != "Tool_describe" {
		t.Fatalf("dropped = %v", dropped)
	}
	// 空 tools / 空 calls 原样返回。
	if k, d := filterKnownCalls(calls, nil); len(k) != 3 || d != nil {
		t.Fatalf("nil tools: kept=%v dropped=%v", k, d)
	}
	if k, d := filterKnownCalls(nil, tools); k != nil || d != nil {
		t.Fatalf("nil calls: kept=%v dropped=%v", k, d)
	}
}

// ZCode 的图片分片形状：{"type":"image","mediaType":"image/png","dataUrl":"data:..."}。
func TestPromptAndImagesInlineDataURL(t *testing.T) {
	const dataURL = "data:image/png;base64,aGVsbG8="
	msgs := []openAIMessage{
		{Role: "user", Text: "看这张图", Content: []map[string]any{
			{"type": "image", "mediaType": "image/png", "dataUrl": dataURL},
		}},
	}
	p, imgs := promptAndImages(msgs)
	if len(imgs) != 1 {
		t.Fatalf("images = %#v", imgs)
	}
	img := imgs[0]
	if img.MIME != "image/png" || img.URL != dataURL || img.Filename != "image-1.png" {
		t.Fatalf("image = %#v", img)
	}
	if img.Offset != 0 {
		t.Fatalf("offset = %d, want message start", img.Offset)
	}
	if !strings.Contains(p, "看这张图") || !strings.Contains(p, "已随本消息以附件发送") {
		t.Fatalf("prompt missing text/attachment marker:\n%s", p)
	}
	// 旧行为：只要能传图，就不该再声明「无法传图」。
	if strings.Contains(p, "无法传图") {
		t.Fatalf("stale placeholder leaked:\n%s", p)
	}
}

func TestPromptAndImagesOpenAIShape(t *testing.T) {
	const dataURL = "data:image/jpeg;base64,aGVsbG8="
	msgs := []openAIMessage{
		{Role: "system", Text: "sys"},
		{Role: "user", Text: "图", Content: []map[string]any{
			{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
		}},
	}
	_, imgs := promptAndImages(msgs)
	if len(imgs) != 1 || imgs[0].MIME != "image/jpeg" || imgs[0].Filename != "image-1.jpg" {
		t.Fatalf("images = %#v", imgs)
	}
	if imgs[0].Offset == 0 {
		t.Fatal("offset should point at the second message, not zero")
	}
}

func TestPromptAndImagesRejectsNonInline(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "远程图", Content: []map[string]any{
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
		}},
		// ZCode 裸 base64 + mediaType 也要能拼成 data URL。
		{Role: "user", Text: "裸 base64", Content: []map[string]any{
			{"type": "image", "mediaType": "image/webp", "data": "aGVsbG8="},
		}},
	}
	p, imgs := promptAndImages(msgs)
	if len(imgs) != 1 || imgs[0].MIME != "image/webp" || imgs[0].URL != "data:image/webp;base64,aGVsbG8=" {
		t.Fatalf("images = %#v", imgs)
	}
	if !strings.Contains(p, "不是可内联发送的图片") {
		t.Fatalf("remote image should be reported, got:\n%s", p)
	}
}

func TestPromptAndImagesVideoPlaceholder(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "看视频", Content: []map[string]any{{"type": "video", "dataUrl": "data:video/mp4;base64,aGVsbG8="}}},
	}
	p, imgs := promptAndImages(msgs)
	if len(imgs) != 0 {
		t.Fatalf("images = %#v", imgs)
	}
	if !strings.Contains(p, "无法接收视频") {
		t.Fatalf("missing video placeholder:\n%s", p)
	}
}

func TestPromptAndImagesCapsCount(t *testing.T) {
	parts := make([]map[string]any, 0, cliMaxImages+2)
	urls := make([]string, 0, cliMaxImages+2)
	for i := 0; i < cliMaxImages+2; i++ {
		u := "data:image/png;base64," + strings.Repeat("A", i+1)
		urls = append(urls, u)
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	p, imgs := promptAndImages([]openAIMessage{{Role: "user", Text: "九张", Content: parts}})
	if len(imgs) != cliMaxImages {
		t.Fatalf("images = %d, want %d", len(imgs), cliMaxImages)
	}
	if !strings.Contains(p, "超出单次附件数量上限") {
		t.Fatalf("missing cap notice:\n%s", p)
	}
}

func TestPromptAndImagesTextOnly(t *testing.T) {
	p, imgs := promptAndImages([]openAIMessage{{Role: "user", Text: "hi"}})
	if len(imgs) != 0 || strings.Contains(p, "无法传图") || strings.Contains(p, "附件") {
		t.Fatalf("text-only prompt polluted: %q imgs=%v", p, imgs)
	}
}

// 图片 Offset 的平移依赖「工具规则只前置、不改用户 prompt」这一性质。
func TestToolPromptOnlyPrepends(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "Bash"}}}
	choice := toolChoiceOpenAI{Mode: "auto"}
	const user = "[用户]\n看图\n"
	if got, want := toolPrompt(user, tools, choice), toolPrompt("", tools, choice)+user; got != want {
		t.Fatalf("toolPrompt is not a pure prefix:\ngot  %q\nwant %q", got, want)
	}
}
