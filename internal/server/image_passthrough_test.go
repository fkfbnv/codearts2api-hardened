package server

import (
	"encoding/json"
	"testing"
)

// 图片分片必须原样带到上游，不能被 flattenContent 拍平成纯文本。
func TestImagePartsReachUpstream(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4.1-flash",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"这张图里是什么？"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
		]}]
	}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := buildUpstreamMessages(req, false)
	if len(msgs) != 1 {
		t.Fatalf("upstream messages=%d", len(msgs))
	}
	parts, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("content type=%T, want []map[string]any", msgs[0].Content)
	}
	var imageURL string
	for _, p := range parts {
		if p["type"] == "image_url" {
			if m, ok := p["image_url"].(map[string]any); ok {
				imageURL, _ = m["url"].(string)
			}
		}
	}
	if imageURL != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url=%q", imageURL)
	}
	wire, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(wire) {
		t.Fatal("upstream wire is not valid JSON")
	}
}

// ZCode 发的分片是 {"type":"image","mediaType":...,"dataUrl":...}，同样要带图。
func TestZCodeImagePartReachUpstream(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[
		{"type":"text","text":"如图所示"},
		{"type":"image","mediaType":"image/png","dataUrl":"data:image/png;base64,aGVsbG8=","source":{"kind":"inline","mimeType":"image/png"}}
	]}]}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := buildUpstreamMessages(req, false)
	parts, ok := msgs[0].Content.([]map[string]any)
	if !ok {
		t.Fatalf("content type=%T, want []map[string]any", msgs[0].Content)
	}
	found := false
	for _, p := range parts {
		if p["type"] == "image_url" {
			if m, ok := p["image_url"].(map[string]any); ok && m["url"] == "data:image/png;base64,aGVsbG8=" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("image part lost: %#v", parts)
	}
}

// 视频分片必须保留。以前 hasImage 只由图片置位，纯视频消息整体退回拍平文本，
// 上游只能看到「没有附件」（kimi 实测回 "I don't see a video attached"）。
func TestVideoPartReachUpstream(t *testing.T) {
	for _, body := range []string{
		`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":[
			{"type":"text","text":"描述视频"},
			{"type":"video_url","video_url":{"url":"data:video/mp4;base64,aGVsbG8="}}
		]}]}`,
		// 纯视频（无文本分片）也必须保留成数组
		`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":[
			{"type":"video_url","video_url":{"url":"data:video/mp4;base64,aGVsbG8="}}
		]}]}`,
		// ZCode 的分片形状必须被转成 video_url（上游只认这个类型名）
		`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":[
			{"type":"text","text":"描述视频"},
			{"type":"video","mediaType":"video/mp4","dataUrl":"data:video/mp4;base64,aGVsbG8="}
		]}]}`,
	} {
		req, err := parseChatRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		msgs := buildUpstreamMessages(req, false)
		parts, ok := msgs[0].Content.([]map[string]any)
		if !ok {
			t.Fatalf("content type=%T, want []map[string]any", msgs[0].Content)
		}
		found := false
		for _, p := range parts {
			if p["type"] == "video_url" {
				if m, ok := p["video_url"].(map[string]any); ok && m["url"] == "data:video/mp4;base64,aGVsbG8=" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("video part lost: %#v", parts)
		}
	}
}

// 纯文本消息必须维持原来的字符串 content，行为不变。
func TestPlainTextStillFlattened(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := buildUpstreamMessages(req, false)
	if got, ok := msgs[0].Content.(string); !ok || got != "ab" {
		t.Fatalf("content=%#v, want string ab", msgs[0].Content)
	}
}
