// Package toolconv 实现跨协议统一的工具处理层(借鉴 new-api relaykit/toolconv 的设计,精简为 function 工具)。
//
// 设计思想:
//   - 三种协议(OpenAI Chat / Anthropic Messages / OpenAI Responses)的工具定义与工具历史
//     格式各不相同,但最终都要转成上游 CNB 的 OpenAI Chat 格式。
//   - 本包提供统一模型(Definition / Call / Result),把各协议的工具"提取"成统一模型,
//     再"编码"成上游格式,避免三个 handler 各自为政。
//
// CNB 上游约束(实测):
//   - 工具名必须以 cnb_ 前缀,否则 403;网关自动加前缀,响应时还原,客户端无感。
//   - tool_choice 字段本身触发 403,统一丢弃(上游自动选择工具,行为等价 auto)。
//   - 工具历史需 tool_call_id 配对(assistant.tool_calls ↔ tool.tool_call_id)。
package toolconv

import (
	"encoding/json"
	"strings"

	"cnb2api/internal/upstream"
)

// Definition 统一工具定义(仅 function 类型,CNB 上游只支持 function 工具)。
type Definition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Call 统一工具调用历史(assistant 消息里的 tool_calls)。
type Call struct {
	ID        string
	Name      string
	Arguments string // JSON 字符串
}

// Result 统一工具结果历史(tool 消息)。
type Result struct {
	CallID  string
	Content string
}

// Renamer 管理工具名映射(上游名 -> 客户端原名),用于转发前加前缀、响应时还原。
type Renamer struct {
	m map[string]string
}

// NewRenamer 创建空映射。
func NewRenamer() *Renamer { return &Renamer{m: map[string]string{}} }

// Forward 转发前改名:无 cnb_ 前缀则加前缀并记录映射,便于响应时还原。
func (r *Renamer) Forward(name string) string {
	if name == "" || strings.HasPrefix(name, "cnb_") {
		return name
	}
	up := "cnb_" + name
	r.m[up] = name
	return up
}

// Restore 响应时还原原名;未命中映射则原样返回。
func (r *Renamer) Restore(name string) string {
	if orig, ok := r.m[name]; ok {
		return orig
	}
	return name
}

// ToUpstream 把统一定义编码成上游工具(工具名加前缀)。
func ToUpstream(defs []Definition, r *Renamer) []upstream.Tool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]upstream.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, upstream.Tool{
			Type: "function",
			Function: upstream.ToolFunction{
				Name:        r.Forward(d.Name),
				Description: d.Description,
				Parameters:  d.Parameters,
			},
		})
	}
	return out
}

// FromOpenAIChat 从 OpenAI Chat 的 tools 提取统一定义。
// tools 是 []json.RawMessage,每个元素形如 {"type":"function","function":{...}}。
func FromOpenAIChat(tools []json.RawMessage) []Definition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Definition, 0, len(tools))
	for _, raw := range tools {
		var t struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if t.Type != "" && t.Type != "function" {
			continue // 上游只支持 function 工具
		}
		out = append(out, Definition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

// FromAnthropic 从 Anthropic 的 tools 提取统一定义。
// Anthropic 工具形如 {"name":..., "description":..., "input_schema":{...}}。
func FromAnthropic(tools []AnthropicTool) []Definition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]Definition, 0, len(tools))
	for _, t := range tools {
		var params map[string]any
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			_ = json.Unmarshal(t.InputSchema, &params)
		}
		out = append(out, Definition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return out
}

// FromResponses 从 OpenAI Responses 的 tools 提取统一定义。
// Responses 工具形如 {"type":"function","name":...,"description":...,"parameters":{...}}。
func FromResponses(tools json.RawMessage) []Definition {
	if len(tools) == 0 || string(tools) == "null" {
		return nil
	}
	var raw []struct {
		Type        string         `json:"type"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(tools, &raw); err != nil {
		return nil
	}
	out := make([]Definition, 0, len(raw))
	for _, t := range raw {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out = append(out, Definition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return out
}

// AnthropicTool 是 Anthropic 工具定义(供 FromAnthropic 使用)。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
