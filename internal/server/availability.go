package server

import (
	"bufio"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"codearts2api/internal/pool"
	"codearts2api/internal/upstream"
)

// 模型可用性探测。
//
// 背景（2026-09-15 实测）：上游 /v1/models 的发现结果 ≠ 账号真能调用的模型。
// agent-center 与 /v1/model/builtin 会列出未在当前账号注册的模型（实测
// GLM-5.2-ArkTS-SPARK / OpenPangu-2.0-Pro / OpenPangu-2.0-Flash 全部返回
// InferHub.002002009.404），福利模型在未领取时返回 InferHub.4004.200
// benefit not found。只按发现结果列模型，客户端会拿到一堆必然失败的条目。
//
// 因此这里做一次轻量真实探测（一条最小 chat 请求），把结果缓存起来：
//   - /v1/models 过滤掉已知不可用的模型
//   - 聊天路由避开不可用模型
//   - 上游报 not registered / benefit not found 时立即记为不可用（从真实流量学习）

const (
	// availabilityTTL 探测结果有效期。套餐/福利变化后最多滞后这么久。
	availabilityTTL = 30 * time.Minute
	// probeTimeout 单次探测超时。
	probeTimeout = 45 * time.Second
	// probeInterval 后台批量探测间隔。
	probeInterval = 15 * time.Minute
	// probeStartDelay 启动后多久开始首轮探测（避开接入初期的真实请求）。
	probeStartDelay = 2 * time.Minute
	// probeIdleBefore 账号至少空闲这么久才探测。真实请求本身就会写可用性结论
	// （成功 markUsable / 失败 markUnusable），探测只是空闲时补齐，避免与用户
	// 抢上游那 3 个并发会话槽位（槽位释放实测 >15s）。
	probeIdleBefore = 10 * time.Minute
	// probeGap 两次探测之间的间隔。探针本身是串行的，但它占用的上游会话槽位
	// 释放很慢（实测 >15s）：连续探 3 个就占满单账号的 3 个并发会话，紧随其后的
	// 真实请求会被上游判为 TM.00001041 并开始排队。留出释放窗口再探下一个。
	probeGap = 20 * time.Second
	// probeConcurrency 并发探测数。探测本身会占上游会话槽位（单账号 3 个并发），
	// 且账号并发锁默认 max_concurrent=1，所以这里也用 1：慢一点，但不能干扰真实请求。
	probeConcurrency = 1
)

type availState int

const (
	availUnknown availState = iota
	availUsable
	availUnusable
)

type availEntry struct {
	state  availState
	reason string
	at     time.Time
}

// probesStop 在服务退出时关闭，让带 sleep 的探测循环尽快结束。
var probesStop = make(chan struct{})

var availability = struct {
	sync.Mutex
	byKey     map[string]availEntry // accountID|modelLower -> 状态
	probing   map[string]bool       // 正在探测的 key（去重）
	sweeping  bool                  // 是否已有一轮后台批量探测在跑
	lastSweep time.Time
	// lastTraffic accountID -> 最后一次真实请求时间。探测只为「补齐结论」，
	// 有真实流量的账号不探测（真实请求本身就会写结论，且探测占用的上游会话
	// 槽位释放很慢，会挤掉用户请求）。
	lastTraffic map[string]time.Time
}{byKey: map[string]availEntry{}, probing: map[string]bool{}, lastTraffic: map[string]time.Time{}}

// anonAccountID 用于 user_id 为空的账号。码道的 auths 文件里 user_id 常为空
// （实测该账号 user_id/user_name 都是空串但完全可用），以前 markUnusable /
// markUsable 遇到空 ID 直接 return，导致可用性缓存对这类账号彻底失效：
// /v1/models 会把坏模型一直列着，每次调用都撞一遍 404。单账号部署下用固定
// 哨兵值即可正确归并——多账号部署的 user_id 都不会为空。
const anonAccountID = "\x00anon"

// keyAccountID 把空账号 ID 归一成哨兵值，避免可用性缓存静默失效。
func keyAccountID(accountID string) string {
	if accountID == "" {
		return anonAccountID
	}
	return accountID
}

func availKey(accountID, model string) string {
	return keyAccountID(accountID) + "|" + strings.ToLower(model)
}

// modelAvailability 返回缓存的可用性；过期视为未知。
func modelAvailability(accountID, model string) (availState, string) {
	availability.Lock()
	defer availability.Unlock()
	e, ok := availability.byKey[availKey(accountID, model)]
	if !ok || time.Since(e.at) > availabilityTTL {
		return availUnknown, ""
	}
	return e.state, e.reason
}

