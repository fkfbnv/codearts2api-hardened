package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/upstream"
)

// 上游 HTTP 200 + 首帧 error_code（模型级无权）：必须变成真正的 404 JSON，
// 而不是 200 + SSE error 事件。这是移植第 6 项的端到端验证。
func TestFirstFrameModelErrorReturnsHTTP404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: done\ndata: {\"error_code\":\"TM.000010051\",\"error_msg\":\"您当前无权调用该模型，请联系企业管理员开通。\"}\n\n")
	}))
	defer backend.Close()

	h, _ := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("first-frame model error should map to 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("response should be JSON, got Content-Type=%q body=%s", ct, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse error body: %v (%s)", err, rec.Body.String())
	}
	if payload.Error.Code != "model_not_available" {
		t.Fatalf("unexpected error code %q", payload.Error.Code)
	}
}

// 上游 401：只能软冷却，绝不能把账号永久禁用（1110 自锁事故的回归测试）。
func TestUpstream401SoftCooldownsInsteadOfDisabling(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error_msg":"Incorrect IAM authentication information: decrypt token fail","error_code":"APIG.0301"}`)
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	total, healthy, disabled, cooling, _ := p.Stats()
	if total == 0 {
		t.Fatal("test pool has no accounts")
	}
	if disabled != 0 {
		t.Fatalf("401 must not permanently disable accounts: %#v", p.List())
	}
	if healthy != 0 || cooling != total {
		t.Fatalf("401 should soft-cool all accounts: total=%d healthy=%d cooling=%d", total, healthy, cooling)
	}
	for _, st := range p.List() {
		if st["disabled"] == true {
			t.Fatalf("account left disabled after 401: %#v", st)
		}
	}
}

// 请求体上限可配置：调小后超限应 413，且不触碰上游。
func TestMaxBodyBytesIsConfigurable(t *testing.T) {
	upstreamHits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backend.Close()

	h, _ := toolChoiceTestHandler(t, backend.URL)
	h.cfg.MaxBodyBytes = 1 << 20 // 1MB

	big := `{"model":"GLM-5.2","messages":[{"role":"user","content":"` + strings.Repeat("A", 2<<20) + `"}],"max_tokens":8}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1MB") {
		t.Fatalf("413 message should carry configured limit: %s", rec.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("oversized body must not reach upstream, hits=%d", upstreamHits)
	}
}

// 有缓存时目录刷新不阻塞：立即返回旧值，后台刷新（这里上游不可达，正好验证不等待）。
func TestFetchDynamicModelsReturnsCachedImmediately(t *testing.T) {
	h, _ := toolChoiceTestHandler(t, "http://127.0.0.1:1")

	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "cached-model", Name: "cached-model"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.refreshing = false
	dynamicModelsCache.Unlock()

	start := time.Now()
	got := h.fetchDynamicModels()
	elapsed := time.Since(start)

	if len(got) != 1 || got[0].ID != "cached-model" {
		t.Fatalf("expected cached model list, got %#v", got)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("cached fetch should not block on upstream, took %s", elapsed)
	}

	// 等后台刷新结束，避免影响后续测试。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		dynamicModelsCache.RLock()
		busy := dynamicModelsCache.refreshing
		dynamicModelsCache.RUnlock()
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
