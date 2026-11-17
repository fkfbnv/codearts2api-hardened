package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
)

func TestRequestLogRing(t *testing.T) {
	l := newRequestLog(3)
	for i := 0; i < 5; i++ {
		l.add(requestEntry{Model: fmt.Sprintf("m%d", i)})
	}
	got := l.snapshot(0)
	if len(got) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(got))
	}
	want := []string{"m4", "m3", "m2"}
	for i, m := range want {
		if got[i].Model != m {
			t.Fatalf("got[%d] = %q, want %q", i, got[i].Model, m)
		}
	}
	// limit 生效，仍按新→旧。
	if one := l.snapshot(1); len(one) != 1 || one[0].Model != "m4" {
		t.Fatalf("snapshot(1) = %#v", one)
	}
}

type fakeFlusher struct {
	header  http.Header
	status  int
	flushed bool
}

func (f *fakeFlusher) Header() http.Header         { return f.header }
func (f *fakeFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeFlusher) WriteHeader(code int)        { f.status = code }
func (f *fakeFlusher) Flush()                      { f.flushed = true }

func TestStatusWriterForwardsFlush(t *testing.T) {
	f := &fakeFlusher{header: http.Header{}}
	sw := &statusWriter{ResponseWriter: f}
	sw.WriteHeader(http.StatusCreated)
	_, _ = sw.Write([]byte("x"))
	sw.Flush()
	if !f.flushed {
		t.Fatal("Flush was not forwarded (SSE would buffer silently)")
	}
	if sw.code() != http.StatusCreated {
		t.Fatalf("code = %d", sw.code())
	}
}

func TestChatCompletionsBodyLimitStats(t *testing.T) {
	h := NewHandler(Config{MaxBodyBytes: 16})
	body := strings.Repeat("x", 100)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
	st := h.bodyLimitStats()
	if st["local_413"] != 1 {
		t.Fatalf("local_413 = %v", st["local_413"])
	}
	if st["max_request_bytes"] != int64(100) {
		t.Fatalf("max_request_bytes = %v, want 100 (Content-Length)", st["max_request_bytes"])
	}
	items := h.recent.snapshot(1)
	if len(items) != 1 || items[0].Status != http.StatusRequestEntityTooLarge || items[0].Code != "request_too_large" {
		t.Fatalf("recent = %#v", items)
	}
}

func TestAvailabilitySnapshot(t *testing.T) {
	const good, bad = "panel-test-good", "panel-test-bad"
	markUsable("u-panel", good)
	markUnusable("u-panel", bad, "无权调用")
	cells := availabilitySnapshot([]map[string]any{{"uid": "u-panel"}}, []string{good, bad, "panel-test-none"})
	if len(cells) != 3 {
		t.Fatalf("cells = %d", len(cells))
	}
	want := map[string]string{good: "usable", bad: "unusable", "panel-test-none": "unknown"}
	for _, c := range cells {
		m, _ := c["model"].(string)
		if got := c["state"]; got != want[m] {
			t.Fatalf("%s state = %v, want %s", m, got, want[m])
		}
	}
}

func TestAdminProbeSkipsCLIModel(t *testing.T) {
	p, err := pool.New([]*auth.Auth{{UserID: "u-probe", UserName: "u-probe"}}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, CLIModels: map[string]string{"m-cli": "m-cli"}})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/probe",
		strings.NewReader(`{"uid":"u-probe","model":"m-cli"}`))
	rec := httptest.NewRecorder()
	h.adminProbe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"skipped":true`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// 未知账号 → 404。
	rec2 := httptest.NewRecorder()
	h.adminProbe(rec2, httptest.NewRequest(http.MethodPost, "/admin/api/probe",
		strings.NewReader(`{"uid":"nope","model":"m"}`)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("unknown uid status = %d", rec2.Code)
	}
}

func TestCLIOverviewDisabled(t *testing.T) {
	h := NewHandler(Config{})
	got := h.cliOverview()
	if got["enabled"] != false || got["healthy"] != false {
		t.Fatalf("cli overview = %#v", got)
	}
}
