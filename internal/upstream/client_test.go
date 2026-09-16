package upstream

import (
	"encoding/json"
	"testing"
)

func TestSanitizeUpstreamBody(t *testing.T) {
	// 构造含 🇹🇼 的请求体
	req := &ChatRequest{
		Model:  "deepseek-v4.1-flash",
		Stream: true,
		Messages: []ChatMessage{
			{Role: "user", Content: "🇹🇼 的天气"},
			{Role: "tool", Content: "结果含 🇹🇼🇹🇼"},
		},
	}
	body, _ := json.Marshal(req)

	got := sanitizeUpstreamBody(body)

	// 不应再含 🇹🇼
	if containsTWFlag(got) {
		t.Fatalf("sanitize 后仍含 🇹🇼: %s", got)
	}
	// 应含替换后的"tw"
	if !containsStr(got, "tw") {
		t.Fatalf("sanitize 后应含\"tw\": %s", got)
	}
	// 其它内容不受影响
	if !containsStr(got, "的天气") || !containsStr(got, "结果含") {
		t.Fatalf("sanitize 误伤其它内容: %s", got)
	}
}

func TestSanitizeUpstreamBodyNoop(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"普通内容 台湾 文字"}]}`)
	got := sanitizeUpstreamBody(body)
	if string(got) != string(body) {
		t.Fatalf("无 🇹🇼 时不应改动: %s", got)
	}
}

func containsTWFlag(b []byte) bool {
	return containsStr(b, "\U0001F1F9\U0001F1FC")
}

func containsStr(b []byte, s string) bool {
	return len(b) >= len(s) && indexOf(b, []byte(s)) >= 0
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