// markUnusable 记录某账号下某模型不可用（来自探测或真实请求失败）。
func markUnusable(accountID, model, reason string) {
	if model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUnusable, reason: reason, at: time.Now()}
}

// markUsable 记录某账号下某模型可用。
func markUsable(accountID, model string) {
	if model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUsable, at: time.Now()}
}

// noteTraffic 记录账号上一次真实请求时间（探测据此避让）。
func noteTraffic(accountID string) {
	availability.Lock()
	defer availability.Unlock()
	availability.lastTraffic[keyAccountID(accountID)] = time.Now()
}

// trafficIdleFor 返回账号距上次真实请求的空闲时长；从未有流量返回极大值。
func trafficIdleFor(accountID string) time.Duration {
	availability.Lock()
	defer availability.Unlock()
	t, ok := availability.lastTraffic[keyAccountID(accountID)]
	if !ok {
		return time.Duration(1<<62 - 1)
	}
	return time.Since(t)
}

// ---------------------------------------------------------------------------
// 上游错误分类
// ---------------------------------------------------------------------------
//
// 这套服务最容易出事的地方不是协议转换，而是「错误归谁」：把模型级/请求级
// 错误当成账号故障，单账号池就会整池冷却甚至永久禁用。两次真实事故：
//   - 1109 glm-5.3-flash 的 APIG.0101 落进 5xx 分支 → 整池冷却 10 分钟
//   - 1110 一次 401 APIG.0301 直接 Disable → 账号永久禁用，watchdog 跳过，
//     凭据不再刷新，直到手工改 state.json 才恢复
//
// 所以这里把分类显式化，并规定每类错误对账号状态的影响。判据用「完整错误码」
// 匹配（带前后边界），不匹配裸子串：第三方项目有 `"1005" in text` 把
// InferHub.001001005（参数非法）误判成权益耗尽的真实事故。

type upstreamErrorKind int

const (
	errKindUnknown upstreamErrorKind = iota
	// errKindModelUnavailable 该账号不能用这个模型（未注册/未开通/福利过期/路由未发布）。
	// 换账号可能有用，但不处罚账号，只记录「账号×模型」不可用。
	errKindModelUnavailable
	// errKindAuth 凭据/签名问题：401、APIG.0301、APIG.0602。可刷新一次后重试。
	errKindAuth
	// errKindAuthExpired refresh_token 终态失效（STS5.1806）：只能重新登录。
	errKindAuthExpired
	// errKindParamInvalid 请求形状或模型能力不匹配（001001005 参数非法、
	// 001001020 多模态不匹配）。换账号结果相同，不轮换、不冷却。
	errKindParamInvalid
	// errKindContentModeration 本次内容被上游内容审查拒绝。换账号/换措辞都可能
	// 过不了，但不是账号故障。
	errKindContentModeration
	// errKindConcurrentLimit 并发会话/排队（TM.00001041、TPM 排队）。瞬时。
	errKindConcurrentLimit
	// errKindRateLimit 429 / 模型级限流。
	errKindRateLimit
	// errKindServer 真正的上游 5xx。
	errKindServer
)

func (k upstreamErrorKind) String() string {
	switch k {
	case errKindModelUnavailable:
		return "model_unavailable"
	case errKindAuth:
		return "auth"
	case errKindAuthExpired:
		return "auth_expired"
	case errKindParamInvalid:
		return "param_invalid"
	case errKindContentModeration:
		return "content_moderation"
	case errKindConcurrentLimit:
		return "concurrent_limit"
	case errKindRateLimit:
		return "rate_limit"
	case errKindServer:
		return "server"
	}
	return "unknown"
}

func isAlnumByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// containsCode 按「完整错误码」匹配：前后必须是边界，避免 001001005 里的
// 1005 被当成独立码。msg 必须已转小写。
func containsCode(low, code string) bool {
	if code == "" {
		return false
	}
	for i := 0; ; {
		idx := strings.Index(low[i:], code)
		if idx < 0 {
			return false
		}
		idx += i
		beforeOK := idx == 0 || !isAlnumByte(low[idx-1])
		end := idx + len(code)
		afterOK := end >= len(low) || !isAlnumByte(low[end])
		if beforeOK && afterOK {
			return true
		}
		i = idx + 1
	}
}

// upstreamErrorText 提取上游错误的可判定文本（码 + 说明），兼容两类错误类型。
func upstreamErrorText(err error) string {
	if err == nil {
		return ""
	}
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.Code + " " + ue.Msg
	}
	var ae *upstream.ApiError
	if errors.As(err, &ae) {
		return ae.Message
	}
	return err.Error()
}

