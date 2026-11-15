package auth

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 从 refresh_token 解出账号身份。
//
// 背景（2026-11-15）：码道登录成功后，auths/codearts-unknown.json 里
// user_id / user_name / domain_id 全是空串——portal 回调只回 code，不带身份，
// 而 STS caller-identity / current/user 又常打不通，于是一直以 "unknown" 落盘。
// 结果：日志里 account= 全空、账号池按 UserID 索引退化成空键、
// state.json.chats.json 的键是 ""、多个模型共用一个 chat_id。
//
// 身份其实就在 refresh_token 里：它是 JWT，payload 的 user_profile 字段是
// base64(JSON)，含 account_id / account_name / principal_id / principal_urn：
//
//	{"account_id":"01a1...","account_name":"fkfbnv144",
//	 "principal_id":"01a1...","principal_urn":"iam::01a1...:user:fkfbnv144"}
//
// 注意 JWT 里**没有** sub claim，所以旧的 identityFromRefreshToken 只能返回空串。

// RefreshIdentity 从 refresh_token 解出的身份。
type RefreshIdentity struct {
	UserID   string // principal_id（= 旧实现想取的 sub）
	UserName string // account_name / principal_urn 尾段
	DomainID string // account_id
}

// Empty 报告是否没解出任何身份。
func (i RefreshIdentity) Empty() bool {
	return i.UserID == "" && i.UserName == "" && i.DomainID == ""
}

// IdentityFromRefreshToken 解 JWT 形式的 refresh_token 里的 user_profile。
// 解不出来返回零值（调用方按「没拿到身份」处理，不报错——旧格式/非 JWT 都要能过）。
func IdentityFromRefreshToken(tok string) RefreshIdentity {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return RefreshIdentity{}
	}
	payload, err := decodeJWTPart(parts[1])
	if err != nil {
		return RefreshIdentity{}
	}
	var claims struct {
		UserProfile string `json:"user_profile"`
		// 少数签发变体直接把这些放顶层，一并兼容。
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		DomainID string `json:"domain_id"`
		Sub      string `json:"sub"`
		Account  string `json:"account_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return RefreshIdentity{}
	}

	var id RefreshIdentity
	if claims.UserProfile != "" {
		if prof, err := decodeJWTPart(claims.UserProfile); err == nil {
			var p struct {
				AccountID    string `json:"account_id"`
				AccountName  string `json:"account_name"`
				PrincipalID  string `json:"principal_id"`
				PrincipalURN string `json:"principal_urn"`
			}
			if json.Unmarshal(prof, &p) == nil {
				id.UserID = p.PrincipalID
				id.UserName = p.AccountName
				id.DomainID = p.AccountID
				if id.UserName == "" {
					// principal_urn 形如 iam::<account>:user:<name>
					if i := strings.LastIndex(p.PrincipalURN, ":user:"); i >= 0 {
						id.UserName = p.PrincipalURN[i+len(":user:"):]
					}
				}
			}
		}
	}

	// user_profile 缺失/解不开时退回顶层字段。
	if id.UserID == "" {
		id.UserID = firstNonEmptyStr(claims.UserID, claims.Sub)
	}
	if id.UserName == "" {
		id.UserName = claims.UserName
	}
	if id.DomainID == "" {
		id.DomainID = firstNonEmptyStr(claims.DomainID, claims.Account)
	}
	return id
}

// BackfillIdentities 用 refresh_token 里的身份回填缺失字段并落盘，返回更新后的列表。
//
// 只填空字段：已有值的账号一律不动（人工改过的、登录时已拿到身份的都不覆盖）。
// 顺带把 codearts-unknown.json 这类空 ID 文件名改成 codearts-<user_id>.json，
// 否则账号池/日志里永远是 "unknown"，多账号时分不清谁是谁。
func BackfillIdentities(auths []*Auth, dir string) []*Auth {
	for _, a := range auths {
		if a == nil {
			continue
		}
		if a.UserID != "" && a.UserName != "" && a.DomainID != "" {
			continue
		}
		id := IdentityFromRefreshToken(a.Refresh())
		if id.Empty() {
			log.Printf("auth identity: %s has no identity and refresh_token carries none (re-login to fix)",
				a.FileName())
			continue
		}
		if err := a.UpdateIdentity(id.UserID, id.UserName, id.DomainID); err != nil {
			log.Printf("auth identity: backfill %s failed: %v", a.FileName(), err)
			continue
		}
		log.Printf("auth identity: backfilled user_id=%s name=%s domain=%s (from refresh_token)",
			a.UserID, a.UserName, a.DomainID)
		renameAuthFile(a, dir)
	}
	return auths
}

// renameAuthFile 把空 ID 的 auth 文件改名成 codearts-<user_id>.json。
// 目标已存在时保留原文件不覆盖（宁可留两份，也不丢凭据）。
func renameAuthFile(a *Auth, dir string) {
	if dir == "" || a.UserID == "" {
		return
	}
	want := filepath.Join(dir, a.FileName())
	if filepath.Clean(want) == filepath.Clean(a.path) {
		return
	}
	if _, err := os.Stat(want); err == nil {
		log.Printf("auth identity: %s not renamed to %s (target exists)", filepath.Base(a.path), a.FileName())
		return
	}
	if err := os.Rename(a.path, want); err != nil {
		log.Printf("auth identity: rename %s -> %s failed: %v", filepath.Base(a.path), a.FileName(), err)
		return
	}
	oldName := filepath.Base(a.path)
	a.SetPath(want)
	log.Printf("auth identity: renamed %s -> %s", oldName, a.FileName())
}

// decodeJWTPart 解 base64url 片段，兼容有无 padding 两种写法。
func decodeJWTPart(s string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
