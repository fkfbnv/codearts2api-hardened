package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const roleOnlyFrame = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"

type openAIErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseOpenAIError(t *testing.T, body []byte) openAIErrorPayload {
	t.Helper()
	var payload openAIErrorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse error body: %v (%s)", err, body)
	}
	return payload
}

// 角色帧之后上游才回参数非法（ModelArts.81001）：必须回 400 invalid_request 的
// JSON，而不是 200 + SSE error；账号不能被冷却/记错误。
func TestRoleFrameThenParamInvalidReturnsHTTP400(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrame+
			"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81001.400\",\"error_msg\":\"该模型始终思考，不支持关闭思考；请使用 low、high 或 max。\"}\n\n")
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("role-frame param error should map to 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("response should be JSON, got Content-Type=%q body=%s", ct, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("must not emit SSE error for pre-stream failure: %s", rec.Body.String())
	}
	payload := parseOpenAIError(t, rec.Body.Bytes())
	if payload.Error.Code != "invalid_request" {
		t.Fatalf("unexpected error code %q body=%s", payload.Error.Code, rec.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "81001") && !strings.Contains(payload.Error.Message, "始终思考") {
		t.Fatalf("truncated upstream reason missing: %q", payload.Error.Message)
	}
	assertErrorLeftPoolHealthy(t, p)
}

// 角色帧之后上游才回内容审查（ModelArts.81011）：400 content_filter，账号不冷却。
func TestRoleFrameThenContentModerationReturnsHTTP400(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrame+
			"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81011.403\",\"error_msg\":\"抱歉，我无法提供关于您提问内容的建议。\"}\n\n")
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("role-frame content moderation should map to 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	payload := parseOpenAIError(t, rec.Body.Bytes())
	if payload.Error.Code != "content_filter" {
		t.Fatalf("unexpected error code %q body=%s", payload.Error.Code, rec.Body.String())
	}
	if !strings.Contains(payload.Error.Message, "抱歉，我无法提供关于您提问内容的建议") {
		t.Fatalf("truncated upstream reason missing: %q", payload.Error.Message)
	}
	assertErrorLeftPoolHealthy(t, p)
	for _, acct := range p.Accounts() {
		if st, _ := modelAvailability(acct.UID, "GLM-5.2"); st == availUnusable {
			t.Fatalf("content moderation must not mark model unusable: %#v", acct.UID)
		}
	}
}

// 角色帧之后的无权错误（TM.000010051）：原有模型级处理不能回归，仍是 404
// model_not_available（两个测试账号都试过、都无权）。
func TestRoleFrameThenModelDeniedStillReturns404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrame+
			"event: done\ndata: {\"error_code\":\"TM.000010051\",\"error_msg\":\"您当前无权调用该模型，请联系企业管理员开通。\"}\n\n")
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("role-frame model denied should map to 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	payload := parseOpenAIError(t, rec.Body.Bytes())
	if payload.Error.Code != "model_not_available" {
		t.Fatalf("unexpected error code %q body=%s", payload.Error.Code, rec.Body.String())
	}
	assertErrorLeftPoolHealthy(t, p)
}

// 非流式 AggregateRaw 错误分支：SSE 正文里的内容审查错误（code/message 形态，
// preflight 无法提前识别）必须回 400 content_filter，而不是 503 no_healthy_account，
// 且账号健康、模型不被下载。
func TestAggregateContentModerationReturnsHTTP400(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, roleOnlyFrame+
			"data: {\"code\":\"InferHub.ModelArts.81011.403\",\"message\":\"抱歉，我无法提供关于您提问内容的建议。\"}\n\n")
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)
	body := `{"model":"GLM-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("aggregate content moderation should map to 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	respBody := rec.Body.String()
	if strings.Contains(respBody, "no_healthy_account") {
		t.Fatalf("content moderation must not degrade into no_healthy_account: %s", respBody)
	}
	payload := parseOpenAIError(t, rec.Body.Bytes())
	if payload.Error.Code != "content_filter" {
		t.Fatalf("unexpected error code %q body=%s", payload.Error.Code, respBody)
	}
	if !strings.Contains(payload.Error.Message, "抱歉，我无法提供关于您提问内容的建议") {
		t.Fatalf("truncated upstream reason missing: %q", payload.Error.Message)
	}
	assertErrorLeftPoolHealthy(t, p)
	for _, acct := range p.Accounts() {
		if st, _ := modelAvailability(acct.UID, "GLM-5.2"); st == availUnusable {
			t.Fatalf("content moderation must not mark model unusable: %#v", acct.UID)
		}
	}
}

func assertErrorLeftPoolHealthy(t *testing.T, p interface {
	Stats() (int, int, int, int, int64)
	List() []map[string]any
}) {
	t.Helper()
	total, healthy, disabled, cooling, _ := p.Stats()
	if total != 2 || healthy != 2 || disabled != 0 || cooling != 0 {
		t.Fatalf("client-side upstream error changed pool state: total=%d healthy=%d disabled=%d cooling=%d",
			total, healthy, disabled, cooling)
	}
	for _, state := range p.List() {
		if state["err_count"] != 0 {
			t.Fatalf("client-side upstream error accumulated err_count: %#v", state)
		}
	}
}
