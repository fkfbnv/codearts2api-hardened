package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codearts2api/internal/upstream"
)

// 请求体超过上游 10MiB：网关本地回 413 request_too_large（带明确原因），
// 不再把上游的 InferHub.001001005 误导文案透传，也不冷却/记错误账号。
func TestOversizedUpstreamBodyReturns413WithoutTouchingPool(t *testing.T) {
	var called atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrame)
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)

	// 请求本身要能过网关自己的 32MB 上限，只在发往上游这一步超 10MiB。
	big := strings.Repeat("x", int(upstream.DefaultChatBodyLimit)+(64<<10))
	reqBody, err := json.Marshal(map[string]any{
		"model":  "GLM-5.2",
		"stream": false,
		"messages": []map[string]any{
			{"role": "user", "content": big},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqBody) <= int(upstream.DefaultChatBodyLimit) {
		t.Fatalf("test request body is not oversized: %d", len(reqBody))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(reqBody))))

	if called.Load() {
		t.Fatal("oversized request must not reach the upstream")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d body=%s", rec.Code, rec.Body.String())
	}
	payload := parseOpenAIError(t, rec.Body.Bytes())
	if payload.Error.Code != "request_too_large" {
		t.Fatalf("unexpected error code %q body=%s", payload.Error.Code, rec.Body.String())
	}
	for _, want := range []string{
		fmt.Sprint(upstream.DefaultChatBodyLimit), "10485760", "InferHub.001001005",
	} {
		if !strings.Contains(payload.Error.Message, want) {
			t.Fatalf("error message should mention %q: %q", want, payload.Error.Message)
		}
	}
	assertToolChoicePoolHealthy(t, p)
}
