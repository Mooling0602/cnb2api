// Anthropic Messages API 适配层（/v1/messages、/anthropic/v1/messages）。
//
// 复用 cnb2api 现有管线：鉴权、CSRF 凭证池、上游请求、工具调用重塑。
// 本文件只做协议翻译：
//   - 输入：Anthropic 格式（system + messages + tools）→ 内部统一格式
//   - 输出：Anthropic 格式（非流式 message 对象 / 流式 SSE 事件序列）
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cnb2api/internal/toolconv"
	"cnb2api/internal/upstream"
)

// anthropicRequest 是 Anthropic Messages 请求格式（子集）。
type anthropicRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system"` // string 或 [{type:text,...}]
	Messages    []anthropicMsg  `json:"messages"`
	Tools       []anthropicTool `json:"tools"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	Reasoning   json.RawMessage `json:"thinking"` // Anthropic thinking: {type:"enabled",budget_tokens:N}
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 [blocks]
}

type anthropicTool = toolconv.AnthropicTool

// handleAnthropicMessages 处理 POST /v1/messages。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "messages is required"})
		return
	}

	// 构造上游请求（OpenAI 兼容内部格式）
	upReq := &upstream.ChatRequest{
		Model:     s.model,
		Stream:    true,
		Messages:  make([]upstream.ChatMessage, 0, len(req.Messages)+1),
		MaxTokens: req.MaxTokens,
	}
	if req.Temperature != nil {
		upReq.Temperature = req.Temperature
	}
	if req.TopP != nil {
		upReq.TopP = req.TopP
	}
	if len(req.Reasoning) > 0 {
		re := extractThinkingEffort(req.Reasoning)
		if re != nil {
			upReq.ReasoningEffort = reasoningEffortPtr(*re)
			enableThinking := true
			upReq.EnableThinking = &enableThinking
		}
	} else {
		// 默认思考:客户端未传 thinking 时,默认 low(最轻量)。
		upReq.ReasoningEffort = reasoningEffortPtr(reasoningDefault)
	}

	// system 字段 → system message
	if len(req.System) > 0 && string(req.System) != "null" {
		if sys := extractAnthropicText(req.System); sys != "" {
			upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "system", Content: sys})
		}
	}

	// 工具定义:统一提取 + 加 cnb_ 前缀(上游白名单要求),响应时还原,客户端无感。
	renamer := toolconv.NewRenamer()
	upReq.Tools = toolconv.ToUpstream(toolconv.FromAnthropic(req.Tools), renamer)

	// messages:Anthropic content 可能是 string 或 blocks。
	// 工具历史归一化(借鉴 new-api):assistant 的 tool_use → tool_calls,
	// user 的 tool_result → tool 消息 + tool_call_id(上游要求配对)。
	for _, m := range req.Messages {
		blocks, isBlocks := toolconv.ParseAnthropicContent(m.Content)
		if !isBlocks {
			// 纯字符串内容
			upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: m.Role, Content: extractAnthropicText(m.Content)})
			continue
		}
		switch m.Role {
		case "assistant":
			calls, text := toolconv.AnthropicCalls(blocks)
			parts := extractAnthropicImages(m.Content)
			if len(calls) > 0 {
				upCalls := make([]upstream.ToolCall, 0, len(calls))
				for _, c := range calls {
					upCalls = append(upCalls, upstream.ToolCall{
						ID:   c.ID,
						Type: "function",
						Function: upstream.ToolCallFunction{
							Name:      renamer.Forward(c.Name),
							Arguments: c.Arguments,
						},
					})
				}
				upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "assistant", Content: text, Parts: parts, ToolCalls: upCalls})
			} else {
				upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "assistant", Content: text, Parts: parts})
			}
		case "user":
			results, text := toolconv.AnthropicResults(blocks)
			parts := extractAnthropicImages(m.Content)
			if len(results) > 0 {
				for _, res := range results {
					upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "tool", Content: res.Content, ToolCallID: res.CallID})
				}
				// text 为空但有图片时同样要发,否则纯图消息会被丢掉。
				if text != "" || len(parts) > 0 {
					upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "user", Content: text, Parts: parts})
				}
			} else {
				upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "user", Content: text, Parts: parts})
			}
		default:
			upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: m.Role, Content: extractAnthropicText(m.Content), Parts: extractAnthropicImages(m.Content)})
		}
	}

	ctx := r.Context()
	resp, err := s.upstream.Chat(ctx, upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		s.anthropicStreamResponse(w, resp, renamer)
	} else {
		s.anthropicNonStreamResponse(w, resp, renamer)
	}
}

// handleAnthropicCountTokens 处理 POST /v1/messages/count_tokens。
// 粗略估算 input token 数（与 qwen2api-rs 一致的 ×1.35 通胀系数）。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []anthropicMsg  `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	var sb strings.Builder
	if len(req.System) > 0 && string(req.System) != "null" {
		sb.WriteString(extractAnthropicText(req.System))
		sb.WriteString("\n")
	}
	for _, m := range req.Messages {
		sb.WriteString(extractAnthropicText(m.Content))
		sb.WriteString("\n")
	}
	// 粗略估算：中文字符 ≈ 1 token，其他 ≈ 4 字符/token
	text := sb.String()
	cjk := 0
	for _, r := range text {
		if r > 0x2E80 {
			cjk++
		}
	}
	other := len(text) - cjk
	base := cjk + other/4
	inflated := int(float64(base) * 1.35)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": inflated})
}

