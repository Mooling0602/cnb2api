package server

import "strings"

// reasoningDefault 是客户端未表态思考强度时注入的默认档位。
// 默认开启轻量思考以保证回答质量；需要完全不思考时，客户端显式传 off。
const reasoningDefault = "low"

// reasoningOff 是上游唯一接受、且真正不触发思考链的 wire 值（空串）。
const reasoningOff = ""

// normalizeReasoningEffort 把客户端给出的思考强度归一化为上游 wire 值。
//
// 上游 cnb.cool 的白名单（实测：大小写敏感、精确匹配、非法值直接 400）：
//
//	"" | none | minimal | low | medium | high | xhigh | max
//
// 空串是上游唯一接受且真正不触发思考链的取值；字面量 "off" 会被上游拒绝：
//
//	HTTP 400 {"code":11150,"msg":"the reasoning effort value is not supported by the current model"}
//
// 而省略该字段又会被本网关按默认值注入 reasoningDefault，因此这里把
// "off"（大小写不敏感）归一化为空串，作为「不思考」的统一表达。
//
// 其余取值一律原样透传：未知档位交由上游报错，避免网关硬编码档位列表、
// 在上游新增档位后失效。
func normalizeReasoningEffort(v string) string {
	if strings.EqualFold(v, "off") {
		return reasoningOff
	}
	return v
}

// reasoningEffortPtr 返回归一化后的思考强度指针。
// 返回非 nil 指针即会序列化为 "reasoning_effort":"<值>"（含空串），
// 空串正是上游的「不思考」。
func reasoningEffortPtr(v string) *string {
	normalized := normalizeReasoningEffort(v)
	return &normalized
}
