package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

// adminOverview 面板总览。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	total, healthy, disabled, cooling, credits := h.cfg.Pool.Stats()
	models := h.modelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{"id": m["id"]})
	}
	accounts := h.cfg.Pool.List()
	matrixModels := h.matrixModels(accounts)
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "codearts2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
			"credits":  credits,
		},
		"accounts": accounts,
		"models":   ids,
		"availability": map[string]any{
			"ttl_seconds": int(availabilityTTL / time.Second),
			"models":      matrixModels,
			"cells":       availabilitySnapshot(accounts, matrixModels),
		},
		"body_limit": h.bodyLimitStats(),
		"cli":        h.cliOverview(),
		"schedule": map[string]any{
			"watch": h.cfg.WatchInfo,
		},
	})
}

// matrixModels 矩阵列：各账号模型目录的并集 + CLI 模型；不用 modelList()
// 的过滤结果，否则「所有账号都不可用」的模型会直接从矩阵里消失。
func (h *Handler) matrixModels(accounts []map[string]any) []string {
	seen := make(map[string]string)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; !ok {
			seen[key] = id
		}
	}
	for _, a := range accounts {
		uid, _ := a["uid"].(string)
		models, _ := upstream.AccountModels(uid)
		for _, m := range models {
			add(m.ID)
		}
	}
	for id := range h.cfg.CLIModels {
		add(id)
	}
	list := make([]string, 0, len(seen))
	for _, id := range seen {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i]) < strings.ToLower(list[j]) })
	if len(list) > 40 {
		list = list[:40]
	}
	return list
}

// adminRecent 最近请求（新→旧）。只存内存，重启清空。
func (h *Handler) adminRecent(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= recentRequestsCap {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capacity": recentRequestsCap,
		"items":    h.recent.snapshot(limit),
	})
}

type probeBody struct {
	UID   string `json:"uid"`
	Model string `json:"model"`
}

// adminProbe 手动探活单个「账号 × 模型」。同步执行，只探一格。
func (h *Handler) adminProbe(w http.ResponseWriter, r *http.Request) {
	var req probeBody
	defer r.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if rerr != nil || json.Unmarshal(raw, &req) != nil || req.UID == "" || req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid 和 model 必填"})
		return
	}
	acct := h.cfg.Pool.Get(req.UID)
	if acct == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	res := h.probeModelOnce(r.Context(), acct, req.Model, true)
	msg := res.message
	if res.state == "usable" {
		msg = "可用"
	} else if res.state == "unusable" && res.reason != "" {
		msg = "不可用：" + res.reason
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      res.state == "usable",
		"uid":     req.UID,
		"model":   req.Model,
		"state":   res.state,
		"reason":  res.reason,
		"skipped": res.skipped,
		"message": msg,
	})
}

// cliOverview CLI 后端状态（健康检查缓存 15 秒，避免面板每次刷新都等 serve）。
func (h *Handler) cliOverview() map[string]any {
	out := map[string]any{
		"enabled":           h.cfg.CLIEnabled,
		"endpoint":          h.cfg.CLIEndpoint,
		"provider":          h.cfg.CLIProvider,
		"agent":             h.cfg.CLIAgent,
		"model_count":       len(h.cfg.CLIModels),
		"multimodal_models": h.cfg.CLIMultimodal,
		"healthy":           false,
		"checked_at":        "",
	}
	if !h.cfg.CLIEnabled {
		return out
	}
	if h.cfg.CLI == nil {
		out["message"] = "CLI 后端未就绪（启动失败或 serve 未连上）"
		return out
	}
	h.cliMu.Lock()
	healthy, checked, msg := h.cliHealthy, h.cliChecked, h.cliErr
	fresh := !checked.IsZero() && time.Since(checked) < 15*time.Second
	h.cliMu.Unlock()
	if !fresh {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		healthy = h.cfg.CLI.Healthy(ctx)
		cancel()
		checked = time.Now()
		if healthy {
			msg = ""
		} else {
			msg = "serve /session 不可达"
		}
		h.cliMu.Lock()
		h.cliHealthy, h.cliChecked, h.cliErr = healthy, checked, msg
		h.cliMu.Unlock()
	}
	out["healthy"] = healthy
	out["checked_at"] = checked.Format(time.RFC3339)
	if msg != "" {
		out["message"] = msg
	}
	return out
}

