package server

import (
	"encoding/json"

	"cnb2api/internal/upstream"
)

// extractResponsesImages 从 Responses 协议的 content parts 中提取图片块。
//
// Responses 格式与 Chat 不同:image_url 是**裸字符串**而非嵌套对象,
//
//	{"type":"input_image","image_url":"data:image/png;base64,..."}
//
// 归一化为上游接受的 image_url 块。
func extractResponsesImages(content json.RawMessage) []upstream.ContentPart {
	if len(content) == 0 || string(content) == "null" {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var out []upstream.ContentPart
	for _, p := range parts {
		if p.Type != "input_image" && p.Type != "image_url" && p.Type != "image" {
			continue
		}
		if p.ImageURL == "" {
			continue
		}
		out = append(out, upstream.ContentPart{
			Type:     "image_url",
			ImageURL: &upstream.ImageURL{URL: p.ImageURL},
		})
	}
	return out
}

// extractAnthropicImages 从 Anthropic content blocks 中提取图片块。
//
// Anthropic 格式为 {"type":"image","source":{"type":"base64","media_type":"...","data":"..."}},
// 转换为上游接受的 data URL。source.type 为 "url" 时上游不支持(远程地址会被拒),
// 此处按原样拼成 URL 透传,由上游给出明确报错。
func extractAnthropicImages(content json.RawMessage) []upstream.ContentPart {
	if len(content) == 0 || string(content) == "null" {
		return nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Source *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var out []upstream.ContentPart
	for _, b := range blocks {
		if b.Type != "image" || b.Source == nil {
			continue
		}
		var url string
		switch {
		case b.Source.Data != "":
			mime := b.Source.MediaType
			if mime == "" {
				mime = "image/png"
			}
			url = "data:" + mime + ";base64," + b.Source.Data
		case b.Source.URL != "":
			url = b.Source.URL
		default:
			continue
		}
		out = append(out, upstream.ContentPart{
			Type:     "image_url",
			ImageURL: &upstream.ImageURL{URL: url},
		})
	}
	return out
}

// 上游实测(cnb.cool /ai/chat/completions):
//   - content 既接受纯字符串,也接受 [{type:text},{type:image_url}] 数组;
//     只有图片没有文本的数组同样可用。
//   - 图片必须内联为 data URL(base64),远程 http(s) 地址会被上游 400 拒绝
//     (code 11133)。客户端应自行下载后内联。
//   - mime 白名单实测较宽松:image/png、image/jpeg、image/jpg、image/gif、
//     image/webp 可用(大小写、";charset=..." 后缀、前后空格均被接受);
//     image/bmp、image/svg+xml 被拒。上游只看声明的 mime 字符串,
//     不校验字节与 mime 是否一致。
//   - 图片可出现在 user / system / assistant / tool 任一角色,单条消息可带多图。
//
// 这里只做结构与类型判定,不校验 mime 与 base64 合法性,一律原样透传交由
// 上游裁决——与 reasoning_effort 同一原则:网关不硬编码上游的取值列表,
// 避免上游放宽或收紧后网关结论过期。
func extractChatImages(raw json.RawMessage) []upstream.ContentPart {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	var out []upstream.ContentPart
	for _, p := range parts {
		if p.Type != "image_url" || p.ImageURL == nil {
			continue
		}
		out = append(out, upstream.ContentPart{
			Type:     "image_url",
			ImageURL: &upstream.ImageURL{URL: p.ImageURL.URL},
		})
	}
	return out
}
