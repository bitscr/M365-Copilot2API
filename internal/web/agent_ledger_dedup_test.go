package web

import "testing"

func TestCanonicalToolArgumentsDeduplicateEquivalentJSON(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"x"}`,
	}}}
	if !ledger.hasCompleted("workspace_write_file", ` { "content":"x", "path":"main.go" } `) {
		t.Fatal("equivalent JSON arguments were not deduplicated")
	}
}

func TestFilterCompletedCallsKeepsNewArguments(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"old"}`,
	}}}
	calls := []detectedToolCall{
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"old"}`)},
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"new"}`)},
	}
	got := filterCompletedCalls(calls, ledger)
	if len(got) != 1 || string(got[0].Arguments) != `{"path":"main.go","content":"new"}` {
		t.Fatalf("unexpected filtered calls: %#v", got)
	}
}

func TestRouterContextStaysCompact(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "workspace_write_file", Arguments: `{"path":"main.go"}`, Result: "written successfully",
	}}}
	ctx := ledger.RouterContext()
	if len(ctx) > 2000 {
		t.Fatalf("router context unexpectedly large: %d bytes", len(ctx))
	}
	if len(ctx) == 0 {
		t.Fatal("router context is empty")
	}
}

// TestAgentLedgerMemoryRetryTolerant: Hermes memory-review turns retry the
// SAME memory call with identical arguments while probing old_text matches
// (seen: 3x memory replace with identical 847-byte args after "No entry
// matched"). That loop is client memory maintenance, not a stuck model —
// counting it toward StuckLoop produced a hard 409 that stalled the user's
// real task (00:18:18 be7f76e6). Memory-family tools must be exempt from the
// repeated-call / repeated-failure / stuck-loop bookkeeping.
func TestAgentLedgerMemoryRetryTolerant(t *testing.T) {
	args := `{"action":"replace","content":"Minecraft bot at /opt/minecraft-bot","old_text":"Minecraft bot","target":"memory"}`
	failure := `{"success": false, "error": "No entry matched 'Minecraft bot'"}`
	msgs := []oaiMsg{
		{Role: "user", Content: "Review the conversation above and consider saving to memory."},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "memory", "arguments": args}}}},
		{Role: "tool", ToolCallID: "call_1", Content: failure},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_2", "type": "function", "function": map[string]any{"name": "memory", "arguments": args}}}},
		{Role: "tool", ToolCallID: "call_2", Content: failure},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_3", "type": "function", "function": map[string]any{"name": "memory", "arguments": args}}}},
		{Role: "tool", ToolCallID: "call_3", Content: failure},
	}
	l := buildAgentLedger(msgs)
	if l.StuckLoop {
		t.Fatal("memory retry chain must NOT be flagged StuckLoop")
	}
	if l.RepeatedFailure {
		t.Fatal("memory retry chain must NOT be flagged RepeatedFailure")
	}
	if l.RepeatedCall {
		t.Fatal("memory retry chain must NOT be flagged RepeatedCall")
	}
	if err := l.CanContinue(32); err != nil {
		t.Fatalf("CanContinue should pass for memory retry chain, got: %v", err)
	}

	// Sanity: a NON-memory tool repeating 3x identical still tripped.
	msgs2 := []oaiMsg{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_a", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"probe"}`}}}},
		{Role: "tool", ToolCallID: "call_a", Content: `{"output":"x"}`},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_b", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"probe"}`}}}},
		{Role: "tool", ToolCallID: "call_b", Content: `{"output":"x"}`},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_c", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"probe"}`}}}},
		{Role: "tool", ToolCallID: "call_c", Content: `{"output":"x"}`},
	}
	if l2 := buildAgentLedger(msgs2); !l2.StuckLoop {
		t.Fatal("non-memory 3x identical call must STILL be StuckLoop")
	}
}