// classifyUpstreamError 把错误分类成 (类别, 人类可读原因)。
func classifyUpstreamError(err error) (upstreamErrorKind, string) {
	return classifyUpstreamMessage(upstreamErrorText(err))
}

// classifyUpstreamMessage 对一段上游错误文本分类。判据顺序即优先级：
// 越「具体、越不该罚账号」的类别越靠前。
func classifyUpstreamMessage(msg string) (upstreamErrorKind, string) {
	low := strings.ToLower(msg)
	switch {
	// --- 终态凭据失效：必须重新登录 ---
	case containsCode(low, "sts5.1806") || strings.Contains(low, "invalid refresh token") ||
		strings.Contains(low, "refresh token has been used") || strings.Contains(low, "invalid client id"):
		return errKindAuthExpired, "refresh_token 已失效，需要重新登录（/admin/api/oauth/start 或 codearts2api login）"

	// --- 内容审查：本次内容被拒 ---
	case strings.Contains(low, "modelarts.81011") || strings.Contains(low, "抱歉，我无法提供关于您提问内容的建议"):
		return errKindContentModeration, "上游内容审查拒绝了本次请求"

	// --- 请求形状 / 模型能力不匹配：换账号无用 ---
	case containsCode(low, "001001005") || strings.Contains(low, "request param is invalid"):
		return errKindParamInvalid, "上游判定请求参数非法（InferHub.001001005）"
	case containsCode(low, "001001020") || strings.Contains(low, "multimodal"):
		return errKindParamInvalid, "该模型不支持本次消息形态（多模态不匹配，InferHub.001001020）"

	// --- 模型级不可用：不罚账号，记「账号×模型」 ---
	case containsCode(low, "002002009") || strings.Contains(low, "not registered"):
		return errKindModelUnavailable, "模型未在当前账号注册"
	case containsCode(low, "4004.200") || strings.Contains(low, "benefit not found"):
		return errKindModelUnavailable, "限时福利未领取或已过期（可执行 go run ./cmd/models -claim，或打开 benefit_auto_claim）"
	case containsCode(low, "000010051") || strings.Contains(low, "无权调用该模型"):
		return errKindModelUnavailable, "账号无权调用该模型（企业侧未开通）"
	case containsCode(low, "apig.0101") || strings.Contains(low, "api does not exist or has not been published"):
		return errKindModelUnavailable, "模型在上游网关上不可用（未发布或路由不存在）"

	// --- 并发/排队：瞬时 ---
	case containsCode(low, "00001041") || strings.Contains(low, "并发会话"):
		return errKindConcurrentLimit, "上游并发会话已达上限（瞬时，可重试）"

	// --- 限流 ---
	case strings.Contains(low, "modelarts.81111.429") || strings.Contains(low, "tpm limit") ||
		strings.Contains(low, "rate limit"):
		return errKindRateLimit, "上游限流（429/TPM）"

	// --- 凭据/签名：可刷新一次 ---
	case containsCode(low, "apig.0301") || strings.Contains(low, "decrypt token fail") ||
		containsCode(low, "apig.0602") || strings.Contains(low, "verify ak sk signature fail") ||
		strings.Contains(low, "token expired"):
		return errKindAuth, "上游拒绝凭据或签名（APIG.0301/0602）"
	}
	return errKindUnknown, ""
}

// upstreamUnavailableReason 把上游错误归类为「该模型对这个账号不可用」，否则返回空串。
func upstreamUnavailableReason(err error) string {
	kind, reason := classifyUpstreamError(err)
	if kind != errKindModelUnavailable {
		return ""
	}
	return reason
}

