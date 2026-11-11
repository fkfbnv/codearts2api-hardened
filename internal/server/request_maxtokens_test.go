package server

import "testing"

func TestParseChatRequestClampsMaxTokens(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":262144}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != MaxUpstreamOutputTokens {
		t.Fatalf("max_tokens not clamped: %v", req.MaxTokens)
	}
}

// 客户端传 max_completion_tokens 时折叠成 max_tokens 并同样封顶；
// 发往上游的 body 里不会出现 max_completion_tokens（上游用 001001005 拒它）。
func TestParseChatRequestFoldsMaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":999999}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != MaxUpstreamOutputTokens {
		t.Fatalf("max_completion_tokens not folded/clamped: %v", req.MaxTokens)
	}
}

func TestParseChatRequestKeepsSmallMaxTokens(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	req, err := parseChatRequest(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 128 {
		t.Fatalf("small max_tokens should be preserved: %v", req.MaxTokens)
	}
}
