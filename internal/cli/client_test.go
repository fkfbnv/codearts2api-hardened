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

// fakePart serve 侧收到的 part。
type fakePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

// newFakeServeParts 记录每次 POST /message 的完整 parts。
func newFakeServeParts(t *testing.T) (*httptest.Server, func() [][]fakePart) {
	t.Helper()
	var mu sync.Mutex
	var batches [][]fakePart
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_1"}`))
	})
	mux.HandleFunc("POST /session/ses_1/message", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []fakePart `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		batches = append(batches, body.Parts)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"info":{"modelID":"m"},"parts":[{"type":"text","text":"ok"}]}`))
	})
	return httptest.NewServer(mux), func() [][]fakePart {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]fakePart, len(batches))
		copy(out, batches)
		return out
	}
}

func fileParts(parts []fakePart) []fakePart {
	var out []fakePart
	for _, p := range parts {
		if p.Type == "file" {
			out = append(out, p)
		}
	}
	return out
}

func TestChatSendsImageFileParts(t *testing.T) {
	srv, batches := newFakeServeParts(t)
	defer srv.Close()

	c := New(srv.URL, 0)
	ctx := context.Background()
	const img1 = "data:image/png;base64,AAA"
	const img2 = "data:image/webp;base64,BBB"

	full1 := "[用户]\n第一轮\n"
	if _, err := c.Chat(ctx, ChatRequest{
		ConversationID: "conv", Provider: "p", Model: "m", Prompt: full1,
		Images: []Image{{MIME: "image/png", Filename: "image-1.png", URL: img1, Offset: 0}},
	}); err != nil {
		t.Fatal(err)
	}

	full2 := full1 + "[助手]\n答\n\n[用户]\n第二轮\n"
	secondOffset := len(full1) + 8 // 落在增量区间即可
	if _, err := c.Chat(ctx, ChatRequest{
		ConversationID: "conv", Provider: "p", Model: "m", Prompt: full2,
		Images: []Image{
			{MIME: "image/png", Filename: "image-1.png", URL: img1, Offset: 0},
			{MIME: "image/webp", Filename: "image-2.webp", URL: img2, Offset: secondOffset},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got := batches()
	if len(got) != 2 {
		t.Fatalf("batches = %d", len(got))
	}
	first := fileParts(got[0])
	if len(first) != 1 || first[0].Mime != "image/png" || first[0].URL != img1 || first[0].Filename != "image-1.png" {
		t.Fatalf("first batch files = %#v", first)
	}
	second := fileParts(got[1])
	if len(second) != 1 || second[0].URL != img2 {
		t.Fatalf("second batch should carry only the new image, got %#v", second)
	}
	if !strings.Contains(got[1][0].Text, "第二轮") {
		t.Fatalf("second batch text = %q", got[1][0].Text)
	}
}

func TestChatResendsAllImagesOnRewrite(t *testing.T) {
	srv, batches := newFakeServeParts(t)
	defer srv.Close()

	c := New(srv.URL, 0)
	ctx := context.Background()
	const img1 = "data:image/png;base64,AAA"
	const img2 = "data:image/png;base64,BBB"

	if _, err := c.Chat(ctx, ChatRequest{
		ConversationID: "conv", Provider: "p", Model: "m", Prompt: "[用户]\nA\n",
		Images: []Image{{MIME: "image/png", Filename: "image-1.png", URL: img1}},
	}); err != nil {
		t.Fatal(err)
	}
	// 历史被改写：新会话没有旧图，必须全量重发。
	if _, err := c.Chat(ctx, ChatRequest{
		ConversationID: "conv", Provider: "p", Model: "m", Prompt: "[用户]\nB\n",
		Images: []Image{
			{MIME: "image/png", Filename: "image-1.png", URL: img1},
			{MIME: "image/png", Filename: "image-2.png", URL: img2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := batches()
	if len(got) != 2 {
		t.Fatalf("batches = %d", len(got))
	}
	if files := fileParts(got[1]); len(files) != 2 {
		t.Fatalf("rewrite should resend both images, got %#v", files)
	}
}

func TestChatDedupesIdenticalImages(t *testing.T) {
	srv, batches := newFakeServeParts(t)
	defer srv.Close()

	c := New(srv.URL, 0)
	const img = "data:image/png;base64,AAA"
	if _, err := c.Chat(context.Background(), ChatRequest{
		ConversationID: "conv", Provider: "p", Model: "m", Prompt: "[用户]\n双图\n",
		Images: []Image{
			{MIME: "image/png", Filename: "image-1.png", URL: img, Offset: 0},
			{MIME: "image/png", Filename: "image-2.png", URL: img, Offset: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if files := fileParts(batches()[0]); len(files) != 1 {
		t.Fatalf("duplicate image sent twice: %#v", files)
	}
}

func TestSetMultimodalModels(t *testing.T) {
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /v1/config", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Models []string `json:"multimodalModels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body.Models
		mu.Unlock()
		_, _ = w.Write([]byte(`{"multimodalModels":["ok"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, 0)
	if err := c.SetMultimodalModels(context.Background(), []string{"kimi-k3.1-pro"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "kimi-k3.1-pro" {
		t.Fatalf("patched models = %v", got)
	}
}
