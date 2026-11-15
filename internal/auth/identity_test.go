package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// makeRefreshToken 造一个和上游同形状的 refresh_token：
// JWT（无 sub claim）+ payload.user_profile = base64(JSON)。
// 形状取自 2026-11-15 本机真实凭据（见 auths/codearts-unknown.json）。
func makeRefreshToken(t *testing.T, profile map[string]any) string {
	t.Helper()
	prof, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"exp":          1795442649,
		"iat":          1794718799,
		"iss":          "cn-north-4",
		"type":         "refreshToken",
		"user_profile": base64.StdEncoding.EncodeToString(prof),
		"client_id":    "codearts-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"typ":"JWT","alg":"RS256"}`)) + "." + enc(payload) + ".signature"
}

func TestIdentityFromRefreshTokenUserProfile(t *testing.T) {
	tok := makeRefreshToken(t, map[string]any{
		"account_id":             "01a1487cf8be7e62a4a8b1c02dc68124",
		"account_name":           "fkfbnv144",
		"principal_id":           "01a1487cf8be7566979ff14189fb5e2d",
		"principal_is_root_user": true,
		"principal_urn":          "iam::01a1487cf8be7e62a4a8b1c02dc68124:user:fkfbnv144",
	})

	id := IdentityFromRefreshToken(tok)
	if id.UserID != "01a1487cf8be7566979ff14189fb5e2d" {
		t.Fatalf("user_id = %q", id.UserID)
	}
	if id.UserName != "fkfbnv144" {
		t.Fatalf("user_name = %q", id.UserName)
	}
	if id.DomainID != "01a1487cf8be7e62a4a8b1c02dc68124" {
		t.Fatalf("domain_id = %q", id.DomainID)
	}
	if id.Empty() {
		t.Fatal("identity must not be empty")
	}
}

// account_name 缺失时从 principal_urn 尾段取名字。
func TestIdentityFromRefreshTokenURNFallback(t *testing.T) {
	tok := makeRefreshToken(t, map[string]any{
		"account_id":    "acct-1",
		"principal_id":  "pid-1",
		"principal_urn": "iam::acct-1:user:alice",
	})
	id := IdentityFromRefreshToken(tok)
	if id.UserName != "alice" {
		t.Fatalf("user_name from urn = %q", id.UserName)
	}
}

// 没有 sub claim 的 JWT 不应被误判（旧实现取 sub 只能拿到空串）。
func TestIdentityIgnoresMissingSub(t *testing.T) {
	tok := makeRefreshToken(t, map[string]any{"account_name": "bob", "account_id": "a", "principal_id": "p"})
	id := IdentityFromRefreshToken(tok)
	if id.UserID != "p" || id.UserName != "bob" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

// 非 JWT / 空串 / 坏 base64 一律返回零值，不 panic、不报错。
func TestIdentityFromRefreshTokenMalformed(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b", "a.!!!.c", "eyJ9.eyJ9.x"} {
		if id := IdentityFromRefreshToken(tok); !id.Empty() {
			t.Fatalf("token %q should decode to empty identity, got %+v", tok, id)
		}
	}
}

// 顶层字段变体也要兼容。
func TestIdentityFromTopLevelClaims(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"user_id": "uid-top", "user_name": "topname", "domain_id": "dom-top",
	})
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte("{}")) + "." + enc(payload) + ".sig"
	id := IdentityFromRefreshToken(tok)
	if id.UserID != "uid-top" || id.UserName != "topname" || id.DomainID != "dom-top" {
		t.Fatalf("top-level claims not used: %+v", id)
	}
}

// BackfillIdentities 必须填空字段、保留已有值，并把 unknown 文件名改掉。
func TestBackfillIdentitiesFillsAndRenames(t *testing.T) {
	dir := t.TempDir()
	tok := makeRefreshToken(t, map[string]any{
		"account_id": "dom-1", "account_name": "fkfbnv144", "principal_id": "pid-1",
	})

	blank := &Auth{RefreshToken: tok, CloudDragonTok: "tok", AccessKeyID: "AK", SecretAccessKey: "SK"}
	blank.path = filepath.Join(dir, "codearts-unknown.json")
	if err := blank.Save(); err != nil {
		t.Fatal(err)
	}

	known := &Auth{
		UserID: "keep-me", UserName: "keep-name", DomainID: "keep-dom",
		RefreshToken: tok, CloudDragonTok: "tok2", AccessKeyID: "AK2", SecretAccessKey: "SK2",
	}
	known.path = filepath.Join(dir, "codearts-keep-me.json")
	if err := known.Save(); err != nil {
		t.Fatal(err)
	}

	out := BackfillIdentities([]*Auth{blank, known}, dir)
	if len(out) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(out))
	}
	if out[0].UserID != "pid-1" || out[0].UserName != "fkfbnv144" || out[0].DomainID != "dom-1" {
		t.Fatalf("blank account not backfilled: %+v", out[0])
	}
	if out[1].UserID != "keep-me" || out[1].UserName != "keep-name" {
		t.Fatalf("existing identity must not be overwritten: %+v", out[1])
	}

	// 空 ID 文件应改名，且新文件里带身份。
	if _, err := os.Stat(filepath.Join(dir, "codearts-pid-1.json")); err != nil {
		t.Fatalf("expected renamed auth file: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "codearts-pid-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Auth
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UserName != "fkfbnv144" {
		t.Fatalf("persisted identity missing: user_id=%q name=%q domain=%q",
			got.UserID, got.UserName, got.DomainID)
	}
}