type uidBody struct {
	UID     string `json:"uid"`
	Account string `json:"account"`
	Reason  string `json:"reason"`
}

func readUIDBody(r *http.Request) (uidBody, error) {
	var b uidBody
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.UID == "" {
		b.UID = b.Account
	}
	return b, nil
}

type actionResult struct {
	UID     string  `json:"uid"`
	OK      bool    `json:"ok"`
	Credits float64 `json:"credits,omitempty"`
	Message string  `json:"message,omitempty"`
}

// adminCredits 保留旧路由，校验/刷新登录态；不把它当作真实积分查询。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, uid := range targets {
		uid := uid
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			acct := h.cfg.Pool.Get(uid)
			res := actionResult{UID: uid}
			if acct == nil {
				res.Message = "no account"
			} else if ok, err := h.cfg.Pool.Validate(acct); err != nil {
				res.Message = err.Error()
			} else if !ok {
				res.Message = "token invalid"
			} else {
				res.OK = true
				res.Message = "ok"
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("刷新状态", results), "results": results,
	})
}

// adminCheckin 领取指定账号或全部启用账号的签到/新手活动积分。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	if body.UID != "" && h.cfg.Pool.Get(body.UID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	results := make([]actionResult, 0)
	for _, uid := range h.pickTargets(body.UID) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		result, err := h.cfg.Pool.Checkin(ctx, uid)
		cancel()
		res := actionResult{UID: uid, OK: result.OK, Credits: result.Credits, Message: result.Message}
		if err != nil {
			res.OK, res.Message = false, err.Error()
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("签到/新手领取", results), "results": results,
	})
}

// adminKeepalive 刷新即将过期的 token。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	for _, uid := range targets {
		acct := h.cfg.Pool.Get(uid)
		res := actionResult{UID: uid}
		if acct == nil {
			res.Message = "no account"
			results = append(results, res)
			continue
		}
		// Validate 内部会在临近过期时自动 Refresh
		ok, verr := h.cfg.Pool.Validate(acct)
		if verr != nil {
			res.Message = verr.Error()
		} else if !ok {
			res.Message = "token invalid"
		} else {
			res.OK = true
			res.Message = "refreshed/validated"
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": allOK(results), "message": summaryMsg("保活", results), "results": results,
	})
}

func (h *Handler) adminReload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	auths, err := auth.LoadDir(h.cfg.AuthDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.cfg.Pool.SyncToDir(auths)
	total, healthy, _, _, _ := h.cfg.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已重载 auths", "loaded": len(auths), "total": total, "healthy": healthy,
	})
}

func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.Enable(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已启用 " + body.UID})
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual disable"
	}
	h.cfg.Pool.Disable(body.UID, reason)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已禁用 " + body.UID})
}

func (h *Handler) adminClearCooldown(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	if !h.cfg.Pool.ClearCooldown(body.UID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清冷却 " + body.UID})
}

func (h *Handler) pickTargets(uid string) []string {
	if uid != "" {
		if h.cfg.Pool.Get(uid) == nil {
			return nil
		}
		return []string{uid}
	}
	out := make([]string, 0)
	for _, st := range h.cfg.Pool.List() {
		if disabled, _ := st["disabled"].(bool); disabled {
			continue
		}
		if u, ok := st["uid"].(string); ok && u != "" {
			out = append(out, u)
		}
	}
	return out
}

func allOK(results []actionResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

func summaryMsg(action string, results []actionResult) string {
	if len(results) == 0 {
		return action + ": 无目标账号"
	}
	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	if fail == 0 {
		return action + "完成: " + itoa(ok) + " 成功"
	}
	return action + "完成: " + itoa(ok) + " 成功 / " + itoa(fail) + " 失败"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
