package toolconv

import (
	"encoding/json"
	"testing"
)

func TestRenamerForwardRestore(t *testing.T) {
	r := NewRenamer()
	// 无前缀 → 加前缀并记录
	if got := r.Forward("get_weather"); got != "cnb_get_weather" {
		t.Fatalf("Forward(get_weather) = %q, want cnb_get_weather", got)
	}
	// 已有前缀 → 原样
	if got := r.Forward("cnb_weather"); got != "cnb_weather" {
		t.Fatalf("Forward(cnb_weather) = %q, want cnb_weather", got)
	}
	// 空名 → 原样
	if got := r.Forward(""); got != "" {
		t.Fatalf("Forward(\"\") = %q, want empty", got)
	}
	// 还原
	if got := r.Restore("cnb_get_weather"); got != "get_weather" {
		t.Fatalf("Restore(cnb_get_weather) = %q, want get_weather", got)
	}
	// 未命中 → 原样
	if got := r.Restore("cnb_unknown"); got != "cnb_unknown" {
		t.Fatalf("Restore(cnb_unknown) = %q, want cnb_unknown", got)
	}
}

func TestFromOpenAIChat(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}`),
		json.RawMessage(`{"type":"function","function":{"name":"cnb_x","description":"x"}}`),
		json.RawMessage(`{"type":"web_search"}`), // 非 function 应被忽略
	}
	defs := FromOpenAIChat(tools)
	if len(defs) != 2 {
		t.Fatalf("len(defs) = %d, want 2", len(defs))
	}
	if defs[0].Name != "get_weather" || defs[1].Name != "cnb_x" {
		t.Fatalf("defs names = %q, %q", defs[0].Name, defs[1].Name)
	}
}

func TestFromAnthropic(t *testing.T) {
	tools := []AnthropicTool{
		{Name: "get_weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	defs := FromAnthropic(tools)
	if len(defs) != 1 || defs[0].Name != "get_weather" {
		t.Fatalf("defs = %+v", defs)
	}
	if defs[0].Parameters == nil {
		t.Fatalf("parameters should be parsed")
	}
}

func TestFromResponses(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}}]`)
	defs := FromResponses(raw)
	if len(defs) != 1 || defs[0].Name != "get_weather" {
		t.Fatalf("defs = %+v", defs)
	}
}

func TestAnthropicCallsAndResults(t *testing.T) {
	// assistant tool_use
	assistantBlocks := []AnthropicBlock{
		{Type: "text", Text: "我来查一下"},
		{Type: "tool_use", ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"city":"北京"}`)},
	}
	calls, text := AnthropicCalls(assistantBlocks)
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
	if text != "我来查一下" {
		t.Fatalf("text = %q", text)
	}

	// user tool_result
	userBlocks := []AnthropicBlock{
		{Type: "tool_result", ToolUseID: "call_1", Content: json.RawMessage(`"北京晴"`)},
	}
	results, text2 := AnthropicResults(userBlocks)
	if len(results) != 1 || results[0].CallID != "call_1" || results[0].Content != "北京晴" {
		t.Fatalf("results = %+v", results)
	}
	if text2 != "" {
		t.Fatalf("text2 = %q", text2)
	}
}

func TestResponsesCallAndResult(t *testing.T) {
	call := ResponsesCall(ResponsesInputItem{Type: "function_call", CallID: "c1", Name: "get_weather", Arguments: `{"city":"北京"}`})
	if call.ID != "c1" || call.Name != "get_weather" || call.Arguments != `{"city":"北京"}` {
		t.Fatalf("call = %+v", call)
	}
	res := ResponsesResult(ResponsesInputItem{Type: "function_call_output", CallID: "c1", Output: json.RawMessage(`"北京晴"`)})
	if res.CallID != "c1" || res.Content != "北京晴" {
		t.Fatalf("res = %+v", res)
	}
}
