package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
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

// 上游 401：绝不能把账号永久禁用（1110 自锁事故的回归测试）。
//
// 这里的测试账号没有 refresh_token，所以「刷新后重试」这条路走不通，只能落到
// 错误计数：ErrThreshold=1 时计一次错误即软冷却。关键不变量是 disabled=0——
// 1110 那次就是一次 401 直接 Disable，watchdog 跳过禁用账号 → 凭据再也不刷新。
// 有 refresh_token 的情形见 TestAuth0301RefreshesAndRetriesWithoutCooldown。
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

	total, _, disabled, _, _ := p.Stats()
	if total == 0 {
		t.Fatal("test pool has no accounts")
	}
	if disabled != 0 {
		t.Fatalf("401 must not permanently disable accounts: %#v", p.List())
	}
	for _, st := range p.List() {
		if st["disabled"] == true {
			t.Fatalf("account left disabled after 401: %#v", st)
		}
	}
}

// P1 回归：上游回 APIG.0301（decrypt token fail）时，必须「立即强制刷新 token
// 再重试一次」，而不是把账号扔进 60s 软冷却。
//
// 2026-11-15 实测：12:54/12:56/12:58/13:00 四次 pool cooldown 全是这个形态，
// 集中在 token 到期前的刷新窗口。旧代码只对 HTTP 401 刷新，而 0301 可能被包成
// 其它形态；且刷新成功后立刻重试仍会被拒（APIG 侧新 token 生效有延迟）。
func TestAuth0301RefreshesAndRetriesWithoutCooldown(t *testing.T) {
	var chatHits int
	var sawRefreshedToken bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		chatHits++
		// 第一次用旧 token → 0301；刷新之后（新 token）→ 正常出字。
		if tok := r.Header.Get("x-auth-token"); tok != "NEW-TOKEN" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error_msg":"Incorrect IAM authentication information: decrypt token fail","error_code":"APIG.0301"}`)
			return
		}
		sawRefreshedToken = true
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"refresh_token": "REFRESH-2",
			"credentials": {
				"access_key_id": "AK-NEW",
				"secret_access_key": "SK-NEW",
				"security_token": "NEW-TOKEN",
				"expiration": "`+time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339)+`"
			}
		}`)
	}))
	defer sts.Close()

	dpopJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL

	acct := auth.New(t.Name()+"-acct", "tester", "domain", "OLD-TOKEN", "AK-OLD", "SK-OLD",
		// 剩余 3h、skew=30m：Validate 不会提前刷新，刷新只能由 0301 触发，
		// 这样测的才是「请求路径上的 refresh→retry」，而不是后台主动续期。
		time.Now().Add(3*time.Hour).Format(time.RFC3339), "REFRESH-1", "verifier",
		map[string]string(dpopJWK))
	if err := acct.SetPathAndSave(filepath.Join(t.TempDir(), acct.FileName())); err != nil {
		t.Fatal(err)
	}
	p, err := pool.New([]*auth.Auth{acct}, pool.Config{MaxConcurrent: 1, ErrThreshold: 1, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Accounts() {
		a.Client = upstream.NewWithChatEndpoint(2*time.Second, backend.URL)
	}
	h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 2, ErrThreshold: 1})

	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("0301 should be recovered by refresh+retry, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !sawRefreshedToken {
		t.Fatal("retry did not carry the refreshed token")
	}
	if chatHits < 2 {
		t.Fatalf("expected a retry after refresh, chat hits=%d", chatHits)
	}
	total, healthy, disabled, cooling, _ := p.Stats()
	if disabled != 0 {
		t.Fatalf("0301 must not disable accounts: %#v", p.List())
	}
	if cooling != 0 || healthy != total {
		t.Fatalf("0301 recovered by refresh must not leave cooldown: total=%d healthy=%d cooling=%d",
			total, healthy, cooling)
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
