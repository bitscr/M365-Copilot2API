package web

import (
	"path/filepath"
	"testing"
)

// TestBindingContentHashFormIndependent: the same logical tool call in text
// "CALL_TOOL:" form and structured tool_calls form must produce the same
// canonical full-history hash, so Bind can detect representation changes.
func TestBindingContentHashFormIndependent(t *testing.T) {
	textForm := []oaiMsg{
		{Role: "user", Content: "run check"},
		{Role: "assistant", Content: `CALL_TOOL: terminal({"command":"id","timeout":5})`},
		{Role: "tool", ToolCallID: "call_1", Content: `{"output":"uid=0"}`},
	}
	structForm := []oaiMsg{
		{Role: "user", Content: "run check"},
		{Role: "assistant", Content: "", ToolCalls: []map[string]any{{
			"id": "call_9", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": `{"timeout":5,"command":"id"}`},
		}}},
		{Role: "tool", ToolCallID: "call_9", Content: `{"output":"uid=0"}`},
	}
	ha, hb := bindingContentHash(textForm), bindingContentHash(structForm)
	if ha != hb {
		t.Fatalf("text CALL_TOOL and structured tool_calls must hash equal:\n  text=%s\n  stru=%s", ha, hb)
	}
}

// TestBindingContentHashDiffersOnDifferentContent: different tool arguments
// must NOT collide.
func TestBindingContentHashDiffersOnDifferentContent(t *testing.T) {
	a := []oaiMsg{{Role: "user", Content: "do A"}, {Role: "assistant", Content: `CALL_TOOL: terminal({"command":"id"})`}}
	b := []oaiMsg{{Role: "user", Content: "do B"}, {Role: "assistant", Content: `CALL_TOOL: terminal({"command":"pwd"})`}}
	if bindingContentHash(a) == bindingContentHash(b) {
		t.Fatal("different content must not hash equal")
	}
}

// TestBindMigratesOnContentHash: when the upstream hands out a NEW cloud
// ConversationID for the SAME logical history, Bind must migrate the existing
// binding instead of forking a duplicate sessionBinding.
func TestBindMigratesOnContentHash(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	history := []oaiMsg{
		{Role: "user", Content: "remember ZEBRA-4417"},
		{Role: "assistant", Content: "ok"},
	}
	// First bind: cloud conversation c1.
	sr.Bind("", "c1", "acc-x", &oaiReq{Messages: history}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("expected 1 session after first bind, got %d", got)
	}

	// Same history reappears but upstream returned a new cloud id c2
	// (full-context replay). Must migrate c1's binding, not fork.
	sr.Bind("", "c2", "acc-x", &oaiReq{Messages: history}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("expected 1 session after content-key migration, got %d (fork!)", got)
	}
	for k, sess := range sr.sessions {
		if sess.ConversationID != "c2" {
			t.Fatalf("session %s should have migrated to c2, still c1: %q", k, sess.ConversationID)
		}
	}
}

// TestBindDoesNotMergeDifferentContent: distinct histories with same account
// must stay distinct — content-key migration must never merge unrelated work.
func TestBindDoesNotMergeDifferentContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	sr.Bind("", "c1", "acc-x", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "task one"}}}, "", req)
	sr.Bind("", "c2", "acc-x", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "task two"}}}, "", req)
	if got := len(sr.sessions); got != 2 {
		t.Fatalf("expected 2 distinct sessions, got %d (wrong merge!)", got)
	}
}

// TestBindConvergesPrefixExtensionFrames: the client replays a full-context
// history that grew by one turn (9→11→13 messages), each frame a strict prefix
// of the next — the observed "修复tg不通" fragmentation. Bind must converge
// all frames onto ONE binding, migrating the cloud id forward.
func TestBindConvergesPrefixExtensionFrames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	frame1 := []oaiMsg{
		{Role: "user", Content: "修复tg不通的情况"},
		{Role: "assistant", Content: `CALL_TOOL: skill_view({"name":"hermes-agent"})`},
		{Role: "tool", ToolCallID: "call_1", Content: `{"success":true}`},
	}
	frame2 := append(append([]oaiMsg{}, frame1...), oaiMsg{
		Role: "assistant", Content: `CALL_TOOL: terminal({"command":"echo hi"})`,
	})
	frame3 := append(append([]oaiMsg{}, frame2...), oaiMsg{
		Role: "tool", ToolCallID: "call_2", Content: `{"output":"hi"}`,
	})

	sr.Bind("", "c1", "acc-x", &oaiReq{Messages: frame1}, "", req)
	sr.Bind("", "c2", "acc-x", &oaiReq{Messages: frame2}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("after 2 prefix frames expected 1 session, got %d", got)
	}
	sr.Bind("", "c3", "acc-x", &oaiReq{Messages: frame3}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("after 3 prefix frames expected 1 session, got %d", got)
	}
	for k, sess := range sr.sessions {
		if sess.ConversationID != "c3" {
			t.Fatalf("session %s should carry latest cloud id c3, got %q", k, sess.ConversationID)
		}
		if len(sess.ContextHistory) != len(frame3) {
			t.Fatalf("session should hold longest history %d, got %d", len(frame3), len(sess.ContextHistory))
		}
	}
}

