package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveContentKeyedSameIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 首次请求绑定云端对话，同一 IP/UA 但不同 user 账户。
	sr.Bind("", "conv-shared", "acc1",
		&oaiReq{User: "alice", Messages: []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "你好"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 续接请求来自同一 IP/UA（换 user 仍可命中，说明不做 user 拦截）。
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "bob"),
		&oaiReq{
			User: "bob",
			Messages: []oaiMsg{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "你好"},
				{Role: "user", Content: "多说点"},
			},
		})
	if res.IsNew {
		t.Fatal("同 IP/UA 前缀相同却未复用会话，内容键失效")
	}
	if res.MatchedBy != "context_prefix_2" {
		t.Fatalf("expected context_prefix_2, got %q", res.MatchedBy)
	}
	if res.ConversationID != "conv-shared" {
		t.Fatalf("expected conversation conv-shared, got %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 (增量起点), got %d", res.HistoryLen)
	}
}

func TestResolveDoesNotMatchAcrossIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-a", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 不同 IP / UA 的用户输入同样的短消息，不应串到别人的会话。
	res := sr.Resolve(resolverTestRequest("198.51.100.99", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("跨 IP/UA 的内容必须新建会话，got matched=%s conv=%s", res.MatchedBy, res.ConversationID)
	}
}

func TestResolveSingleMessageReusesForSameUser(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if res.IsNew {
		t.Fatalf("same user re-sending a message should reuse session, got IsNew=true")
	}
}

func TestResolveSingleMessageNeverReusesAcrossUsers(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.20", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("different user must not reuse session, got matched=%s", res.MatchedBy)
	}
}

func resolverTestRequest(ip, ua, user string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("User-Agent", ua)
	return r
}

// TestSuffixMatchRejectsToolTailedCrossTask is the regression test for the
// multi-task memory cross-talk: two concurrent tasks on the same box produce
// identical tool round tails (same tool call + same output, e.g. git status
// on the same repo). A 2-message tail of [assistant(tool_calls), tool(result)]
// must never bind task B to task A's cloud conversation, otherwise the
// upstream resumes task A's history and the local agent chases names that
// never existed in task B.
func TestSuffixMatchRejectsToolTailedCrossTask(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	gitCall := []map[string]any{{
		"id":       "call_old_id",
		"type":     "function",
		"function": map[string]any{"name": "bash", "arguments": `{"command":"git status"}`},
	}}
	toolResult := oaiMsg{Role: "tool", ToolCallID: "call_old_id", Content: "nothing to commit, working tree clean"}

	// Task A stored history ends with the tool round.
	sr.Bind("", "conv-taskA", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "检查 git 状态"},
			{Role: "assistant", ToolCalls: gitCall},
			toolResult,
		}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// Task B sends a DIFFERENT conversation whose tail happens to end with the
	// same tool call + identical tool output. Old code suffix-matched on the
	// 2-message tool tail and resumed task A; the fixed code must not.
	taskB := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "taskB 自己的问题"},
		{Role: "assistant", Content: "taskB 自己的回答"},
		{Role: "assistant", ToolCalls: gitCall},
		toolResult,
	}}
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "bob"), taskB)
	if !res.IsNew {
		t.Fatalf("tool-tailed suffix must NOT bind a different task's session, got matched=%s conv=%s", res.MatchedBy, res.ConversationID)
	}
}

// TestSuffixMatchAcceptsConversationalResume verifies a genuinely truncated
// client (kept recent turns, dropped old ones) still resumes its own session
// through the suffix path when the matched tail contains a user message.
func TestSuffixMatchAcceptsConversationalResume(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-taskA", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
			{Role: "assistant", Content: "第二轮回答"},
		}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// Client truncated its local history to the recent turns and re-sent them
	// (reload/retry after losing early history): the tail of the stored
	// history is reproduced verbatim, with a user message in the window.
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第二轮问题"},
			{Role: "assistant", Content: "第二轮回答"},
		}})
	if res.IsNew {
		t.Fatal("conversational suffix resume must reuse the session")
	}
	if res.ConversationID != "conv-taskA" {
		t.Fatalf("unexpected conversation %s", res.ConversationID)
	}

	// The completed turn re-binds history (as bindConversation does in the
	// real flow), then the following turn with the assistant reply included
	// continues through the normal prefix path.
	sr.Bind("", "conv-taskA", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第二轮问题"},
			{Role: "assistant", Content: "第二轮回答"},
		}},
		"第三轮回答",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))
	res2 := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第二轮问题"},
			{Role: "assistant", Content: "第二轮回答"},
			{Role: "assistant", Content: "第三轮回答"},
			{Role: "user", Content: "继续"},
		}})
	if res2.IsNew {
		t.Fatal("post-resume turn must reuse the session")
	}
	if res2.ConversationID != "conv-taskA" {
		t.Fatalf("unexpected conversation %s after resume", res2.ConversationID)
	}
}

func TestResolverIncrementalBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("", "conv-inc", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 第二轮只应发送历史之外的新增消息。
	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		}})
	if res.IsNew {
		t.Fatal("增量请求应复用以 2 轮历史为前缀的会话")
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2, got %d", res.HistoryLen)
	}

	// 内容不再是前一轮任何历史的前缀时不应误命中。
	res2 := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "全新问题完全无关"}}})
	if !res2.IsNew {
		t.Fatalf("不相关内容必须新建会话, got %s conv=%s", res2.MatchedBy, res2.ConversationID)
	}
}

func TestResolverEvictsAfterTTL(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("sess-old", "conv-old", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 把会话标记为超过默认 2h 闲置。
	sr.mu.Lock()
	old := sr.sessions["sess-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	sr.sessions["sess-old"] = old
	sr.mu.Unlock()

	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}})
	if !res.IsNew {
		t.Fatalf("闲置超 TTL 的会话应失效，got matched=%s", res.MatchedBy)
	}
}

func TestResolverPersistsHistoryAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)

	sr1 := openSessionResolver()
	sr1.Bind("", "conv-persist", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if err := sr1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新打开同一缓存文件，历史仍在 → 前缀仍可命中。
	sr2 := openSessionResolver()
	res := sr2.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
			{Role: "user", Content: "follow-up"},
		}})
	if res.IsNew {
		t.Fatal("contextHistory 应持久化，重启后仍可内容复用")
	}
	if res.ConversationID != "conv-persist" {
		t.Fatalf("unexpected conversation %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 after reload, got %d", res.HistoryLen)
	}
}

func TestAutoCleanupDefaultMaxAgeTwoHours(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	s := newTestServerForAutoCleanup(t)
	s.conversationManager.Record("conv-old", "acc1", "old")
	s.conversationManager.mu.Lock()
	old := s.conversationManager.data["conv-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	s.conversationManager.data["conv-old"] = old
	s.conversationManager.mu.Unlock()

	active := s.activeConversationSet(2 * time.Hour)
	if active["conv-old"] {
		t.Error("3h 闲置的会话不应在 2h 保护窗口内")
	}
}
