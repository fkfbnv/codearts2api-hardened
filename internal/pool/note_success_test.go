package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codearts2api/internal/auth"
)

func newStateTestAuth(userID string) *auth.Auth {
	return auth.New(userID, "user-"+userID, "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
}

func writeStateFile(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 请求成功后，state.json 里残留的旧错误计数、旧错误文本、失效冷却都必须清掉，
// 并落盘（重启后不会复活旧计数）。
func TestNoteSuccessClearsStaleErrorStateAndPersists(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	writeStateFile(t, stateFile, []map[string]any{{
		"name":       "u-ok",
		"err_count":  2,
		"cool_until": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"disabled":   false,
		"reason":     "",
		"last_error": "consecutive errors",
	}})

	p, err := New([]*auth.Auth{newStateTestAuth("u-ok")}, Config{ErrThreshold: 3}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	acct := p.Get("u-ok")
	if acct == nil {
		t.Fatal("account not loaded")
	}
	acct.mu.Lock()
	loaded := struct {
		errCount  int
		lastErr   string
		coolUntil time.Time
	}{acct.errCount, acct.lastErr, acct.coolUntil}
	acct.mu.Unlock()
	if loaded.errCount != 2 || loaded.lastErr == "" || loaded.coolUntil.IsZero() {
		t.Fatalf("state file was not loaded as expected: %+v", loaded)
	}

	p.NoteSuccess("u-ok")

	acct.mu.Lock()
	got := struct {
		errCount  int
		lastErr   string
		coolUntil time.Time
		coolKind  CoolKind
	}{acct.errCount, acct.lastErr, acct.coolUntil, acct.coolKind}
	acct.mu.Unlock()
	if got.errCount != 0 {
		t.Fatalf("errCount not cleared: %d", got.errCount)
	}
	if got.lastErr != "" {
		t.Fatalf("lastErr not cleared: %q", got.lastErr)
	}
	if !got.coolUntil.IsZero() || got.coolKind != 0 {
		t.Fatalf("stale cooldown not cleared: until=%s kind=%d", got.coolUntil, got.coolKind)
	}
	if !p.Healthy("u-ok") {
		t.Fatal("account should be healthy after NoteSuccess")
	}

	// 从磁盘重新加载：清理结果必须已持久化。
	p2, err := New([]*auth.Auth{newStateTestAuth("u-ok")}, Config{ErrThreshold: 3}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	state := p2.List()[0]
	if state["err_count"] != 0 {
		t.Fatalf("persisted err_count still stale: %#v", state)
	}
	if state["last_error"] != "" {
		t.Fatalf("persisted last_error still stale: %#v", state)
	}
	if state["cooling"] == true {
		t.Fatalf("persisted cooldown still active: %#v", state)
	}
}

// 成功请求不能擅自启用被禁用的账号，也不能改禁用原因。
func TestNoteSuccessKeepsDisabledAccountDisabled(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	writeStateFile(t, stateFile, []map[string]any{{
		"name":       "u-disabled",
		"err_count":  1,
		"cool_until": time.Time{}.Format(time.RFC3339),
		"disabled":   true,
		"reason":     "refresh_token 已失效，需要重新登录",
		"last_error": "refresh_token 已失效，需要重新登录",
	}})

	p, err := New([]*auth.Auth{newStateTestAuth("u-disabled")}, Config{ErrThreshold: 3}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	p.NoteSuccess("u-disabled")

	acct := p.Get("u-disabled")
	acct.mu.Lock()
	disabled, reason := acct.disabled, acct.disabledReason
	acct.mu.Unlock()
	if !disabled {
		t.Fatal("NoteSuccess enabled a disabled account")
	}
	if reason != "refresh_token 已失效，需要重新登录" {
		t.Fatalf("NoteSuccess changed disabledReason: %q", reason)
	}
	if p.Healthy("u-disabled") {
		t.Fatal("disabled account reported healthy after NoteSuccess")
	}

	p2, err := New([]*auth.Auth{newStateTestAuth("u-disabled")}, Config{ErrThreshold: 3}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if state := p2.List()[0]; state["disabled"] != true {
		t.Fatalf("disabled flag not persisted after NoteSuccess: %#v", state)
	}
}
