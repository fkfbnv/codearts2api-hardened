package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 心跳会话的核心约定：busy 先行、周期续期、Stop 后 idle 恰好一次且在最后。
func TestChatSessionHeartbeatLifecycle(t *testing.T) {
	var mu sync.Mutex
	var beats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != EpChatSessionHeartbeat {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("user-session-id") == "" {
			t.Error("missing user-session-id header")
		}
		if r.Header.Get("x-client-type") != "kernel" {
			t.Error("missing x-client-type: kernel")
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing signature")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		beats = append(beats, r.URL.Query().Get("status"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapManagerBase = srv.URL
	cred := SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "ST"}

	s, err := c.BeginChatSession(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() == "" {
		t.Fatal("session id empty")
	}
	// Stop 后：idle 恰好一次，且在最后的 busy 之后（规则 4）。
	s.Stop()
	s.Stop() // 幂等

	mu.Lock()
	defer mu.Unlock()
	if len(beats) == 0 || beats[0] != "busy" {
		t.Fatalf("first beat should be busy, got %v", beats)
	}
	if beats[len(beats)-1] != "idle" {
		t.Fatalf("last beat should be idle, got %v", beats)
	}
	idles := 0
	for _, b := range beats {
		if b == "idle" {
			idles++
		}
	}
	if idles != 1 {
		t.Fatalf("idle exactly once, got %d: %v", idles, beats)
	}
}

// 首次 busy 被拒：先尽力注销自己刚造的 id，再把错误交回（规则 2）。
func TestChatSessionBusyRejectedSendsIdle(t *testing.T) {
	var mu sync.Mutex
	var beats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		beats = append(beats, r.URL.Query().Get("status"))
		mu.Unlock()
		if r.URL.Query().Get("status") == "busy" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error_code":"TM.00001041"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapManagerBase = srv.URL
	if _, err := c.BeginChatSession(context.Background(), SignCredential{AccessKeyID: "AK", SecretAccessKey: "SK"}); err == nil {
		t.Fatal("busy rejected should return error")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 2 || beats[0] != "busy" || beats[1] != "idle" {
		t.Fatalf("busy 失败后应补发 idle，got %v", beats)
	}
}
