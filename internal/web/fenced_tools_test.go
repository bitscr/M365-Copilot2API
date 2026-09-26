package web

import "testing"

func TestFencedWorkspaceShellIsStructuredToolCall(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "workspace_shell"}}}
	calls := fencedToolCalls("```workspace_shell\n{\"command\":\"find /workspace -type f -o -type d | sort\"}\n```", tools, "auto")
	if len(calls) != 1 {
		t.Fatalf("expected one structured tool call, got %d", len(calls))
	}
	if calls[0].Name != "workspace_shell" {
		t.Fatalf("unexpected tool name %q", calls[0].Name)
	}
	if string(calls[0].Arguments) != `{"command":"find /workspace -type f -o -type d | sort"}` {
		t.Fatalf("unexpected arguments: %s", calls[0].Arguments)
	}
}

func TestFencedBashNotConvertedWhenUndeclared(t *testing.T) {
	// Issue #12: no bash tool declared -> code blocks must stay as text.
	calls := fencedToolCalls("```powershell\nGet-Process\n```", nil, "auto")
	if len(calls) != 0 {
		t.Fatalf("undeclared shell conversion must not happen, got %d calls", len(calls))
	}
}

func TestFencedBashConvertedWhenDeclared(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	calls := fencedToolCalls("```bash\nls -la\n```", tools, "auto")
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("declared bash should convert, got %+v", calls)
	}
	if string(calls[0].Arguments) != `{"command":"ls -la"}` {
		t.Fatalf("unexpected arguments: %s", calls[0].Arguments)
	}
}

func TestCallToolTextParsedWhenDeclared(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "terminal"}}}
	calls := fencedToolCalls(`CALL_TOOL: terminal({"command":"node probe_ttyd.js","timeout":30})`, tools, "auto")
	if len(calls) != 1 || calls[0].Name != "terminal" {
		t.Fatalf("declared text CALL_TOOL should convert, got %+v", calls)
	}
	if string(calls[0].Arguments) != `{"command":"node probe_ttyd.js","timeout":30}` {
		t.Fatalf("unexpected arguments: %s", calls[0].Arguments)
	}
}

func TestCallToolTextIgnoredWhenUndeclared(t *testing.T) {
	// Issue #12 analog: an undeclared tool in text form must not become a call.
	calls := fencedToolCalls(`CALL_TOOL: terminal({"command":"id"})`, nil, "auto")
	if len(calls) != 0 {
		t.Fatalf("undeclared text CALL_TOOL must be ignored, got %d calls", len(calls))
	}
}

func TestCallToolTextMultipleInvocations(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "terminal"}},
		{"type": "function", "function": map[string]any{"name": "skill_view"}},
	}
	text := "First do this.\nCALL_TOOL: terminal({\"command\":\"id\"})\nThen check that.\nCALL_TOOL: skill_view({\"name\":\"m365-panel\"})"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 2 {
		t.Fatalf("expected 2 text CALL_TOOL invocations, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "terminal" || calls[1].Name != "skill_view" {
		t.Fatalf("unexpected call names: %q %q", calls[0].Name, calls[1].Name)
	}
}

func TestCallToolTextNestedBraceArgs(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "patch"}}}
	args := `{"mode":"replace","new_string":"ws.send(Buffer.concat([Buffer.from('0'), Buffer.from('bash ~/toor.sh\\r')]));","old_string":"cd /home/container && bash toor.sh"}`
	text := "CALL_TOOL: patch(" + args + ")"
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 || calls[0].Name != "patch" {
		t.Fatalf("nested-brace CALL_TOOL should parse, got %+v", calls)
	}
	if string(calls[0].Arguments) != args {
		t.Fatalf("arguments mismatch:\n got: %s\nwant: %s", calls[0].Arguments, args)
	}
}
