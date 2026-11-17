package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"codearts2api/internal/upstream"
)

// recentRequestsCap 面板「最近请求」保留条数。只存内存：重启清空，
// 目的是排障时看最近发生了什么，不做审计。
const recentRequestsCap = 100

// requestEntry 一条请求记录（面板直接序列化这个结构）。
type requestEntry struct {
	Time       time.Time `json:"time"`
	Model      string    `json:"model"`
	Account    string    `json:"account"`
	Source     string    `json:"source"` // direct / cli
	Status     int       `json:"status"`
	Code       string    `json:"code"`
	Kind       string    `json:"kind"`
	Message    string    `json:"message"`
	DurationMS int64     `json:"duration_ms"`
	Stream     bool      `json:"stream"`
	BodyBytes  int       `json:"body_bytes"`
}

// requestLog 环形记录。只保留最近 cap 条，新记录覆盖最旧的。
type requestLog struct {
	mu    sync.Mutex
	items []requestEntry
	next  int
	full  bool
	cap   int
}

func newRequestLog(capacity int) *requestLog {
	if capacity <= 0 {
		capacity = recentRequestsCap
	}
	return &requestLog{items: make([]requestEntry, capacity), cap: capacity}
}

func (l *requestLog) add(e requestEntry) {
	l.mu.Lock()
	l.items[l.next] = e
	l.next = (l.next + 1) % l.cap
	if l.next == 0 {
		l.full = true
	}
	l.mu.Unlock()
}

// snapshot 按新→旧返回最近 limit 条。
func (l *requestLog) snapshot(limit int) []requestEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = l.cap
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]requestEntry, 0, limit)
	idx := l.next
	for i := 0; i < limit; i++ {
		idx = (idx - 1 + l.cap) % l.cap
		out = append(out, l.items[idx])
	}
	return out
}

// requestInfo 一次请求的可变状态。只在处理该请求的 goroutine 内写，
// defer 时读取，不需要额外加锁。
type requestInfo struct {
	start   time.Time
	model   string
	account string
	source  string
	code    string
	kind    string
	message string
	stream  bool
	body    int
}

type requestInfoKey struct{}

func withRequestInfo(ctx context.Context, ri *requestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, ri)
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	ri, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return ri
}

func (ri *requestInfo) note(code, message string) {
	if ri == nil {
		return
	}
	ri.code = code
	if message != "" {
		ri.message = truncateMsg(message, 300)
	}
}

func (ri *requestInfo) noteKind(kind, reason string) {
	if ri == nil {
		return
	}
	ri.kind = kind
	if reason != "" {
		ri.message = truncateMsg(reason, 300)
	}
}

func (ri *requestInfo) entry(status int) requestEntry {
	e := requestEntry{
		Time:       ri.start,
		Model:      ri.model,
		Account:    ri.account,
		Source:     ri.source,
		Status:     status,
		Code:       ri.code,
		Kind:       ri.kind,
		Message:    ri.message,
		DurationMS: time.Since(ri.start).Milliseconds(),
		Stream:     ri.stream,
		BodyBytes:  ri.body,
	}
	if e.Model == "" {
		e.Model = "-"
	}
	if e.Source == "" {
		e.Source = "direct"
	}
	if e.Status == 0 {
		e.Status = http.StatusOK
	}
	return e
}

// statusWriter 记录 HTTP 状态码。必须完整转发底层接口：
// 流式路径用 w.(http.Flusher) 断言，包装器不转发 Flush 会让 SSE 静默变缓冲。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController（若后续使用）能拿到底层 writer。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// requestStats 请求体体积与 413 预检计数（面板运行状态卡）。
type requestStats struct {
	mu           sync.Mutex
	local413     int
	upstream413  int
	maxBodyBytes int64
	last413At    time.Time
}

func (h *Handler) noteBodySize(n int64) {
	h.stats.mu.Lock()
	if n > h.stats.maxBodyBytes {
		h.stats.maxBodyBytes = n
	}
	h.stats.mu.Unlock()
}

func (h *Handler) note413(upstreamLimit bool) {
	h.stats.mu.Lock()
	if upstreamLimit {
		h.stats.upstream413++
	} else {
		h.stats.local413++
	}
	h.stats.last413At = time.Now()
	h.stats.mu.Unlock()
}

func (h *Handler) bodyLimitStats() map[string]any {
	h.stats.mu.Lock()
	defer h.stats.mu.Unlock()
	last := ""
	if !h.stats.last413At.IsZero() {
		last = h.stats.last413At.Format(time.RFC3339)
	}
	return map[string]any{
		"local_mb":          h.cfg.MaxBodyBytes >> 20,
		"upstream_mb":       upstream.DefaultChatBodyLimit >> 20,
		"local_413":         h.stats.local413,
		"upstream_413":      h.stats.upstream413,
		"max_request_bytes": h.stats.maxBodyBytes,
		"last_413_at":       last,
	}
}
