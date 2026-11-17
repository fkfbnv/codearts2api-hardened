package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 超过上游 10MiB 的请求必须在本地拦下：不上游、不换号、错误类型可区分。
func TestChatStreamRejectsBodyOverLimitBeforeUpstream(t *testing.T) {
	var called atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrameForTest)
	}))
	defer backend.Close()

	c := NewWithChatEndpoint(time.Second, backend.URL)
	c.MaxChatBodyBytes = 1024

	msgs := []ChatMessage{{Role: "user", Content: strings.Repeat("x", 2048)}}
	_, err := c.ChatStreamWithOptions(context.Background(), "", msgs, "", SignCredential{}, "tester", "GLM-5.2", ChatOptions{}, false)
	var limitErr *BodyLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("oversized body should return BodyLimitError, got %T %v", err, err)
	}
	if limitErr.Limit != 1024 || limitErr.Actual <= 1024 {
		t.Fatalf("unexpected limit error values: %+v", limitErr)
	}
	if called.Load() {
		t.Fatal("oversized body must not reach the upstream")
	}
	if !strings.Contains(err.Error(), "InferHub.001001005") {
		t.Fatalf("message should name the misleading upstream code: %v", err)
	}
}

// 未配置时使用上游实测的 10MiB 默认值。
func TestDefaultChatBodyLimitIs10MiB(t *testing.T) {
	if DefaultChatBodyLimit != 10<<20 {
		t.Fatalf("default chat body limit changed: %d", DefaultChatBodyLimit)
	}
}

const roleOnlyFrameForTest = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"
