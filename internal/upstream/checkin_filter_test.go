package upstream

import "testing"

// 签到过滤按活动类型（USER_LOGIN / NEW_USER_REGISTER）定位，不再依赖会轮换的
// campaignId。样本取自 2026-11-11 对上游 delivery 的真实返回值。
func TestIsClaimableCreditCampaign(t *testing.T) {
	cases := []struct {
		name string
		camp CreditCampaign
		want bool
	}{
		{"每日签到（ID 已从 1 换成 34）", CreditCampaign{ID: 34, Title: "每日签到领150积分", Type: "USER_LOGIN"}, true},
		{"Skill创新赛每日签到礼", CreditCampaign{ID: 40, Title: "Skill创新赛每日签到礼", Type: "USER_LOGIN"}, true},
		{"Skill创新赛报名礼（标题不含签到，旧判据会漏）", CreditCampaign{ID: 42, Title: "Skill创新赛报名礼", Type: "USER_LOGIN"}, true},
		{"新用户注册礼", CreditCampaign{ID: 4, Title: "新用户注册送 4000 积分", Type: "NEW_USER_REGISTER"}, true},
		{"推荐码（不是签到）", CreditCampaign{ID: 3, Title: "推荐码上有礼", Type: "INVITE_USER"}, false},
		{"学生认证（不是签到）", CreditCampaign{ID: 2, Title: "学生认证领积分", Type: "STUDENT_CERTIFIED"}, false},
		{"老接口无 type 时按标题兜底", CreditCampaign{ID: 99, Title: "每日签到领100积分"}, true},
		{"老接口无 type 时按 ID 4 兜底", CreditCampaign{ID: 4, Title: "新用户注册送积分"}, true},
	}
	for _, tc := range cases {
		if got := isClaimableCreditCampaign(tc.camp); got != tc.want {
			t.Errorf("%s: isClaimableCreditCampaign = %v, want %v", tc.name, got, tc.want)
		}
	}
}
