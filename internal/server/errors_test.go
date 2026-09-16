package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cnb2api/internal/upstream"
)

// TestUpstreamErrorStatusBodyTooLarge 确认体积超限被映射为 413 而非 502,
// 且消息里带上实际体积、上限与可操作建议。
func TestUpstreamErrorStatusBodyTooLarge(t *testing.T) {
	err := &upstream.ErrBodyTooLarge{Size: 1090906, Limit: upstream.MaxBodyBytes, Raw: `{"errcode":413}`}
	code, payload := upstreamErrorStatus(err)

	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("体积超限应返回 413,实际 %d", code)
	}
	body, _ := json.Marshal(payload)
	s := string(body)

	// 必须给出的关键信息
	for _, want := range []string{
		"1090906", // 实际字节数
		"1048576", // 上限字节数
		"request_too_large",
		"body_too_large",
		"base64",  // 说明放大原因
		"history", // 说明历史会重发
	} {
		if !strings.Contains(s, want) {
			t.Errorf("错误响应应包含 %q: %s", want, s)
		}
	}

	// 结构化字段便于程序化处理
	if payload["body_bytes"] != 1090906 {
		t.Errorf("body_bytes 字段错误: %v", payload["body_bytes"])
	}
	if payload["limit_bytes"] != upstream.MaxBodyBytes {
		t.Errorf("limit_bytes 字段错误: %v", payload["limit_bytes"])
	}
}

// TestUpstreamErrorStatusGeneric 确认其它错误仍是 502 upstream_error,
// 即本次改动没有把普通上游故障也说成 413。
func TestUpstreamErrorStatusGeneric(t *testing.T) {
	cases := []error{
		fmt.Errorf("cnb: upstream status 500: boom"),
		fmt.Errorf("cnb: chat failed after retries: %w", errors.New("dial tcp: timeout")),
	}
	for _, err := range cases {
		code, payload := upstreamErrorStatus(err)
		if code != http.StatusBadGateway {
			t.Errorf("%v 应返回 502,实际 %d", err, code)
		}
		m, ok := payload["error"].(map[string]any)
		if !ok {
			t.Fatalf("payload 结构异常: %v", payload)
		}
		if m["type"] != "upstream_error" {
			t.Errorf("type 应为 upstream_error,实际 %v", m["type"])
		}
	}
}

// TestUpstreamErrorStatusWrapped 确认 errors.As 能穿透 fmt.Errorf 的 %w 包装。
// Chat() 在重试耗尽后会用 %w 包一层,若不穿透就会误报成 502。
func TestUpstreamErrorStatusWrapped(t *testing.T) {
	inner := &upstream.ErrBodyTooLarge{Size: 2 << 20, Limit: upstream.MaxBodyBytes}
	wrapped := fmt.Errorf("cnb: chat failed after retries: %w", inner)
	code, _ := upstreamErrorStatus(wrapped)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("包装后的体积超限仍应识别为 413,实际 %d", code)
	}
}

// TestErrBodyTooLargeMessage 锁定错误文本包含可诊断信息。
func TestErrBodyTooLargeMessage(t *testing.T) {
	err := &upstream.ErrBodyTooLarge{Size: 1234567, Limit: upstream.MaxBodyBytes}
	msg := err.Error()
	for _, want := range []string{"1234567", "1048576", "1 MiB", "before it reached the model"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文本应包含 %q: %s", want, msg)
		}
	}
}

// TestWriteUpstreamErrorWrites413 端到端确认 HTTP 状态码真的写出去了。
func TestWriteUpstreamErrorWrites413(t *testing.T) {
	w := httptest.NewRecorder()
	writeUpstreamError(w, &upstream.ErrBodyTooLarge{Size: 3 << 20, Limit: upstream.MaxBodyBytes})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码应为 413,实际 %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("响应应含 error 字段: %s", w.Body.String())
	}
}

// TestMaxBodyBytesMatchesUpstream 记录实测边界,防止有人随手改掉这个常量。
// 实测:1048112 通过 / 1048688 拒绝 -> 1 MiB。
func TestMaxBodyBytesMatchesUpstream(t *testing.T) {
	if upstream.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes 应为 1<<20 (实测边界),实际 %d", upstream.MaxBodyBytes)
	}
	// 实测通过/拒绝的两个点必须分别落在限值两侧
	const pass = 1048112
	const fail = 1048688
	if pass > upstream.MaxBodyBytes {
		t.Errorf("实测通过的 %d 字节不应超过限值 %d", pass, upstream.MaxBodyBytes)
	}
	if fail <= upstream.MaxBodyBytes {
		t.Errorf("实测被拒的 %d 字节应超过限值 %d", fail, upstream.MaxBodyBytes)
	}
}
