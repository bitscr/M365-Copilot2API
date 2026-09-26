package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\n(.*?)\n```")

// parseCallToolInvocations extracts every "CALL_TOOL: name({...})" text-form
// tool call in the given text. Degraded clients / bare upstream decisions may
// relay a tool call as plain content instead of a structured tool_calls row;
// that text is the same logical intent and must be convertible. Arguments are
// decoded as one JSON value so nested braces and escaped strings survive.
func parseCallToolInvocations(text string) []map[string]any {
	var out []map[string]any
	lower := strings.ToLower(text)
	scan := 0
	for {
		rel := strings.Index(lower[scan:], "call_tool:")
		if rel < 0 {
			return out
		}
		start := scan + rel + len("call_tool:")
		seg := strings.TrimSpace(text[start:])
		lp := strings.Index(seg, "(")
		if lp <= 0 {
			scan = start + 1
			continue
		}
		name := strings.TrimSpace(seg[:lp])
		if name == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(name) {
			scan = start + 1
			continue
		}
		body := strings.TrimSpace(seg[lp+1:])
		dec := json.NewDecoder(strings.NewReader(body))
		var args any
		if err := dec.Decode(&args); err != nil {
			scan = start + 1
			continue
		}
		after := strings.TrimSpace(body[dec.InputOffset():])
		if !strings.HasPrefix(after, ")") {
			scan = start + 1
			continue
		}
		// Keep the raw argument text verbatim (decoder already validated it);
		// re-marshaling would HTML-escape & -> \u0026 and could reorder keys.
		raw := body[:dec.InputOffset()]
		out = append(out, map[string]any{
			"function": map[string]any{"name": name, "arguments": raw},
		})
		scan = start
	}
}

// declaredShell returns the shell-ish tool name the client actually
// declared (bash/sh/shell/powershell/cmd), or "" if none. Forcing an
// undeclared bash call on clients that don't support it (issue #12) makes
// them error out and loop, so conversion only happens for declared tools.
func declaredShell(allowed map[string]bool) string {
	for _, n := range []string{"bash", "sh", "shell", "powershell", "cmd"} {
		if allowed[n] {
			return n
		}
	}
	return ""
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		_ = json.Unmarshal([]byte(args), &v)
		// Auto-convert bash/shell code blocks to tool calls, but only when
		// the client declared the tool.
		if name == "bash" || name == "sh" || name == "shell" || name == "powershell" || name == "cmd" {
			converted := name
			if !allowed[name] {
				if shell == "" {
					continue
				}
				converted = shell
			}
			if m, ok := v.(map[string]any); ok {
				if cmd, hasCmd := m["command"]; hasCmd && cmd != "" {
					cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if v == nil {
				cmdBytes, _ := json.Marshal(map[string]any{"command": args})
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			continue
		}
		if !allowed[name] || !toolChoiceAllows(choice, name) {
			continue
		}
		if v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: toolType(name, tools), Name: name, Arguments: b})
	}
	// Also check for plain JSON objects with a "command" field (not in fenced blocks)
	if len(out) == 0 && shell != "" {
		for i := 0; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			end := strings.Index(text[i:], "\n")
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			braceEnd := strings.LastIndex(line, "}")
			if braceEnd < 0 {
				continue
			}
			if !strings.Contains(line[:braceEnd+1], `"command"`) {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[:braceEnd+1]), &obj) != nil {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	if len(out) == 0 {
		for _, raw := range parseCallToolInvocations(text) {
			fn, _ := raw["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			if name == "" || !allowed[name] || !toolChoiceAllows(choice, name) {
				continue
			}
			if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			out = append(out, detectedToolCall{ID: callID(name, args, len(out)), Type: toolType(name, tools), Name: name, Arguments: json.RawMessage(args)})
		}
	}
	return out
}
