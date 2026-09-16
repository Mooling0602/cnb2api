package server

import (
	"errors"
	"fmt"
	"net/http"

	"cnb2api/internal/upstream"
)

// upstreamErrorStatus 把上游错误映射为对客户端更准确的状态码与文案。
//
// 默认仍是 502(上游故障),但请求体超限是**客户端侧问题**:上游外层网关在
// 请求到达模型前就拒绝了,重试或换凭证都没用。这种情况返回 413 并把「怎么改」
// 写进消息,而不是丢一个含糊的 502 让调用方以为上游挂了。
//
// 该分支只影响状态码与提示文案,不改变「是否发送请求」的判定。
func upstreamErrorStatus(err error) (int, map[string]any) {
	var tooLarge *upstream.ErrBodyTooLarge
	if errors.As(err, &tooLarge) {
		sizeMiB := float64(tooLarge.Size) / (1 << 20)
		limitMiB := float64(tooLarge.Limit) / (1 << 20)
		msg := fmt.Sprintf(
			"request body too large: %d bytes (%.2f MiB) exceeds the upstream gateway limit of %d bytes (%.0f MiB). "+
				"The request was rejected before reaching the model, so it cannot be retried. "+
				"Images are sent as base64, which inflates them by ~33%%; every message in the conversation history is resent each turn, "+
				"so images kept in history count against the limit on every request. "+
				"Reduce image size or count, or drop older images from the history.",
			tooLarge.Size, sizeMiB, tooLarge.Limit, limitMiB,
		)
		return http.StatusRequestEntityTooLarge, map[string]any{
			"error": map[string]any{
				"message": msg,
				"type":    "request_too_large",
				"code":    "body_too_large",
				"param":   nil,
			},
			// 便于程序化处理的结构化字段
			"body_bytes":  tooLarge.Size,
			"limit_bytes": tooLarge.Limit,
		}
	}
	return http.StatusBadGateway, map[string]any{
		"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
	}
}

// writeUpstreamError 按映射结果写出错误响应。
func writeUpstreamError(w http.ResponseWriter, err error) {
	code, payload := upstreamErrorStatus(err)
	writeJSON(w, code, payload)
}
