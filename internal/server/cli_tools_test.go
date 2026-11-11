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

func TestPromptFromMessagesImagePlaceholder(t *testing.T) {
	msgs := []openAIMessage{
		{Role: "user", Text: "看这张图", Content: []map[string]any{{"type": "image_url"}}},
	}
	p := promptFromMessages(msgs)
	if !strings.Contains(p, "无法传图") || !strings.Contains(p, "看这张图") {
		t.Fatalf("missing image placeholder:\n%s", p)
	}
	// 纯文本消息不附加占位。
	if strings.Contains(promptFromMessages([]openAIMessage{{Role: "user", Text: "hi"}}), "无法传图") {
		t.Fatal("placeholder leaked into text-only message")
	}
}
