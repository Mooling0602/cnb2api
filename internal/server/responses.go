// OpenAI Responses API 适配层（/v1/responses）。
//
// 接收 input/instructions 格式，转为 messages 后复用现有管线。
// 输出 Response 对象 + 流式事件（response.created/response.output_text.delta/...）。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cnb2api/internal/toolconv"
	"cnb2api/internal/upstream"
)

// responsesRequest 是 OpenAI Responses 格式请求。
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string 或 [items]
	Instructions    string          `json:"instructions"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Stream          bool            `json:"stream"`
	Reasoning       json.RawMessage `json:"reasoning"` // 可选: bool 或 {effort:"low"|"medium"|"high"}
	MaxOutputTokens int             `json:"max_output_tokens"`
}

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}

	// 转换为内部格式
	upReq := &upstream.ChatRequest{
		Model:     s.model,
		Stream:    true, // 内部统一用流式
		Messages:  []upstream.ChatMessage{},
		MaxTokens: req.MaxOutputTokens,
	}

	// reasoning → 推理等级/思考模式
	// 默认注入 reasoning_effort=low 让模型思考(保证回答质量),但思考链内容默认不回传:
	// 只有客户端显式传 reasoning 参数时才回传思考链(OpenAI Responses 标准行为)。
	exposeReasoning := len(req.Reasoning) > 0
	if len(req.Reasoning) > 0 {
		reasoningEffort := extractResponsesReasoning(req.Reasoning)
		if reasoningEffort != "" {
			upReq.ReasoningEffort = &reasoningEffort
		}
		enableThinking := true
		upReq.EnableThinking = &enableThinking
	} else {
		effort := "low"
		upReq.ReasoningEffort = &effort
	}

	// 工具定义:统一提取 + 加 cnb_ 前缀(上游白名单要求),响应时还原。
	renamer := toolconv.NewRenamer()
	upReq.Tools = toolconv.ToUpstream(toolconv.FromResponses(req.Tools), renamer)
	// tool_choice:与 Chat 一致,上游 403,统一丢弃。
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		log.Printf("[REQ] responses tool_choice dropped (upstream rejects it): %s", string(req.ToolChoice))
	}

	// instructions → system message
	if req.Instructions != "" {
		upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "system", Content: req.Instructions})
	}

	// input → messages(支持工具历史归一化:function_call → tool_calls,function_call_output → tool)
	upReq.Messages = append(upReq.Messages, responsesInputToMessages(req.Input, renamer)...)

	ctx := r.Context()
	resp, err := s.upstream.Chat(ctx, upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		s.responsesStreamResponse(w, resp, renamer, exposeReasoning)
	} else {
		s.responsesNonStreamResponse(w, resp, renamer, exposeReasoning)
	}
}

// responsesInputToMessages 把 Responses input 转成上游消息,支持工具历史归一化。
func responsesInputToMessages(input json.RawMessage, renamer *toolconv.Renamer) []upstream.ChatMessage {
	if len(input) == 0 || string(input) == "null" {
		return nil
	}
	// 纯字符串 → 单条 user
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		if s == "" {
			return nil
		}
		return []upstream.ChatMessage{{Role: "user", Content: s}}
	}
	// items 数组
	items, isItems := toolconv.ParseResponsesInput(input)
	if !isItems {
		// 兜底:旧格式 messages 数组
		return legacyInputToMessages(input)
	}
	var out []upstream.ChatMessage
	for _, item := range items {
		switch item.Type {
		case "function_call", "custom_tool_call":
			c := toolconv.ResponsesCall(item)
			out = append(out, upstream.ChatMessage{
				Role: "assistant",
				ToolCalls: []upstream.ToolCall{{
					ID:   c.ID,
					Type: "function",
					Function: upstream.ToolCallFunction{
						Name:      renamer.Forward(c.Name),
						Arguments: c.Arguments,
					},
				}},
			})
		case "function_call_output", "custom_tool_call_output":
			res := toolconv.ResponsesResult(item)
			out = append(out, upstream.ChatMessage{Role: "tool", Content: res.Content, ToolCallID: res.CallID})
		default:
			// 普通 message item
			role := item.Role
			if role == "" {
				role = "user"
			}
			content := responsesItemContent(item.Content)
			out = append(out, upstream.ChatMessage{Role: role, Content: content})
		}
	}
	return out
}

// responsesItemContent 提取 item 的 content(可能是 string 或 parts 数组)。
func responsesItemContent(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	// parts 数组:拼接 text 部分
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" || p.Type == "output_text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(content)
}

// legacyInputToMessages 兼容旧格式 input(messages 数组)。
func legacyInputToMessages(input json.RawMessage) []upstream.ChatMessage {
	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &msgs); err != nil {
		return nil
	}
	out := make([]upstream.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, upstream.ChatMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

// responsesNonStreamResponse 非流式响应：返回 Response 对象。
func (s *Server) responsesNonStreamResponse(w http.ResponseWriter, resp *http.Response, renamer *toolconv.Renamer, exposeReasoning bool) {
	var content strings.Builder
	var reasoning strings.Builder
	// 工具调用聚合:按 index 分组,首个 chunk 提供 id/name,后续 arguments 片段按序拼接
	type tcAgg struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := map[int]*tcAgg{}
	var toolCallOrder []int

	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			return nil
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil
		}
		for _, c := range obj.Choices {
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.Reasoning)
			for _, tc := range c.Delta.ToolCalls {
				agg, ok := toolCalls[tc.Index]
				if !ok {
					agg = &tcAgg{}
					toolCalls[tc.Index] = agg
					toolCallOrder = append(toolCallOrder, tc.Index)
				}
				if tc.ID != "" {
					agg.id = tc.ID
				}
				if tc.Function.Name != "" {
					agg.name = tc.Function.Name
				}
				agg.arguments.WriteString(tc.Function.Arguments)
			}
		}
		return nil
	})

	text := content.String()
	// 注意:content 必须是非 nil 数组(空数组而非 null),否则 OpenAI SDK 报
	// "output.content is not iterable" / 客户端框架判定 EMPTY_RESPONSE。
	outputContent := []map[string]any{}
	if text != "" {
		outputContent = append(outputContent, map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		})
	}

	// output 数组:reasoning item(思考链)+ message item + function_call items
	// 思考链默认不回传(模型思考但客户端无感),仅客户端显式传 reasoning 时输出。
	output := []map[string]any{}
	if exposeReasoning && reasoning.Len() > 0 {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + shortID(16),
			"summary": []any{},
			"content": []map[string]any{
				{"type": "reasoning_text", "text": reasoning.String()},
			},
		})
	}
	output = append(output, map[string]any{
		"type":    "message",
		"id":      "msg_" + shortID(16),
		"role":    "assistant",
		"status":  "completed",
		"content": outputContent,
	})
	for _, idx := range toolCallOrder {
		agg := toolCalls[idx]
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        agg.id,
			"call_id":   agg.id,
			"name":      renamer.Restore(agg.name),
			"arguments": agg.arguments.String(),
			"status":    "completed",
		})
	}

	respID := "resp_" + shortID(24)
	respObj := map[string]any{
		"id":         respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"model":      s.model,
		"status":     "completed",
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  0, // 未透传上游 usage
			"output_tokens": 0,
			"total_tokens":  0,
		},
	}
	writeJSON(w, http.StatusOK, respObj)
}

// responsesStreamResponse 流式响应：输出 SSE 事件序列。
func (s *Server) responsesStreamResponse(w http.ResponseWriter, resp *http.Response, renamer *toolconv.Renamer, exposeReasoning bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	respID := "resp_" + shortID(24)

	// response.created 事件
	// 注意:response 对象必须带 output 数组(空数组),否则 OpenAI SDK 的
	// cloneResponse/ensureCanonicalOutputText 遍历 snapshot.output 时报
	// "snapshot.output is not iterable"。
	created := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         respID,
			"object":     "response",
			"model":      s.model,
			"status":     "in_progress",
			"output":     []any{},
			"output_text": "",
		},
	}
	s.responsesEvent(w, flusher, "response.created", created)

	// 边收边输出 reasoning / text delta / function_call 参数增量
	fullText := ""
	fullReasoning := ""
	// 输出索引分配:reasoning/message/function_call 各占一个 output_index,
	// 客户端(pi-ai/openai SDK)按 output_index 关联 output_item.added 与 delta 事件。
	nextOutputIndex := 0
	reasoningStarted := false
	reasoningIndex := 0
	reasoningItemID := ""
	textStarted := false
	textIndex := 0
	textItemID := ""
	// 工具调用聚合:按 index 分组,首个 chunk 提供 id/name,后续 arguments 片段按序拼接
	type tcAgg struct {
		id          string
		name        string
		arguments   strings.Builder
		outputIndex int
	}
	toolCalls := map[int]*tcAgg{}
	var toolCallOrder []int
	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			return nil
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil
		}
		for _, c := range obj.Choices {
			if c.Delta.Reasoning != "" {
				fullReasoning += c.Delta.Reasoning
				// 思考链默认不回传,仅客户端显式传 reasoning 时转发 delta 事件。
				if exposeReasoning {
					// 首个 reasoning 片段:先发 output_item.added 创建 reasoning item,
					// 否则客户端(pi-ai/openai SDK)按 output_index 找不到 slot,丢弃 delta。
					if !reasoningStarted {
						reasoningStarted = true
						reasoningIndex = nextOutputIndex
						nextOutputIndex++
						reasoningItemID = "rs_" + shortID(16)
						s.responsesEvent(w, flusher, "response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": reasoningIndex,
							"item": map[string]any{
								"type":    "reasoning",
								"id":      reasoningItemID,
								"summary": []any{},
								"content": []map[string]any{
									{"type": "reasoning_text", "text": ""},
								},
								"status": "in_progress",
							},
						})
					}
					s.responsesEvent(w, flusher, "response.reasoning_text.delta", map[string]any{
						"type":          "response.reasoning_text.delta",
						"item_id":       reasoningItemID,
						"output_index":  reasoningIndex,
						"content_index": 0,
						"delta":         c.Delta.Reasoning,
					})
				}
			}
			if c.Delta.Content != "" {
				fullText += c.Delta.Content
				// 首个文本片段:先发 output_item.added 创建 message item。
				if !textStarted {
					textStarted = true
					textIndex = nextOutputIndex
					nextOutputIndex++
					textItemID = "msg_" + shortID(16)
					s.responsesEvent(w, flusher, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": textIndex,
						"item": map[string]any{
							"type":    "message",
							"id":      textItemID,
							"role":    "assistant",
							"status":  "in_progress",
							"content": []map[string]any{
								{"type": "output_text", "text": "", "annotations": []any{}},
							},
						},
					})
				}
				s.responsesEvent(w, flusher, "response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"item_id":       textItemID,
					"output_index":  textIndex,
					"content_index": 0,
					"delta":         c.Delta.Content,
				})
			}
			for _, tc := range c.Delta.ToolCalls {
				agg, ok := toolCalls[tc.Index]
				if !ok {
					agg = &tcAgg{}
					toolCalls[tc.Index] = agg
					toolCallOrder = append(toolCallOrder, tc.Index)
					// 新工具调用 item 出现:先发 output_item.added(OpenAI Responses 协议)
					// 注意:SDK 从 data JSON 的 type 字段识别事件类型,必须带上 type。
					agg.outputIndex = nextOutputIndex
					nextOutputIndex++
					s.responsesEvent(w, flusher, "response.output_item.added", map[string]any{
						"type":         "response.output_item.added",
						"output_index": agg.outputIndex,
						"item": map[string]any{
							"type":      "function_call",
							"id":        tc.ID,
							"call_id":   tc.ID,
							"name":      renamer.Restore(tc.Function.Name),
							"arguments": "",
							"status":    "in_progress",
						},
					})
				}
				if tc.ID != "" {
					agg.id = tc.ID
				}
				if tc.Function.Name != "" {
					agg.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					agg.arguments.WriteString(tc.Function.Arguments)
					// 流式转发参数增量(OpenAI Responses 协议)
					// 注意:SDK 从 data JSON 的 type 字段识别事件类型,必须带上 type。
					s.responsesEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
						"type":         "response.function_call_arguments.delta",
						"item_id":      agg.id,
						"output_index": agg.outputIndex,
						"delta":        tc.Function.Arguments,
					})
				}
			}
		}
		return nil
	})

	// 流结束:补发 output_item.done 终结各 item(客户端按此事件收尾 slot)
	if reasoningStarted {
		s.responsesEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": reasoningIndex,
			"item": map[string]any{
				"type":    "reasoning",
				"id":      reasoningItemID,
				"summary": []any{},
				"content": []map[string]any{
					{"type": "reasoning_text", "text": fullReasoning},
				},
				"status": "completed",
			},
		})
	}
	if textStarted {
		doneContent := []map[string]any{}
		if fullText != "" {
			doneContent = append(doneContent, map[string]any{
				"type":        "output_text",
				"text":        fullText,
				"annotations": []any{},
			})
		}
		s.responsesEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": textIndex,
			"item": map[string]any{
				"type":    "message",
				"id":      textItemID,
				"role":    "assistant",
				"status":  "completed",
				"content": doneContent,
			},
		})
	}
	for _, idx := range toolCallOrder {
		agg := toolCalls[idx]
		s.responsesEvent(w, flusher, "response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": agg.outputIndex,
			"item": map[string]any{
				"type":      "function_call",
				"id":        agg.id,
				"call_id":   agg.id,
				"name":      renamer.Restore(agg.name),
				"arguments": agg.arguments.String(),
				"status":    "completed",
			},
		})
	}

	// 完成事件（聚合所有输出）
	outputContent := []map[string]any{}
	if fullText != "" {
		outputContent = append(outputContent, map[string]any{
			"type":        "output_text",
			"text":        fullText,
			"annotations": []any{},
		})
	}
	output := []map[string]any{}
	if exposeReasoning && fullReasoning != "" {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + shortID(16),
			"summary": []any{},
			"content": []map[string]any{
				{"type": "reasoning_text", "text": fullReasoning},
			},
		})
	}
	output = append(output, map[string]any{
		"type":    "message",
		"id":      "msg_" + shortID(16),
		"role":    "assistant",
		"status":  "completed",
		"content": outputContent,
	})
	// 工具调用:聚合后输出 function_call items(与流式增量事件一致)
	for _, idx := range toolCallOrder {
		agg := toolCalls[idx]
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        agg.id,
			"call_id":   agg.id,
			"name":      renamer.Restore(agg.name),
			"arguments": agg.arguments.String(),
			"status":    "completed",
		})
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      s.model,
			"status":     "completed",
			"output":     output,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
				"total_tokens":  0,
			},
		},
	}
	s.responsesEvent(w, flusher, "response.completed", completed)
	// 标准 SSE 结束哨兵:SDK 检查 sse.data === "[DONE]",不能 JSON 包装。
	w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// responsesEvent 输出一个 Responses SSE 事件。
func (s *Server) responsesEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, v map[string]any) {
	if eventType != "" {
		w.Write([]byte("event: " + eventType + "\n"))
	}
	data, _ := json.Marshal(v)
	w.Write([]byte("data: " + string(data) + "\n\n"))
	flusher.Flush()
}

// extractResponsesReasoning 从 Responses API reasoning 参数提取 effort。
// reasoning 可以是 bool(true) 或对象 {effort:"low"|"medium"|"high"}。
func extractResponsesReasoning(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 尝试 bool
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return "high"
		}
		return ""
	}
	// 尝试对象 {effort: "..."}
	var obj struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Effort != "" {
		return obj.Effort
	}
	return ""
}
