package web

import (
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func TestRetryDedupReusesPinnedConversation(t *testing.T) {
	s := &Server{retryDedup: map[string]retryDedupEntry{}}
	tenant := "tenant-a"
	msgs := []oaiMsg{
		{Role: "user", Content: "第一条"},
		{Role: "assistant", Content: "回复1"},
		{Role: "user", Content: "继续"},
	}

	// 首次:无命中
	if _, ok := s.lookupRetry(tenant, msgs); ok {
		t.Fatal("首次请求不应命中去重")
	}

	// 绑定
	body := &oaiReq{Messages: msgs}
	res := chathub.Result{ConversationID: "conv-123", SessionID: "sess-456"}
	acc := auth.AccountToken{ID: "acc-789"}
	s.rememberRetry(tenant, body, res, acc)

	// 同 tenant 命中
	e, ok := s.lookupRetry(tenant, msgs)
	if !ok {
		t.Fatal("同 tenant 相同尾部应命中去重")
	}
	if e.ConversationID != "conv-123" || e.AccountID != "acc-789" {
		t.Fatalf("去重应返回首次绑定的对话/账号: %+v", e)
	}

	// 不同 tenant 不命中
	if _, ok := s.lookupRetry("tenant-b", msgs); ok {
		t.Fatal("不同 tenant 不应命中")
	}

	// 过期不命中
	s.retryDedupMtx.Lock()
	s.retryDedup[retryDedupKey(tenant, msgs)] = retryDedupEntry{ConversationID: "conv-123", At: time.Now().Add(-2 * time.Minute)}
	s.retryDedupMtx.Unlock()
	if _, ok := s.lookupRetry(tenant, msgs); ok {
		t.Fatal("过期条目不应命中")
	}
}

func TestRetryDedupKeyMatchesSameTail(t *testing.T) {
	// 相同尾部消息(最后3条) + 相同 tenant → key 一致
	msgsA := []oaiMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "第一轮提问"},
		{Role: "assistant", Content: "第一轮回复"},
		{Role: "user", Content: "继续"},
	}
	msgsB := []oaiMsg{
		{Role: "user", Content: "第一轮提问"},
		{Role: "assistant", Content: "第一轮回复"},
		{Role: "user", Content: "继续"},
	}
	ka := retryDedupKey("t", msgsA[1:]) // 尾部3条
	kb := retryDedupKey("t", msgsB)
	if ka != kb {
		t.Fatalf("相同尾部应产生相同 key: %s vs %s", ka, kb)
	}

	// 不同尾部 → key 不同
	msgsC := []oaiMsg{
		{Role: "user", Content: "第一轮提问"},
		{Role: "assistant", Content: "第一轮回复"},
		{Role: "user", Content: "换个问题"},
	}
	kc := retryDedupKey("t", msgsC)
	if ka == kc {
		t.Fatal("不同尾部应产生不同 key")
	}
}