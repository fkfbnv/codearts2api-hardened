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
