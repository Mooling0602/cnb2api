package server

import (
	"encoding/json"
	"strings"
	"testing"

	"cnb2api/internal/upstream"
)

const tinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestExtractChatImages(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"文本+图片", `[{"type":"text","text":"看这图"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]`, 1},
		{"多图", `[{"type":"image_url","image_url":{"url":"a"}},{"type":"image_url","image_url":{"url":"b"}}]`, 2},
		{"仅文本", `[{"type":"text","text":"你好"}]`, 0},
		{"纯字符串", `"你好"`, 0},
		{"空", ``, 0},
		{"null", `null`, 0},
		{"image_url 为 null", `[{"type":"image_url","image_url":null}]`, 0},
		{"其它类型忽略", `[{"type":"thinking","thinking":"x"},{"type":"toolCall","name":"n"}]`, 0},
	}
	for _, c := range cases {
		got := extractChatImages(json.RawMessage(c.in))
		if len(got) != c.want {
			t.Errorf("%s: got %d 张图, want %d", c.name, len(got), c.want)
		}
	}
}

// TestChatMessageMarshalMultimodal 锁定发往上游的精确结构。
// 上游实测接受 [{type:text},{type:image_url:{url}}] 数组。
func TestChatMessageMarshalMultimodal(t *testing.T) {
	m := upstream.ChatMessage{
		Role:    "user",
		Content: "这张图什么颜色?",
		Parts: []upstream.ContentPart{{
			Type:     "image_url",
			ImageURL: &upstream.ImageURL{URL: tinyPNG},
		}},
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("content 应为数组,解析失败: %v\n%s", err, body)
	}
	if len(got.Content) != 2 {
		t.Fatalf("期望 2 个块(文本+图片),实际 %d: %s", len(got.Content), body)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "这张图什么颜色?" {
		t.Errorf("首块应为文本: %s", body)
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil || got.Content[1].ImageURL.URL != tinyPNG {
		t.Errorf("次块应为图片且保持 data URL 原样: %s", body)
	}
	// Parts 本身不应作为独立字段泄漏到 wire 上
	if strings.Contains(string(body), `"Parts"`) {
		t.Errorf("Parts 不应序列化: %s", body)
	}
}

// TestChatMessageMarshalTextOnly 确认无图片时 content 仍是纯字符串
// (保持与既有上游契约一致,避免无谓地把所有请求变成数组)。
func TestChatMessageMarshalTextOnly(t *testing.T) {
	m := upstream.ChatMessage{Role: "user", Content: "你好"}
	body, _ := json.Marshal(m)
	var got struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var s string
	if err := json.Unmarshal(got.Content, &s); err != nil {
		t.Fatalf("无图片时 content 应为字符串,实际 %s", got.Content)
	}
	if s != "你好" {
		t.Errorf("content=%q, want 你好", s)
	}
}

// TestChatMessageMarshalImageOnly 纯图片消息(content 为空)必须只输出图片块,
// 不能塞一个空文本块进去。
func TestChatMessageMarshalImageOnly(t *testing.T) {
	m := upstream.ChatMessage{
		Role:  "user",
		Parts: []upstream.ContentPart{{Type: "image_url", ImageURL: &upstream.ImageURL{URL: tinyPNG}}},
	}
	body, _ := json.Marshal(m)
	var got struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("content 应为数组: %v\n%s", err, body)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "image_url" {
		t.Fatalf("纯图消息应只含 1 个 image_url 块: %s", body)
	}
}

// TestChatMessageMarshalToolCallsPreserved 确认自定义 MarshalJSON 没有丢字段。
func TestChatMessageMarshalToolCallsPreserved(t *testing.T) {
	m := upstream.ChatMessage{
		Role:       "assistant",
		Content:    "",
		ToolCalls:  []upstream.ToolCall{{ID: "call_1", Type: "function", Function: upstream.ToolCallFunction{Name: "cnb_read", Arguments: `{"a":1}`}}},
		ToolCallID: "call_1",
	}
	body, _ := json.Marshal(m)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"role", "content", "tool_calls", "tool_call_id"} {
		if _, ok := got[k]; !ok {
			t.Errorf("字段 %q 丢失: %s", k, body)
		}
	}
}

func TestExtractResponsesImages(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"input_image 裸字符串 url", `[{"type":"input_text","text":"看"},{"type":"input_image","image_url":"` + tinyPNG + `"}]`, 1},
		{"多图", `[{"type":"input_image","image_url":"a"},{"type":"input_image","image_url":"b"}]`, 2},
		{"只有文本", `[{"type":"input_text","text":"你好"}]`, 0},
		{"空 url 忽略", `[{"type":"input_image","image_url":""}]`, 0},
		{"纯字符串", `"你好"`, 0},
		{"空", ``, 0},
	}
	for _, c := range cases {
		got := extractResponsesImages(json.RawMessage(c.in))
		if len(got) != c.want {
			t.Errorf("%s: got %d, want %d", c.name, len(got), c.want)
		}
	}
}

func TestExtractAnthropicImages(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantURL string
	}{
		{
			"base64 source 转 data URL",
			`[{"type":"text","text":"看"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]`,
			1, "data:image/jpeg;base64,AAAA",
		},
		{
			"缺 media_type 默认 png",
			`[{"type":"image","source":{"type":"base64","data":"BBBB"}}]`,
			1, "data:image/png;base64,BBBB",
		},
		{
			"url source 原样透传",
			`[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]`,
			1, "https://x/y.png",
		},
		{"只有文本", `[{"type":"text","text":"你好"}]`, 0, ""},
		{"无 source 忽略", `[{"type":"image"}]`, 0, ""},
	}
	for _, c := range cases {
		got := extractAnthropicImages(json.RawMessage(c.in))
		if len(got) != c.want {
			t.Errorf("%s: got %d 张, want %d", c.name, len(got), c.want)
			continue
		}
		if c.want > 0 && (got[0].ImageURL == nil || got[0].ImageURL.URL != c.wantURL) {
			t.Errorf("%s: url=%v, want %q", c.name, got[0].ImageURL, c.wantURL)
		}
	}
}

// TestAnthropicTextAndImageCoexist 确认 Anthropic 的同一 blocks 数组里
// 文本与图片能分别提取,互不干扰。
func TestAnthropicTextAndImageCoexist(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"描述这张图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZZZ"}}]`)
	text := extractAnthropicText(raw)
	imgs := extractAnthropicImages(raw)
	if text != "描述这张图" {
		t.Errorf("文本提取错误: %q", text)
	}
	if len(imgs) != 1 {
		t.Fatalf("应提取 1 张图,实际 %d", len(imgs))
	}
}
