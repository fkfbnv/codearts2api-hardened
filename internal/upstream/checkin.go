package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	EpCreditDelivery = "/v1/ops/delivery"
	EpCreditClaim    = "/v1/ops/claim"
	EpCreditConfirm  = "/v1/ops/confirm"
	creditChannel    = "IDE"
)

// creditChannels 2026-11-02：新积分体系的「每日签到领100积分」投放在 PORTAL 通道，
// IDE 通道只见老活动（1030 上线时实测三次轮询均只领到新用户 4000）。
// delivery 轮询双通道并集，claim 按活动来源通道发送。
var creditChannels = []string{"IDE", "PORTAL"}

// CreditCampaign 是活动投放接口的脱敏条目；不保留 access 中的活动票据。
type CreditCampaign struct {
	ID                 int     `json:"campaignId"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	Claimable          bool    `json:"claimable"`
	BenefitUnit        string  `json:"benefitUnit"`
	BenefitAmount      float64 `json:"benefitAmount"`
	PendingTotalAmount float64 `json:"pendingTotalAmount"`
	// Type 活动类型（上游用 USER_LOGIN 标识每日签到）。campaignId 会随活动
	// 轮换而变化（1102 是 1，1111 已换号），按类型定位才稳。
	Type  string `json:"type"`
	Extra struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
		// TriggerEvent 与 Type 等价的触发事件标识（如 user.login）。
		TriggerEvent string `json:"triggerEvent"`
		Type         string `json:"type"`
	} `json:"extra"`

	// Channel 该条目来自哪个投放通道（不序列化），claim 时原样带回。
	Channel string `json:"-"`
}

type CreditClaimResult struct {
	CampaignID int     `json:"campaign_id"`
	Title      string  `json:"title"`
	OK         bool    `json:"ok"`
	Status     string  `json:"status"`
	Credits    float64 `json:"credits,omitempty"`
	Message    string  `json:"message"`
}

// CheckinResult 的 Credits 是本次领取量，不是账号余额。
type CheckinResult struct {
	OK        bool                `json:"ok"`
	Credits   float64             `json:"credits,omitempty"`
	Message   string              `json:"message"`
	Campaigns []CreditClaimResult `json:"campaigns"`
}

func (c *Client) creditRequest(ctx context.Context, method, path string, body any, cred SignCredential) (json.RawMessage, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.snapURL(path), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, raw, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: req.URL.Path, Message: truncateStr(string(response), 300)}
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("parse credit activity response: %w", err)
	}
	if envelope.Code == nil {
		return nil, fmt.Errorf("credit activity response missing business code")
	}
	if *envelope.Code != 0 {
		return nil, &ApiError{Code: *envelope.Code, Status: http.StatusBadRequest, Path: req.URL.Path, Message: envelope.Message}
	}
	return envelope.Data, nil
}

// FetchCreditCampaigns 只读查询活动，端点位于 snap 根路径而非 snap-manager。
// 2026-11-02 起轮询双通道（IDE+PORTAL）并集；PORTAL 覆盖同 ID 的 IDE 条目，
// 单通道失败不阻断（新体系活动只在 PORTAL 投放时 IDE 仍可用），两通道全失败才报错。
func (c *Client) FetchCreditCampaigns(ctx context.Context, cred SignCredential) ([]CreditCampaign, error) {
	merged := make(map[int]CreditCampaign)
	var firstErr error
	for _, ch := range creditChannels {
		raw, err := c.creditRequest(ctx, http.MethodGet, EpCreditDelivery+"?channel="+ch, nil, cred)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var data struct {
			Items *[]CreditCampaign `json:"items"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse credit campaigns(%s): %w", ch, err)
			}
			continue
		}
		if data.Items == nil {
			continue
		}
		for _, camp := range *data.Items {
			camp.Channel = ch
			merged[camp.ID] = camp
		}
	}
	if len(merged) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]CreditCampaign, 0, len(merged))
	for _, camp := range merged {
		out = append(out, camp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// claimChannel 返回该活动领取时应使用的投放通道。
func (camp CreditCampaign) claimChannel() string {
	if camp.Channel != "" {
		return camp.Channel
	}
	return creditChannel
}

// isClaimableCreditCampaign 判断某活动是否属于「签到 / 新手」类，需要领取。
//
// 2026-11-11 改为按活动类型定位：campaignId 会随活动轮换（1102 每日签到是 1，
// 1111 已变成 34），按 ID 匹配会静默失效。实测 delivery 返回：
//
//	34 每日签到领150积分      type=USER_LOGIN
//	40 Skill创新赛每日签到礼  type=USER_LOGIN
//	42 Skill创新赛报名礼      type=USER_LOGIN（2000 分，标题不含「签到」）
//	4  新用户注册送 4000 积分 type=NEW_USER_REGISTER
//
// 旧判据（ID 1/4 或标题含「签到」）会漏掉 42 这类一次性礼包。
// 学生认证(STUDENT_CERTIFIED) / 邀请(INVITE_USER) 不在此列：它们不是签到。
func isClaimableCreditCampaign(camp CreditCampaign) bool {
	typ := strings.ToUpper(strings.TrimSpace(firstNonEmpty(camp.Type, camp.Extra.Type)))
	switch typ {
	case "USER_LOGIN", "NEW_USER_REGISTER":
		return true
	}
	// 兼容上游没返回 type 的情况（老版本接口）：保留原判据作兜底。
	if camp.ID == 4 || camp.ID == 1 || strings.Contains(camp.Title, "签到") {
		return true
	}
	return false
}

// Checkin 领取每日签到（USER_LOGIN）和新用户（NEW_USER_REGISTER）积分。
// 上游决定资格和每日刷新，已领取不再 claim；CLAIMED 只补 confirm。
// 调用方应串行化同一账号的领取。
func (c *Client) Checkin(ctx context.Context, cred SignCredential) (CheckinResult, error) {
	campaigns, err := c.FetchCreditCampaigns(ctx, cred)
	if err != nil {
		return CheckinResult{Message: err.Error()}, err
	}
	out := CheckinResult{OK: true, Campaigns: make([]CreditClaimResult, 0)}
	for _, campaign := range campaigns {
		if !isClaimableCreditCampaign(campaign) {
			continue
		}
		if campaign.BenefitUnit != "" && campaign.BenefitUnit != "CREDIT" {
			continue
		}
		res := c.claimCreditCampaign(ctx, cred, campaign, time.Now())
		out.Campaigns = append(out.Campaigns, res)
		out.OK = out.OK && res.OK
		out.Credits += res.Credits
	}
	parts := make([]string, 0, len(out.Campaigns))
	for _, res := range out.Campaigns {
		parts = append(parts, res.Title+": "+res.Message)
	}
	out.Message = strings.Join(parts, "; ")
	if len(parts) == 0 {
		out.Message = "无可领取的签到或新手活动"
	}
	return out, nil
}

func (c *Client) claimCreditCampaign(ctx context.Context, cred SignCredential, campaign CreditCampaign, now time.Time) CreditClaimResult {
	res := CreditClaimResult{CampaignID: campaign.ID, Title: campaign.Title, Status: strings.TrimSpace(campaign.Status), OK: true}
	if res.Title == "" {
		res.Title = fmt.Sprintf("积分活动 %d", campaign.ID)
	}
	if end, err := time.Parse(time.RFC3339Nano, campaign.Extra.EndTime); err == nil && !now.Before(end) {
		res.Message = "活动已结束"
		return res
	}
	if start, err := time.Parse(time.RFC3339Nano, campaign.Extra.StartTime); err == nil && now.Before(start) {
		res.Message = "活动尚未开始"
		return res
	}
	switch res.Status {
	case "CONFIRMED", "CONSUMED":
		res.Message = "已领取"
		return res
	case "CLAIMED":
		// 已到账但尚未确认；不得重复领取。
	case "ELIGIBLE":
		if !campaign.Claimable {
			res.OK, res.Message = false, "暂不可领取，稍后重试"
			return res
		}
		_, err := c.creditRequest(ctx, http.MethodPost, EpCreditClaim, map[string]any{
			"campaignId": campaign.ID, "channel": campaign.claimChannel(),
			"idempotentKey": fmt.Sprintf("claim_%d_%d", campaign.ID, now.UnixMilli()),
		}, cred)
		if err != nil {
			res.OK, res.Message = false, err.Error()
			return res
		}
		res.Status = "CLAIMED"
		res.Credits = campaign.PendingTotalAmount
		if res.Credits <= 0 {
			res.Credits = campaign.BenefitAmount
		}
	case "":
		res.Message = "未获得活动资格"
		return res
	default:
		res.OK, res.Message = false, "未知活动状态: "+res.Status
		return res
	}
	if _, err := c.creditRequest(ctx, http.MethodPost, EpCreditConfirm, map[string]any{"campaignId": campaign.ID}, cred); err != nil {
		res.OK, res.Message = false, "已领取，确认失败，稍后重试: "+err.Error()
		return res
	}
	res.Status = "CONFIRMED"
	res.Message = "已补确认"
	if res.Credits > 0 {
		res.Message = fmt.Sprintf("领取成功（%g 积分）", res.Credits)
	}
	return res
}
