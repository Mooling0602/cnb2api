package server

import (
	"encoding/json"
	"strings"
	"testing"

	"cnb2api/internal/upstream"
)

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// off 及其大小写变体 → 上游空串
		{"off", ""},
		{"OFF", ""},
		{"Off", ""},
		{"oFf", ""},
		// 上游白名单内的取值原样透传
		{"", ""},
		{"none", "none"},
		{"minimal", "minimal"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		// 未知值原样透传,交由上游报错(网关不硬编码档位列表)
		{"banana", "banana"},
		{"offish", "offish"},
		{" off", " off"},
	}
	for _, c := range cases {
		if got := normalizeReasoningEffort(c.in); got != c.want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReasoningEffortPtrMarshalsEmptyString 锁定一个关键细节:
// 指向空串的非 nil 指针必须仍然序列化出 "reasoning_effort":"",
// 因为空串正是上游「不思考」的唯一表达。若哪天把字段改成
// omitempty 依赖值本身,这里会失败。
func TestReasoningEffortPtrMarshalsEmptyString(t *testing.T) {
	req := &upstream.ChatRequest{
		Model:           "deepseek-v4.1-flash",
		Stream:          true,
		ReasoningEffort: reasoningEffortPtr("off"),
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := got["reasoning_effort"]
	if !ok {
		t.Fatalf("off 必须序列化出 reasoning_effort 字段(空串),实际缺省: %s", body)
	}
	if v != "" {
		t.Fatalf("off 应归一化为空串,实际 %v: %s", v, body)
	}
}

// TestReasoningEffortPtrNilOmits 确认 nil 指针完全不发该字段。
func TestReasoningEffortPtrNilOmits(t *testing.T) {
	req := &upstream.ChatRequest{Model: "m", Stream: true}
	body, _ := json.Marshal(req)
	if strings.Contains(string(body), "reasoning_effort") {
		t.Fatalf("nil 指针不应发出 reasoning_effort: %s", body)
	}
}

// TestReasoningDefaultIsWhitelisted 保证默认档位在上游白名单内,
// 否则「客户端不传就注入默认值」这条路径会把请求打成 400。
func TestReasoningDefaultIsWhitelisted(t *testing.T) {
	whitelist := map[string]bool{
		"": true, "none": true, "minimal": true, "low": true,
		"medium": true, "high": true, "xhigh": true, "max": true,
	}
	if !whitelist[reasoningDefault] {
		t.Fatalf("reasoningDefault=%q 不在上游白名单内", reasoningDefault)
	}
	if normalizeReasoningEffort("off") != reasoningOff {
		t.Fatal("off 必须归一化为上游空串")
	}
}

func TestExtractThinkingEffortMapsBudget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"小预算 → low", `{"type":"enabled","budget_tokens":1000}`, "low"},
		{"中预算 → medium", `{"type":"enabled","budget_tokens":5000}`, "medium"},
		{"大预算 → high", `{"type":"enabled","budget_tokens":20000}`, "high"},
	}
	for _, c := range cases {
		got := extractThinkingEffort(json.RawMessage(c.in))
		if got == nil {
			t.Errorf("%s: 期望非 nil", c.name)
			continue
		}
		if *got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, *got, c.want)
		}
	}
	// type 非 enabled(如 disabled)→ nil,不走 reasoning 通道
	if got := extractThinkingEffort(json.RawMessage(`{"type":"disabled"}`)); got != nil {
		t.Errorf("disabled 应返回 nil,实际 %q", *got)
	}
}

func TestExtractResponsesReasoning(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`true`, "high"},
		{`false`, ""},
		{`{"effort":"off"}`, "off"},
		{`{"effort":"high"}`, "high"},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := extractResponsesReasoning(json.RawMessage(c.in)); got != c.want {
			t.Errorf("extractResponsesReasoning(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