// extractAnthropicText 从 Anthropic content 提取纯文本（支持 string 或 blocks 数组）。
func extractAnthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 尝试 string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 尝试 blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				sb.WriteString(b.Text)
			case "tool_use":
				sb.WriteString("<tool_use name=\"" + b.Name + "\">")
			case "tool_result":
				sb.WriteString("(tool result)")
			}
			sb.WriteString("\n")
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// ── 非流式 Anthropic message 响应 ──

func (s *Server) anthropicNonStreamResponse(w http.ResponseWriter, resp *http.Response, renamer *toolconv.Renamer) {
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
	var contentBlocks []map[string]any
	// 思考链:上游 reasoning_content → Anthropic thinking block(置于 text 之前)
	if reasoning.Len() > 0 {
		contentBlocks = append(contentBlocks, map[string]any{
			"type":      "thinking",
			"thinking":  reasoning.String(),
			"signature": "",
		})
	}
	if text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}
	// 有工具调用时输出 tool_use block(arguments 是 JSON 字符串,unmarshal 成 map)
	stopReason := "end_turn"
	if len(toolCallOrder) > 0 {
		stopReason = "tool_use"
		for _, idx := range toolCallOrder {
			agg := toolCalls[idx]
			var input map[string]any
			if agg.arguments.Len() > 0 {
				_ = json.Unmarshal([]byte(agg.arguments.String()), &input)
			}
			if input == nil {
				input = map[string]any{}
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type":  "tool_use",
				"id":    agg.id,
				"name":  renamer.Restore(agg.name),
				"input": input,
			})
		}
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": ""})
	}

	respObj := map[string]any{
		"id":            "msg_" + shortID(12),
		"type":          "message",
		"role":          "assistant",
		"model":         s.model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0, // 上游 usage 未透传，置 0
			"output_tokens": 0,
		},
	}
	writeJSON(w, http.StatusOK, respObj)
}

// ── 流式 Anthropic SSE ──

func (s *Server) anthropicStreamResponse(w http.ResponseWriter, resp *http.Response, renamer *toolconv.Renamer) {
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

	msgID := "msg_" + shortID(12)

	// message_start
	start := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	s.anthropicEvent(w, flusher, start)

	// 边收边转发 thinking / text / tool_use blocks
	blockOpen := false    // text block 是否已打开
	thinkingOpen := false // thinking block 是否已打开
	blockIndex := 0
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
			// 思考链:reasoning_content → thinking block(先于 text)
			if c.Delta.Reasoning != "" {
				if !thinkingOpen {
					s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
					thinkingOpen = true
				}
				s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": c.Delta.Reasoning}})
			}
			text := c.Delta.Content
			if text != "" {
				if !blockOpen {
					s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
					blockOpen = true
				}
				s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "text_delta", "text": text}})
			}
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
		flusher.Flush()
		return nil
	})
	if thinkingOpen {
		s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	if blockOpen {
		s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	// 有工具调用时输出 tool_use 事件序列
	stopReason := "end_turn"
	if len(toolCallOrder) > 0 {
		stopReason = "tool_use"
		for _, idx := range toolCallOrder {
			agg := toolCalls[idx]
			s.anthropicEvent(w, flusher, map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    agg.id,
					"name":  renamer.Restore(agg.name),
					"input": map[string]any{},
				},
			})
			if agg.arguments.Len() > 0 {
				s.anthropicEvent(w, flusher, map[string]any{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": agg.arguments.String()},
				})
			}
			s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_stop", "index": blockIndex})
			blockIndex++
		}
	}
	s.anthropicEvent(w, flusher, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	s.anthropicEvent(w, flusher, map[string]any{"type": "message_stop"})
	flusher.Flush()
}

// anthropicEvent 输出一个 Anthropic SSE 事件。
func (s *Server) anthropicEvent(w http.ResponseWriter, flusher http.Flusher, v map[string]any) {
	data, _ := json.Marshal(v)
	w.Write([]byte("event: " + v["type"].(string) + "\n"))
	w.Write([]byte("data: " + string(data) + "\n\n"))
	flusher.Flush()
}

// extractThinkingEffort 从 Anthropic thinking 参数推断 reasoning effort。
func extractThinkingEffort(raw json.RawMessage) *string {
	if len(raw) == 0 {
		return nil
	}
	var obj struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type != "enabled" {
		return nil
	}
	switch {
	case obj.BudgetTokens <= 1000:
		s := "low"
		return &s
	case obj.BudgetTokens <= 10000:
		s := "medium"
		return &s
	default:
		s := "high"
		return &s
	}
}
