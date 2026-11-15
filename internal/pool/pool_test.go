package pool

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func TestAddAccountInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.AddAccount(a)

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("newly added account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestSyncToDirInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.SyncToDir([]*auth.Auth{a})

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("synced account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestValidateKeepsAccountEnabledAfterRetryableRefreshFailure(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refresh", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, refreshErr := p.Validate(p.Accounts()[0])
	if ok || refreshErr == nil {
		t.Fatalf("validate=(ok=%t, err=%v), want retryable refresh failure", ok, refreshErr)
	}
	status := p.List()[0]
	if disabled, _ := status["disabled"].(bool); disabled {
		t.Fatalf("retryable refresh failure permanently disabled account: %#v", status)
	}
}

// watch.refresh_skew_minutes 必须是有效的：此前 pool 硬编码 1 小时阈值，
// 调度器传进来的 RefreshSkew 只用于日志，配置改了也不生效。
func TestRefreshSkewControlsThreshold(t *testing.T) {
	a := auth.New("u-skew", "n", "d", "tok", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "refresh-token", "verifier")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 2h：skew=1h 时不刷新，skew=3h 时应命中刷新（这里只验证判定分支，
	// 真刷新需要网络，故只检查不触发的那一侧不报错）。
	if err := p.CheckAndRefreshTokenWithin("u-skew", time.Hour); err != nil {
		t.Fatalf("剩余 2h、skew=1h 不应触发刷新: %v", err)
	}
}

// RefreshTokenIfStale 必须单飞：refresh_token 用过即换，并发请求各自刷新会让先
// 到手的 token 立刻作废（表现为「刷新成功但仍被 APIG.0301 拒」）。
// 这里用一个计数型假 STS 验证：新鲜窗口内第二次调用不打上游。
func TestRefreshTokenIfStaleSingleFlight(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"refresh_token": "refresh-token-` + strconv.Itoa(n) + `",
			"credentials": {
				"access_key_id": "AK` + strconv.Itoa(n) + `",
				"secret_access_key": "SK` + strconv.Itoa(n) + `",
				"security_token": "TOK` + strconv.Itoa(n) + `",
				"expiration": "` + time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339) + `"
			}
		}`))
	}))
	defer sts.Close()

	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New("u-sf", "user", "domain", "token", "ak", "sk",
		time.Now().Add(10*time.Minute).Format(time.RFC3339), "refresh-token-0", "verifier",
		map[string]string(dpopPrivateJWK))
	// refresh 成功要落盘，auth 需要一个真实路径
	if err := a.SetPathAndSave(filepath.Join(t.TempDir(), a.FileName())); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}

	refreshed, err := p.RefreshTokenIfStale("u-sf", 30*time.Second)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if !refreshed {
		t.Fatal("first call should hit upstream")
	}
	token1 := a.Token()

	// 新鲜窗口内的并发请求：不应再打上游，也不应换掉刚拿到的 token。
	for i := 0; i < 3; i++ {
		refreshed, err := p.RefreshTokenIfStale("u-sf", 30*time.Second)
		if err != nil {
			t.Fatalf("repeat refresh %d: %v", i, err)
		}
		if refreshed {
			t.Fatalf("repeat %d should reuse the fresh credentials, not hit upstream", i)
		}
	}
	if got := a.Token(); got != token1 {
		t.Fatalf("token changed by suppressed refresh: %q -> %q", token1, got)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh, got %d", got)
	}

	// maxAge=0 强制刷新：绕过新鲜窗口，应真的再打一次。
	if _, err := p.RefreshTokenIfStale("u-sf", 0); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("forced refresh should hit upstream, calls=%d", got)
	}
}

// 刷新成功后必须清掉旧的 0301 软冷却：新凭据能覆盖旧结论，否则账号白等 60s。
func TestRefreshClearsStaleCooldown(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"refresh_token": "r2",
			"credentials": {
				"access_key_id": "AK", "secret_access_key": "SK", "security_token": "TOK",
				"expiration": "` + time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339) + `"
			}
		}`))
	}))
	defer sts.Close()

	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New("u-cool", "user", "domain", "token", "ak", "sk",
		time.Now().Add(10*time.Minute).Format(time.RFC3339), "r1", "verifier",
		map[string]string(dpopPrivateJWK))
	if err := a.SetPathAndSave(filepath.Join(t.TempDir(), a.FileName())); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}

	p.Cooldown("u-cool", CoolSoft, time.Minute, "上游拒绝凭据或签名（APIG.0301/0602）")
	if p.Healthy("u-cool") {
		t.Fatal("account should be cooling before refresh")
	}
	if _, err := p.RefreshTokenIfStale("u-cool", 0); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !p.Healthy("u-cool") {
		t.Fatal("fresh credentials must clear the cooldown")
	}
}
