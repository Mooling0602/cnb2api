package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cnb2api/internal/auth"
)

// newTestClient 起一个假上游并返回指向它的 Client。
// handler 决定 POST /ai/chat/completions 的行为。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *auth.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// 模拟首页:提供 csrftoken 与 csrfkey,供 auth.Fetch 取凭证
			http.SetCookie(w, &http.Cookie{Name: "csrfkey", Value: strings.Repeat("b", 32), Path: "/"})
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script id="cnb-csrftoken-script">window.csrftoken="` + strings.Repeat("a", 32) + `"</script>`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	// 让 auth.Fetch/doChat 指向假上游
	prev := auth.BaseURL()
	auth.SetBaseURL(srv.URL)
	t.Cleanup(func() { auth.SetBaseURL(prev) })

	pool, err := auth.NewPool(auth.PoolConfig{MinSize: 1, MaxSize: 8, TTL: 30 * time.Minute, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewClient(pool, 10*time.Second), pool
}

// TestNoCredentialLeakOn413 是本次修复的回归测试。
//
// 修复前:413 走 default 分支且未调用 Report,每次失败都让凭证的 inUse 计数
// 永久 +1,凭证池随失败次数不断膨胀(实测连发 6 次 413:池 1→6)。
func TestNoCredentialLeakOn413(t *testing.T) {
	client, pool := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"errcode":413,"errmsg":"[BODY_TOO_LARGE]Request body too large"}`))
	})

	before := pool.Count()
	for i := 0; i < 6; i++ {
		_, err := client.Chat(context.Background(), &ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ChatMessage{{Role: "user", Content: "x"}},
		})
		var tooLarge *ErrBodyTooLarge
		if !errors.As(err, &tooLarge) {
			t.Fatalf("第 %d 次应返回 ErrBodyTooLarge,实际 %v", i+1, err)
		}
	}
	after := pool.Count()
	if after != before {
		t.Fatalf("413 不应导致凭证池膨胀: before=%d after=%d", before, after)
	}
}

// TestNoCredentialLeakOnOtherErrors 覆盖 default 分支的其它状态码。
func TestNoCredentialLeakOnOtherErrors(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusTeapot} {
		client, pool := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"boom":true}`))
		})
		before := pool.Count()
		for i := 0; i < 5; i++ {
			if _, err := client.Chat(context.Background(), &ChatRequest{
				Model:    "m",
				Messages: []ChatMessage{{Role: "user", Content: "x"}},
			}); err == nil {
				t.Fatalf("status %d 应返回错误", status)
			}
		}
		if after := pool.Count(); after != before {
			t.Errorf("status %d 导致池膨胀: before=%d after=%d", status, before, after)
		}
	}
}

// TestChatRequestBodySerializedOnce 确认图片等大体积内容只序列化一次,
// 且每次重试发送的是同一份 body(避免重复放大)。
func TestChatRequestBodySerializedOnce(t *testing.T) {
	var sizes []int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := 0
		buf := make([]byte, 32*1024)
		for {
			k, err := r.Body.Read(buf)
			n += k
			if err != nil {
				break
			}
		}
		sizes = append(sizes, n)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"boom":true}`))
	})

	img := "data:image/png;base64," + strings.Repeat("A", 50000)
	_, _ = client.Chat(context.Background(), &ChatRequest{
		Model: "m",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "看图",
			Parts:   []ContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: img}}},
		}},
	})

	if len(sizes) == 0 {
		t.Fatal("假上游未收到请求")
	}
	// 每个请求体都应含且仅含一份图片数据
	for i, n := range sizes {
		if n < len(img) {
			t.Errorf("第 %d 次请求体 %d 字节,小于单张图片数据 %d 字节", i+1, n, len(img))
		}
		if n > len(img)*2 {
			t.Errorf("第 %d 次请求体 %d 字节,疑似图片被重复放大(单图 %d)", i+1, n, len(img))
		}
	}
	// 重试之间体积应完全一致
	for i := 1; i < len(sizes); i++ {
		if sizes[i] != sizes[0] {
			t.Errorf("重试请求体大小不一致: %v", sizes)
			break
		}
	}
}

// TestBodyTooLargeReportsActualSize 确认错误里带的是真实序列化体积。
func TestBodyTooLargeReportsActualSize(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"errcode":413}`))
	})

	img := "data:image/png;base64," + strings.Repeat("B", 120000)
	_, err := client.Chat(context.Background(), &ChatRequest{
		Model: "m",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "看图",
			Parts:   []ContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: img}}},
		}},
	})
	var tooLarge *ErrBodyTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("应返回 ErrBodyTooLarge,实际 %v", err)
	}
	// 实际 body 一定大于图片数据本身
	if tooLarge.Size < len(img) {
		t.Errorf("上报体积 %d 应不小于图片数据 %d", tooLarge.Size, len(img))
	}
	if tooLarge.Limit != MaxBodyBytes {
		t.Errorf("Limit 应为 %d,实际 %d", MaxBodyBytes, tooLarge.Limit)
	}
	// 序列化后的 JSON 体积应与上报值一致(允许少量包装开销)
	body, _ := json.Marshal(&ChatRequest{
		Model: "m", Stream: true,
		Messages: []ChatMessage{{
			Role:    "user",
			Content: "看图",
			Parts:   []ContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: img}}},
		}},
	})
	if diff := tooLarge.Size - len(body); diff > 64 || diff < -64 {
		t.Errorf("上报体积 %d 与实际序列化 %d 偏差过大", tooLarge.Size, len(body))
	}
}

// TestChatBodyTooLargeNotRetried 确认体积超限不做无谓重试:
// 同一份 body 重试必然再被拒,且会浪费凭证。
func TestChatBodyTooLargeNotRetried(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"errcode":413}`))
	})
	_, _ = client.Chat(context.Background(), &ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if calls != 1 {
		t.Errorf("体积超限应立即返回,不应重试;实际请求了 %d 次", calls)
	}
}

// TestCSRFErrorStillRetries 确认本次重构没有破坏原有的「凭证失效则换凭证重试」逻辑。
func TestCSRFErrorStillRetries(t *testing.T) {
	var calls int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"msg":"blocked by csrf"}`))
	})
	_, err := client.Chat(context.Background(), &ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "x"}},
	})
	if err == nil {
		t.Fatal("CSRF 持续失败应返回错误")
	}
	if calls != 3 {
		t.Errorf("CSRF 失效应重试至 3 次,实际 %d 次", calls)
	}
	if !strings.Contains(err.Error(), "csrf") {
		t.Errorf("错误应提及 csrf: %v", err)
	}
}
