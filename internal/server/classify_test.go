package server

import "testing"

// containsCode 必须按完整错误码匹配。第三方项目有 `"1005" in text` 把
// InferHub.001001005（参数非法）误判成权益耗尽的真实事故。
func TestContainsCodeDoesNotMatchSubstring(t *testing.T) {
	const msg = "codearts error code=InferHub.001001005.400 msg=The request param is invalid"
	if containsCode(msg, "1005") {
		t.Fatal("bare 1005 must not match inside 001001005")
	}
	if !containsCode(msg, "001001005") {
		t.Fatal("full code 001001005 should match")
	}
	if !containsCode("codearts error code=TM.000010051 msg=无权调用该模型", "000010051") {
		t.Fatal("000010051 should match after a dot boundary")
	}
}

func TestClassifyUpstreamMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want upstreamErrorKind
	}{
		{"codearts error code=InferHub.001001005.400 msg=The request param is invalid", errKindParamInvalid},
		{"codearts error code=InferHub.001001020.406 msg=model not multimodal", errKindParamInvalid},
		// 2026-11-15 22:23 实测：关闭思考参数打到「始终思考」模型，上游回 502
		// ModelArts.81001.400，曾落进 5xx 兜底把整池冷 10 分钟。
		{"codearts api code=502 http=502 path=/api/v2/chat/completions msg=InferHub.ModelArts.81001.400 该模型始终思考，不支持关闭思考；请使用 low、high 或 max。", errKindParamInvalid},
		{"codearts error code=TM.000010051 msg=无权调用该模型", errKindModelUnavailable},
		{"codearts error code=InferHub.APIG.0101.404 msg=The API does not exist or has not been published in the environment", errKindModelUnavailable},
		{"codearts error code=InferHub.002002009.404 msg=model is not registered", errKindModelUnavailable},
		{"codearts error code=InferHub.4004.200 msg=benefit not found", errKindModelUnavailable},
		{"codearts error code=InferHub.ModelArts.81011.403 msg=抱歉，我无法提供关于您提问内容的建议", errKindContentModeration},
		{"codearts error code=TM.00001041 msg=并发会话数已达上限", errKindConcurrentLimit},
		{"codearts error code=InferHub.ModelArts.81111.429 msg=TPM limit reached", errKindRateLimit},
		{"codearts error code=APIG.0301 msg=Incorrect IAM authentication information: decrypt token fail", errKindAuth},
		{"codearts error code=APIG.0602 msg=token expired", errKindAuth},
		{"codearts error code=STS5.1806 msg=invalid refresh token: InvalidDPoPHeader", errKindAuthExpired},
		// 1116 实测：两个大请求卡 61–63 秒，上游回 504 APIG.0203 Backend timeout，
		// 曾落进 5xx 兜底把单账号池冷 10 分钟。
		{`codearts api code=504 http=504 path=/api/v2/chat/completions msg={"error_msg":"Backend timeout","error_code":"APIG.0203","request_id":"b10677c3"}`, errKindGatewayTimeout},
		{"codearts error code=InferHub.APIG.0203.504 msg=Backend timeout", errKindGatewayTimeout},
		{"dial tcp 10.0.0.1:443: connect: connection refused", errKindUnknown},
	}
	for _, tc := range cases {
		got, reason := classifyUpstreamMessage(tc.msg)
		if got != tc.want {
			t.Errorf("classify(%q) = %v, want %v", tc.msg, got, tc.want)
		}
		if got != errKindUnknown && reason == "" {
			t.Errorf("classify(%q) returned kind %v without a reason", tc.msg, got)
		}
	}
}

// 模型级错误必须继续被判定为「不可用」——这是 1109 补丁的核心不变量。
func TestModelUnavailableReasonsStayDetected(t *testing.T) {
	for _, msg := range []string{
		"codearts error code=TM.000010051 msg=无权调用该模型",
		"codearts error code=InferHub.APIG.0101.404 msg=not published",
		"codearts error code=InferHub.002002009.404 msg=model is not registered",
	} {
		if reason := upstreamUnavailableReasonText(msg); reason == "" {
			t.Errorf("expected model-unavailable reason for %q", msg)
		}
	}
	// 参数非法不是模型不可用：不能被 markUnusable，否则 /v1/models 会误下架。
	if reason := upstreamUnavailableReasonText("codearts error code=InferHub.001001005.400 msg=param is invalid"); reason != "" {
		t.Errorf("param-invalid must not be treated as model unavailable, got %q", reason)
	}
}
