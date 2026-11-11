package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newFakeServe 模拟 codearts serve：POST /session 建会话，
// POST /session/{id}/message 记录收到的 prompt 并回固定文本。
func newFakeServe(t *testing.T, mu *sync.Mutex, got *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_1"}`))
	})
	mux.HandleFunc("POST /session/ses_1/message", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*got = append(*got, body.Parts[0].Text)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"info":{"modelID":"m"},"parts":[{"type":"text","text":"ok"}]}`))
	})
	return httptest.NewServer(mux)
}

func TestChatDeltaOnPrefix(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := newFakeServe(t, &mu, &got)
	defer srv.Close()

	c := New(srv.URL, 0)
	ctx := context.Background()

	full1 := "[用户]\n第一轮\n"
	if _, err := c.Chat(ctx, ChatRequest{ConversationID: "conv", Provider: "p", Model: "m", Prompt: full1}); err != nil {
		t.Fatal(err)
	}
	full2 := full1 + "[助手]\n答\n\n[用户]\n第二轮\n"
	if _, err := c.Chat(ctx, ChatRequest{ConversationID: "conv", Provider: "p", Model: "m", Prompt: full2}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("got %d prompts", len(got))
	}
	if got[0] != full1 {
		t.Fatalf("first prompt = %q", got[0])
	}
	if !strings.Contains(got[1], "第二轮") || strings.Contains(got[1], "第一轮") {
		t.Fatalf("second prompt should be delta only, got %q", got[1])
	}
}

func TestChatFullResendOnRewrite(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := newFakeServe(t, &mu, &got)
	defer srv.Close()

	c := New(srv.URL, 0)
	ctx := context.Background()
	if _, err := c.Chat(ctx, ChatRequest{ConversationID: "conv", Provider: "p", Model: "m", Prompt: "[用户]\nA\n"}); err != nil {
		t.Fatal(err)
	}
	// 历史被改写：前缀不匹配 → 全量重建。
	if _, err := c.Chat(ctx, ChatRequest{ConversationID: "conv", Provider: "p", Model: "m", Prompt: "[用户]\nB\n"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[1] != "[用户]\nB\n" {
		t.Fatalf("prompts = %q", got)
	}
}
