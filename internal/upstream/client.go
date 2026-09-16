// Package upstream 实现向 CNB /ai/chat/completions 发送请求并透传 SSE 流。
//
// 请求格式（逆向自前端 _app.js）：
//
//	POST /ai/chat/completions
//	Headers:
//	  Content-Type: application/json
//	  Csrftoken: <csrf token>          // 必须
//	  Cookie: csrfkey=<csrf key>        // 必须（与 token 配对）
//	  Origin/Referer: https://cnb.cool
//	Body:
//	  {
//	    "model": "deepseek-v4.1-flash",
//	    "stream": true,                 // 上游强制流式
//	    "messages": [...],
//	    "tools": [...],                 // 可选，NPC 自带工具
//	    "maxTokens": N,                 // 可选
//	    "enable_thinking": bool,        // 可选
//	    "presence_penalty": float       // 可选
//	  }
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cnb2api/internal/auth"
)

const (
	chatPath  = "/ai/chat/completions"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

// chatURL 返回当前上游聊天接口地址（跟随 auth.BaseURL 配置）。
func chatURL() string { return auth.BaseURL() + chatPath }

// ChatRequest 是 CNB chat/completions 的请求体（OpenAI 兼容超集）。
type ChatRequest struct {
	Model           string        `json:"model"`
	Stream          bool          `json:"stream"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []Tool        `json:"tools,omitempty"`
	ToolChoice      any           `json:"tool_choice,omitempty"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	ReasoningEffort *string       `json:"reasoning_effort,omitempty"`
	EnableThinking  *bool         `json:"enable_thinking,omitempty"`
	PresencePenalty *float64      `json:"presence_penalty,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	TopP            *float64      `json:"top_p,omitempty"`
}

// ChatMessage 是聊天消息。
//
// Content 为纯文本;需要携带图片时,把图片放进 Parts,序列化时会自动
// 展开成 OpenAI 多模态 content 数组(见 MarshalJSON)。
type ChatMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Parts      []ContentPart `json:"-"`                      // 图片等非文本块,非空时 content 输出为数组
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`   // assistant 消息的工具调用
	ToolCallID string        `json:"tool_call_id,omitempty"` // tool 消息对应的调用 ID
}

// ContentPart 是多模态 content 数组里的一个块。
// 文本块由 ChatMessage.Content 自动生成,这里只显式承载图片。
type ContentPart struct {
	Type     string    `json:"type"`                // "text" | "image_url"
	Text     string    `json:"text,omitempty"`      // Type=="text" 时有效
	ImageURL *ImageURL `json:"image_url,omitempty"` // Type=="image_url" 时有效
}

// ImageURL 是 image_url 块的内容。
// 上游只接受 data URL(base64 内联),远程 http(s) 地址会被上游 400 拒绝。
type ImageURL struct {
	URL string `json:"url"`
}

// MarshalJSON 在带图片时把 content 输出为多模态数组,否则保持字符串。
//
// 上游实测:content 既接受纯字符串,也接受 [{type:text},{type:image_url}] 数组;
// 只有图片没有文本(如 [{type:image_url}])同样可用。
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	type wireMessage struct {
		Role       string     `json:"role"`
		Content    any        `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	var content any = m.Content
	if len(m.Parts) > 0 {
		parts := make([]ContentPart, 0, len(m.Parts)+1)
		if m.Content != "" {
			parts = append(parts, ContentPart{Type: "text", Text: m.Content})
		}
		parts = append(parts, m.Parts...)
		content = parts
	}
	return json.Marshal(wireMessage{
		Role:       m.Role,
		Content:    content,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
	})
}

// HasImage 报告该消息是否携带图片。
func (m ChatMessage) HasImage() bool { return len(m.Parts) > 0 }

// ToolCall 是 assistant 消息里的工具调用（OpenAI 标准格式）。
// 注意:function.arguments 是 JSON 字符串,与工具定义里的 parameters(map)不同。
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 是工具调用的函数信息。
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 是函数工具定义。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 是工具函数定义。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// SSEChunk 是解析后的 SSE 数据块。
type SSEChunk struct {
	Raw    []byte // 原始 data 内容
	IsDone bool   // 是否为 [DONE]
}

// Client 是 CNB 上游客户端。
type Client struct {
	hc       *http.Client
	csrfPool *auth.Pool
}

// NewClient 创建上游客户端。
func NewClient(pool *auth.Pool, timeout time.Duration) *Client {
	return &Client{
		hc:       &http.Client{Timeout: timeout},
		csrfPool: pool,
	}
}

// MaxBodyBytes 是上游 CNB 外层网关的请求体硬上限。
//
// 实测二分定位:1048112 字节通过,1048688 字节返回
// {"errcode":413,"errmsg":"[BODY_TOO_LARGE]Request body too large"}。
// 注意该限制来自外层网关(错误格式为 errcode/errmsg),请求根本不会到达模型;
// 上游也不支持 Content-Encoding: gzip 绕过(实测返回 500)。
//
// 该常量仅用于诊断文案与体积预检提示,不作为拦截依据:是否超限始终由上游裁决,
// 避免上游放宽限制后网关仍按旧值拒绝请求。
const MaxBodyBytes = 1 << 20 // 1 MiB

// ErrBodyTooLarge 表示请求体超过上游网关上限,请求未送达模型。
//
// 这是客户端侧问题(与上游故障无关),网关据此返回 413 而非 502,
// 并附上实际体积与可操作建议。
type ErrBodyTooLarge struct {
	Size  int    // 实际请求体字节数
	Limit int    // 上游上限
	Raw   string // 上游原始响应
}

func (e *ErrBodyTooLarge) Error() string {
	return fmt.Sprintf("cnb: request body too large: %d bytes exceeds upstream limit %d bytes (1 MiB); "+
		"the upstream gateway rejected the request before it reached the model", e.Size, e.Limit)
}

// Chat 向 CNB 发送聊天请求，返回 SSE 流。调用方负责关闭 resp.Body。
// 请求成功(2xx)时返回 resp，resp.Body 关闭时自动归还凭证；遇凭证失效(401/403)会尝试换凭证重试。
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*http.Response, error) {
	// 强制流式（上游拒绝非流式）
	req.Stream = true

	// 序列化一次即可:请求体在重试之间不变,而图片内联后体积可观,
	// 放在循环内会在每次换凭证重试时重复序列化。
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// 内容清洗:上游 CNB 对特定 emoji(如台湾国旗 tw)触发 500 Internal Server Error
	// (实测任何消息角色 user/system/tool 均触发,属上游内容审核 bug)。
	// 网关在最后一公里做等价替换,规避上游 bug,非内容审查。
	body = sanitizeUpstreamBody(body)

	// 最多重试 3 次（换凭证）
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cs, err := c.csrfPool.Acquire()
		if err != nil {
			return nil, err
		}

		resp, err := c.doChat(ctx, cs, body)
		if err != nil {
			c.csrfPool.Report(cs, false)
			lastErr = err
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			// 成功：包装 body，关闭时自动归还凭证
			resp.Body = &reportCloser{ReadCloser: resp.Body, pool: c.csrfPool, cs: cs}
			return resp, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// 读取响应体判断是否为 CSRF 失效（可重试）还是业务拒绝（不可重试）
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			s := string(raw)
			if isCSRFError(s) {
				// 凭证失效，标记并重试
				c.csrfPool.Report(cs, false)
				lastErr = fmt.Errorf("cnb: csrf rejected (status %d): %s", resp.StatusCode, strings.TrimSpace(s))
				continue
			}
			// 业务拒绝（如 Agent calls not allowed）：不重试，直接透传
			c.csrfPool.Report(cs, true) // 凭证本身有效，不消耗错误计数
			return nil, fmt.Errorf("cnb: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(s))
		case http.StatusRequestEntityTooLarge:
			// 体积超限:重试无意义(同一具请求体必然再次被拒),立即返回可诊断的错误。
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			// 凭证本身有效,不消耗错误计数,并且必须归还 inUse,否则反复触发会撑爆凭证池。
			c.csrfPool.Report(cs, true)
			return nil, &ErrBodyTooLarge{Size: len(body), Limit: MaxBodyBytes, Raw: strings.TrimSpace(string(raw))}
		default:
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			// 归还凭证:该分支的错误来自客户端请求本身(参数/体积等),与凭证有效性无关。
			// 漏掉这次 Report 会让 inUse 计数永久泄漏,凭证池随失败次数不断膨胀。
			c.csrfPool.Report(cs, true)
			return nil, fmt.Errorf("cnb: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
	}
	return nil, fmt.Errorf("cnb: chat failed after retries: %w", lastErr)
}

// isCSRFError 判断响应体是否为 CSRF 校验失败（区别于业务拒绝）。
func isCSRFError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "csrf") ||
		strings.Contains(lower, "blocked by csrf") ||
		strings.Contains(lower, "csrf 校验失败")
}

// reportCloser 包装 resp.Body：关闭时自动归还凭证并上报结果。
type reportCloser struct {
	io.ReadCloser
	pool *auth.Pool
	cs   *auth.CSRF
}

func (r *reportCloser) Close() error {
	err := r.ReadCloser.Close()
	r.pool.Report(r.cs, err == nil)
	return err
}

// doChat 执行单次请求。body 为已序列化并清洗过的请求体(由 Chat 复用)。
func (c *Client) doChat(ctx context.Context, cs *auth.CSRF, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json, text/plain, */*")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Origin", "https://cnb.cool")
	httpReq.Header.Set("Referer", "https://cnb.cool/")
	httpReq.Header.Set("Csrftoken", cs.Token)
	httpReq.AddCookie(&http.Cookie{Name: "csrfkey", Value: cs.Key, Path: "/"})

	return c.hc.Do(httpReq)
}

// sanitizeUpstreamBody 清洗转发上游的请求体,规避上游已知 bug。
// 目前仅处理:台湾国旗 emoji 🇹🇼 → "tw"(上游对该 emoji 返回 500)。
func sanitizeUpstreamBody(body []byte) []byte {
	// 🇹🇼 是 U+1F1F9 U+1F1FC(regional indicator T + W)
	return bytes.ReplaceAll(body, []byte("\U0001F1F9\U0001F1FC"), []byte("tw"))
}

// ReadSSE 从响应体读取 SSE 事件，逐行回调。返回是否正常结束。
// data 行的内容会解析出来；`data: [DONE]` 触发 IsDone。
func ReadSSE(resp *http.Response, onChunk func(chunk SSEChunk) error) error {
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") &&
		resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cnb: not sse (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024) // 支持大 chunk

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return onChunk(SSEChunk{IsDone: true})
			}
			if err := onChunk(SSEChunk{Raw: []byte(payload)}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
