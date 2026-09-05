package toolconv

import (
	"encoding/json"
	"strings"
)

// AnthropicBlock 是 Anthropic content 数组里的一个 block。
type AnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// ParseAnthropicContent 解析 Anthropic content(可能是 string 或 blocks 数组)。
// 返回 blocks 与是否成功解析为数组;string 内容返回空 blocks + false。
func ParseAnthropicContent(content json.RawMessage) ([]AnthropicBlock, bool) {
	if len(content) == 0 || string(content) == "null" {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return nil, false // 纯字符串,无 blocks
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// AnthropicCalls 从 assistant 消息的 blocks 提取工具调用(tool_use)。
// 同时返回纯文本部分(非 tool_use block 的 text 拼接)。
func AnthropicCalls(blocks []AnthropicBlock) (calls []Call, text string) {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			args := ""
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			calls = append(calls, Call{ID: b.ID, Name: b.Name, Arguments: args})
		case "text", "input_text":
			sb.WriteString(b.Text)
		}
	}
	return calls, sb.String()
}

// AnthropicResults 从 user 消息的 blocks 提取工具结果(tool_result)。
// 同时返回纯文本部分。
func AnthropicResults(blocks []AnthropicBlock) (results []Result, text string) {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			content := ""
			if len(b.Content) > 0 && string(b.Content) != "null" {
				var s string
				if err := json.Unmarshal(b.Content, &s); err == nil {
					content = s
				} else {
					content = string(b.Content)
				}
			}
			results = append(results, Result{CallID: b.ToolUseID, Content: content})
		case "text", "input_text":
			sb.WriteString(b.Text)
		}
	}
	return results, sb.String()
}

// ResponsesInputItem 是 Responses input 数组里的一个元素。
type ResponsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// ParseResponsesInput 解析 Responses input(可能是 string 或 items 数组)。
func ParseResponsesInput(input json.RawMessage) ([]ResponsesInputItem, bool) {
	if len(input) == 0 || string(input) == "null" {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		return nil, false // 纯字符串
	}
	var items []ResponsesInputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, false
	}
	return items, true
}

// ResponsesCall 从 function_call item 提取工具调用。
func ResponsesCall(item ResponsesInputItem) Call {
	id := item.CallID
	if id == "" {
		id = item.ID
	}
	return Call{ID: id, Name: item.Name, Arguments: item.Arguments}
}

// ResponsesResult 从 function_call_output item 提取工具结果。
func ResponsesResult(item ResponsesInputItem) Result {
	content := ""
	if len(item.Output) > 0 && string(item.Output) != "null" {
		var s string
		if err := json.Unmarshal(item.Output, &s); err == nil {
			content = s
		} else {
			content = string(item.Output)
		}
	}
	return Result{CallID: item.CallID, Content: content}
}