// probeModel 用一条最小请求真实试一次该模型，并记录结果。
//
// 探测会占用上游会话槽位（单账号只有 3 个并发会话），因此这里必须走账号的并发锁：
// 拿不到锁就让位给真实请求，等下一轮再试——否则用户请求会被探测挤成
// TM.00001041「并发会话数已达上限」。
func (h *Handler) probeModel(acct *pool.Account, model string) {
	if acct == nil || acct.Auth == nil {
		return
	}
	// 账号一旦有真实请求在跑（或刚跑过），本轮探测整体让位：探测只是为了让
	// /v1/models 说真话，绝不能因此让用户请求排队等锁。下一轮再试。
	if h.cfg.Pool.Busy(acct.Name) || trafficIdleFor(acct.UID) < probeIdleBefore {
		return
	}
	if !h.cfg.Pool.AcquireLock(acct.Name) {
		return
	}
	defer h.cfg.Pool.ReleaseLock(acct.Name)
	key := availKey(acct.UID, model)
	availability.Lock()
	if availability.probing[key] {
		availability.Unlock()
		return
	}
	availability.probing[key] = true
	availability.Unlock()
	defer func() {
		availability.Lock()
		delete(availability.probing, key)
		availability.Unlock()
	}()

	token, ak, sk := acct.Auth.Credentials()
	cred := upstream.SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	body := map[string]any{
		"model":    upstream.CanonicalModel(model),
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	rc, err := acct.Client.SendChatV2(ctx, body, "", cred, cred.SecurityToken,
		upstream.IsBenefitModel(acct.UID, model))
	if err != nil {
		// 并发上限是暂时的，不代表模型不可用。
		if strings.Contains(err.Error(), "TM.00001041") {
			return
		}
		if reason := upstreamUnavailableReason(err); reason != "" {
			markUnusable(acct.UID, model, reason)
			log.Printf("probe %s/%s: 不可用：%s", acct.Name, model, reason)
			return
		}
		log.Printf("probe %s/%s: 未判定 %v", acct.Name, model, err)
		return
	}
	defer rc.Close()
	// 读到首个事件即可：200 也要看内容，上游会把错误塞在 200 的 SSE 里。
	br := bufio.NewReaderSize(rc, 64*1024)
	var head strings.Builder
	for i := 0; i < 6; i++ {
		line, err := br.ReadString('\n')
		head.WriteString(line)
		if err != nil {
			break
		}
	}
	headStr := head.String()
	if reason := upstreamUnavailableReasonText(headStr); reason != "" {
		markUnusable(acct.UID, model, reason)
		log.Printf("probe %s/%s: 不可用：%s", acct.Name, model, reason)
		return
	}
	if headStr == "" {
		return
	}
	markUsable(acct.UID, model)
	log.Printf("probe %s/%s: 可用", acct.Name, model)
}

// upstreamUnavailableReasonText 同 upstreamUnavailableReason，作用于响应正文。
func upstreamUnavailableReasonText(s string) string {
	kind, reason := classifyUpstreamMessage(s)
	if kind != errKindModelUnavailable {
		return ""
	}
	return reason
}

// StartAvailabilityProber 供 main 启动后台探测器。
func (h *Handler) StartAvailabilityProber(ctx context.Context) { h.startAvailabilityProber(ctx) }

// startAvailabilityProber 后台周期性探测：把「发现到但还没结论」的模型试一遍。
// 只在有账号且拿到目录后才干活，避免空转打上游。
func (h *Handler) startAvailabilityProber(ctx context.Context) {
	go func() {
		// 启动后先等服务预热：立刻探测会和刚接入的客户端抢账号槽位。
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeStartDelay):
		}
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		h.sweepAvailability()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.sweepAvailability()
			}
		}
	}()
}

// sweepAvailability 对所有健康账号的已发现模型做一轮探测（跳过已有结论的）。
func (h *Handler) sweepAvailability() {
	availability.Lock()
	if time.Since(availability.lastSweep) < probeInterval/2 {
		availability.Unlock()
		return
	}
	availability.lastSweep = time.Now()
	availability.Unlock()

	for _, acct := range h.pendingProbes() {
		h.probeModel(acct.acct, acct.model)
	}
}

// pendingProbe 一个待探测的「账号 + 模型」。
type pendingProbe struct {
	acct  *pool.Account
	model string
}

// pendingProbes 列出所有「健康账号 × 已发现但还没有可用性结论」的组合。
func (h *Handler) pendingProbes() []pendingProbe {
	var out []pendingProbe
	for _, acct := range h.cfg.Pool.Accounts() {
		if acct == nil || acct.Auth == nil || !h.cfg.Pool.Healthy(acct.Name) {
			continue
		}
		models, ok := upstream.AccountModels(acct.UID)
		if !ok {
			continue
		}
		for _, mi := range models {
			if st, _ := modelAvailability(acct.UID, mi.ID); st != availUnknown {
				continue
			}
			out = append(out, pendingProbe{acct: acct, model: mi.ID})
		}
	}
	return out
}

// runProbes 并发执行待探测列表（上限 probeConcurrency）。
func (h *Handler) runProbes(items []pendingProbe) {
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	first := true
	for _, it := range items {
		// 让上一个探针占用的上游会话先释放，再探下一个。
		if !first {
			select {
			case <-time.After(probeGap):
			case <-probesStop:
				return
			}
		}
		first = false
		wg.Add(1)
		sem <- struct{}{}
		go func(it pendingProbe) {
			defer wg.Done()
			defer func() { <-sem }()
			h.probeModel(it.acct, it.model)
		}(it)
	}
	wg.Wait()
}
