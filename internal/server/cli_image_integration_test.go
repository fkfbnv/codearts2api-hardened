package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codearts2api/internal/cli"
)

type servedPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

// newImageFakeServe 模拟 codearts serve，记录每次 message 的 parts。
func newImageFakeServe(t *testing.T) (*httptest.Server, func() [][]servedPart) {
	t.Helper()
	var mu sync.Mutex
	var batches [][]servedPart
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_img"}`))
	})
	mux.HandleFunc("POST /session/ses_img/message", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []servedPart `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		batches = append(batches, body.Parts)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"info":{"modelID":"m-test"},"parts":[{"type":"text","text":"好的"}]}`))
	})
	return httptest.NewServer(mux), func() [][]servedPart {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]servedPart, len(batches))
		copy(out, batches)
		return out
	}
}

func servedFiles(parts []servedPart) []servedPart {
	var out []servedPart
	for _, p := range parts {
		if p.Type == "file" {
			out = append(out, p)
		}
	}
	return out
}

func TestChatCompletionsCLIImage(t *testing.T) {
	srv, batches := newImageFakeServe(t)
	defer srv.Close()

	h := NewHandler(Config{
		CLI:         cli.New(srv.URL, 0),
		CLIProvider: "huaweicloud-maas",
		CLIModels:   map[string]string{"m-test": "m-test"},
	})
	const dataURL = "data:image/png;base64,aGVsbG8="
	body := `{"model":"m-test","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看这张图"},` +
		`{"type":"image","mediaType":"image/png","dataUrl":"` + dataURL + `"}]}]}`

	rec := httptest.NewRecorder()
	h.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := batches()
	if len(got) != 1 {
		t.Fatalf("batches = %d", len(got))
	}
	files := servedFiles(got[0])
	if len(files) != 1 || files[0].URL != dataURL || files[0].Mime != "image/png" {
		t.Fatalf("file parts = %#v", files)
	}
	if !strings.Contains(got[0][0].Text, "看这张图") {
		t.Fatalf("text part = %q", got[0][0].Text)
	}
}

// 带工具时工具规则是前置的，图片 Offset 必须跟着平移，否则第二轮的新图会被
// 增量过滤误判成历史图，根本发不出去。
func TestChatCompletionsCLIImageWithToolsDelta(t *testing.T) {
	srv, batches := newImageFakeServe(t)
	defer srv.Close()

	h := NewHandler(Config{
		CLI:         cli.New(srv.URL, 0),
		CLIProvider: "huaweicloud-maas",
		CLIModels:   map[string]string{"m-test": "m-test"},
	})
	const img1 = "data:image/png;base64,AAA"
	const img2 = "data:image/png;base64,BBB"
	tools := `"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object","properties":{}}}}]`

	turn1 := `{"model":"m-test",` + tools + `,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"图一"},{"type":"image","mediaType":"image/png","dataUrl":"` + img1 + `"}]}]}`
	turn2 := `{"model":"m-test",` + tools + `,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"图一"},{"type":"image","mediaType":"image/png","dataUrl":"` + img1 + `"}]},` +
		`{"role":"assistant","content":"收到"},` +
		`{"role":"user","content":[{"type":"text","text":"图二"},{"type":"image","mediaType":"image/png","dataUrl":"` + img2 + `"}]}]}`

	for i, body := range []string{turn1, turn2} {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d status = %d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}

	got := batches()
	if len(got) != 2 {
		t.Fatalf("batches = %d", len(got))
	}
	if files := servedFiles(got[0]); len(files) != 1 || files[0].URL != img1 {
		t.Fatalf("turn1 files = %#v", files)
	}
	files := servedFiles(got[1])
	if len(files) != 1 || files[0].URL != img2 {
		t.Fatalf("turn2 should send only the new image, got %#v", files)
	}
}