// TestBindPrefixInheritanceToleratesFormChange: a frame that replays the same
// logical tool call in a DIFFERENT representation (structured tool_calls vs
// text CALL_TOOL) must still converge — prefix equality uses messagesEqual.
func TestBindPrefixInheritanceToleratesFormChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	frame1 := []oaiMsg{
		{Role: "user", Content: "run check"},
		{Role: "assistant", Content: `CALL_TOOL: terminal({"command":"id"})`},
	}
	frame2 := []oaiMsg{
		{Role: "user", Content: "run check"},
		{Role: "assistant", Content: "", ToolCalls: []map[string]any{{
			"id": "call_9", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": `{"command":"id"}`},
		}}},
		{Role: "tool", ToolCallID: "call_9", Content: `{"output":"uid=0"}`},
	}
	sr.Bind("", "c1", "acc-x", &oaiReq{Messages: frame1}, "", req)
	sr.Bind("", "c2", "acc-x", &oaiReq{Messages: frame2}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("form-change prefix frame must converge, got %d sessions", got)
	}
}

// TestBindPrefixInheritanceToleratesEmptySemantics: a frame replaying the same
// logical turn as "(empty)" / "" / "NO_TOOL_NEEDED" must converge — no-text
// assistant turns normalize to the same empty semantics in both messagesEqual
// and the canonical hash. Regression for the b8efef2f / 4653cac6 fork.
func TestBindPrefixInheritanceToleratesEmptySemantics(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	frame1 := []oaiMsg{
		{Role: "user", Content: "恢复tg不通"},
		{Role: "assistant", Content: "NO_TOOL_NEEDED", ReasoningContent: "task done"},
	}
	// same logical turn replayed as the Hermes "(empty)" placeholder, plus the
	// injected retry turn — the real 23-message continuation of the fork.
	frame2 := []oaiMsg{
		{Role: "user", Content: "恢复tg不通"},
		{Role: "assistant", Content: "(empty)"},
		{Role: "user", Content: "You just executed tool calls but returned an empty response. Please process the tool results above and continue with the task."},
	}

	sr.Bind("", "c1", "acc-x", &oaiReq{Messages: frame1}, "", req)
	sr.Bind("", "c2", "acc-x", &oaiReq{Messages: frame2}, "", req)
	if got := len(sr.sessions); got != 1 {
		t.Fatalf("empty-semantics prefix frame must converge to 1 session, got %d (b8efef2f/4653cac6 regression)", got)
	}
	for k, sess := range sr.sessions {
		if sess.ConversationID != "c2" {
			t.Fatalf("session %s should carry latest cloud id c2, got %q", k, sess.ConversationID)
		}
	}
}

// TestIsBareNoToolNeeded covers the B'' answer-path normalization: only the
// bare stop token counts; any real text (even short) passes through.
func TestIsBareNoToolNeeded(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"NO_TOOL_NEEDED", true},
		{"  NO_TOOL_NEEDED  ", true},
		{"no_tool_needed", true},
		{"", false},
		{"已恢复 Telegram 连接:Hermes Gateway 已重启,PID 2564", false},
		{"NO_TOOL_NEEDED but here's the summary: done", false},
		{"NO_TOOL_NEEDED\n\nEverything is fine now.", false},
		{"(empty)", false},
		{"I'll check the gateway status now.", false},
	}
	for _, c := range cases {
		if got := isBareNoToolNeeded(c.in); got != c.want {
			t.Errorf("isBareNoToolNeeded(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestBindPrefixInheritanceDoesNotMergeDifferentThreads: histories that share
// a long common prefix but DIVERGE (different task tails) must NOT converge —
// only strict prefix extensions qualify.
func TestBindPrefixInheritanceDoesNotMergeDifferentThreads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	sr := openSessionResolver()
	req := resolverTestRequest("203.0.113.10", "client-a", "alice")

	common := []oaiMsg{
		{Role: "user", Content: "fix the gateway"},
		{Role: "assistant", Content: "checking..."},
	}
	taskA := append(append([]oaiMsg{}, common...), oaiMsg{Role: "assistant", Content: `CALL_TOOL: terminal({"command":"hermes gateway start"})`})
	taskB := append(append([]oaiMsg{}, common...), oaiMsg{Role: "assistant", Content: `CALL_TOOL: terminal({"command":"pkill hermes"})`})
	sr.Bind("", "cA", "acc-x", &oaiReq{Messages: taskA}, "", req)
	sr.Bind("", "cB", "acc-x", &oaiReq{Messages: taskB}, "", req)
	if got := len(sr.sessions); got != 2 {
		t.Fatalf("diverged threads must stay separate, got %d sessions", got)
	}
}