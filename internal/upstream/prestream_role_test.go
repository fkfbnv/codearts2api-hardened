package upstream

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 角色帧之后才到的业务错误（参数非法/内容审查）仍属于「尚未输出任何正文」：
// preflightSSE 必须把它当作预流错误返回，而不是让角色帧先写出去、最后变成
// 200 + SSE error。
func TestPreflightSSERoleFrameThenBusinessError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "param invalid 81001",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81001.400\",\"error_msg\":\"该模型始终思考，不支持关闭思考；请使用 low、high 或 max。\"}\n\n",
			want: "81001",
		},
		{
			name: "content moderation 81011",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81011.403\",\"error_msg\":\"抱歉，我无法提供关于您提问内容的建议。\"}\n\n",
			want: "81011",
		},
		{
			name: "model access denied 000010051",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: done\ndata: {\"error_code\":\"TM.000010051\",\"error_msg\":\"您当前无权调用该模型，请联系企业管理员开通。\"}\n\n",
			want: "000010051",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := preflightSSE(io.NopCloser(strings.NewReader(tc.body)))
			if rc != nil {
				rc.Close()
			}
			if err == nil {
				t.Fatalf("role-frame followed by %s business error was not surfaced as pre-stream error", tc.name)
			}
			var ae *ApiError
			if !errors.As(err, &ae) {
				t.Fatalf("want *ApiError, got %T (%v)", err, err)
			}
			if !strings.Contains(ae.Message, tc.want) {
				t.Fatalf("error message lost upstream reason: %q", ae.Message)
			}
		})
	}
}

// 角色帧 + 错误：sse 层也不得把角色帧先写出去（那样 httptest/真实连接都已
// 发出响应头，handler 无法再回正确的 HTTP 状态码）。
func TestStreamRoleFrameThenBusinessErrorIsPreStream(t *testing.T) {
	stream := strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81011.403\",\"error_msg\":\"抱歉，我无法提供关于您提问内容的建议。\"}\n\n")
	rec := httptest.NewRecorder()
	err := Stream(rec, stream, "GLM-5.2")
	var pre *PreStreamError
	if !errors.As(err, &pre) {
		t.Fatalf("role-frame error should be PreStreamError, got %T (%v)", err, err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("pre-stream error must not write SSE body, got: %s", body)
	}
}

// 已经输出正文后再收到业务错误：必须保持 SSE error + [DONE]，不能被误判成
// PreStreamError（响应已经开始，改不了状态码）。
func TestStreamWritesErrorEventAfterContentForBusinessError(t *testing.T) {
	stream := strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"event: done\ndata: {\"error_code\":\"InferHub.ModelArts.81001.400\",\"error_msg\":\"该模型始终思考，不支持关闭思考；请使用 low、high 或 max。\"}\n\n")
	rec := httptest.NewRecorder()
	err := Stream(rec, stream, "GLM-5.2")
	if err == nil {
		t.Fatal("embedded business error was reported as success")
	}
	var pre *PreStreamError
	if errors.As(err, &pre) {
		t.Fatalf("error after content must not be PreStreamError: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("client did not receive terminal error stream: %s", body)
	}
	if !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("content deltas must still be delivered: %s", body)
	}
}
