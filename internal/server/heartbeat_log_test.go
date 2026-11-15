package server

import (
	"errors"
	"testing"
	"time"
)

// 心跳不可用的降级日志必须限流：原来每个请求一条（1112 实测几秒一条，
// 一天几百行），把真正有用的上游错误全淹了。
func TestNoteHeartbeatUnavailableIsRateLimited(t *testing.T) {
	// 用独立账号名，避免与其它测试共用 gate。
	const acct = "heartbeat-gate-test"
	heartbeatUnavailableLogGate.Lock()
	delete(heartbeatUnavailableLogGate.last, acct)
	delete(heartbeatUnavailableLogGate.suppres, acct)
	heartbeatUnavailableLogGate.Unlock()

	err := errors.New(`chat-session busy rejected: HTTP 404 {"error_code":"APIG.0101"}`)
	if !isHeartbeatUnavailable(err) {
		t.Fatal("APIG.0101 must be treated as heartbeat unavailable")
	}

	noteHeartbeatUnavailable(acct, err)

	heartbeatUnavailableLogGate.Lock()
	first := heartbeatUnavailableLogGate.last[acct]
	heartbeatUnavailableLogGate.Unlock()
	if first.IsZero() {
		t.Fatal("first occurrence should record a timestamp")
	}

	// 随后 20 次同账号降级：只累计，不刷新时间戳（即不再打日志）。
	for i := 0; i < 20; i++ {
		noteHeartbeatUnavailable(acct, err)
	}
	heartbeatUnavailableLogGate.Lock()
	after := heartbeatUnavailableLogGate.last[acct]
	suppressed := heartbeatUnavailableLogGate.suppres[acct]
	heartbeatUnavailableLogGate.Unlock()

	if !after.Equal(first) {
		t.Fatalf("timestamp advanced despite rate limit: %v -> %v", first, after)
	}
	if suppressed != 20 {
		t.Fatalf("expected 20 suppressed reports, got %d", suppressed)
	}

	// 窗口过期后应重新放行一条，并带上被压掉的次数。
	heartbeatUnavailableLogGate.Lock()
	heartbeatUnavailableLogGate.last[acct] = time.Now().Add(-25 * time.Hour)
	heartbeatUnavailableLogGate.Unlock()

	noteHeartbeatUnavailable(acct, err)
	heartbeatUnavailableLogGate.Lock()
	cleared := heartbeatUnavailableLogGate.suppres[acct]
	heartbeatUnavailableLogGate.Unlock()
	if cleared != 0 {
		t.Fatalf("suppression counter should reset after emitting, got %d", cleared)
	}

	heartbeatUnavailableLogGate.Lock()
	delete(heartbeatUnavailableLogGate.last, acct)
	delete(heartbeatUnavailableLogGate.suppres, acct)
	heartbeatUnavailableLogGate.Unlock()
}

// 其它错误不能被误判成「心跳接口不存在」，否则会把真正的失败降级掉。
func TestIsHeartbeatUnavailableOnlyFor0101(t *testing.T) {
	for _, msg := range []string{
		`chat-session busy rejected: HTTP 403 {"error_code":"TM.00001041"}`,
		`chat-session busy rejected: HTTP 500 internal`,
		`transport error: connection reset`,
	} {
		if isHeartbeatUnavailable(errors.New(msg)) {
			t.Fatalf("must not degrade on %q", msg)
		}
	}
}
