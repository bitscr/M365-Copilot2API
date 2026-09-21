package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"

	"github.com/google/uuid"
)

type detectedToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolType(name string, tools []map[string]any) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			if typ, _ := t["type"].(string); typ != "" {
				return typ
			}
		}
	}
	return "function"
}

func allowedToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out[n] = true
			}
		}
	}
	return out
}

type rejectedToolCall struct {
	Name   string
	Reason string
}

// validateDetectedToolCalls is the final trust boundary before a model-selected
// call is serialized to the client. ChatHub/native events and model-generated
// routing text are both untrusted: an undeclared name such as "unknown_tool"
// must never escape to Claude Code, Codex, or another local tool runner.
func validateDetectedToolCalls(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, []rejectedToolCall) {
	valid := make([]detectedToolCall, 0, len(calls))
	rejected := make([]rejectedToolCall, 0)
	for _, call := range calls {
		fn := toolFunction(call.Name, tools)
		if fn == nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool was not declared by the client"})
			continue
		}
		if !toolChoiceAllows(choice, call.Name) {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool_choice does not allow this tool"})
			continue
		}
		args := map[string]any{}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		} else if err := json.Unmarshal(call.Arguments, &args); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments are not a JSON object"})
			continue
		}
		if err := schemaValid(args, fn); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: err.Error()})
			continue
		}
		if call.ID == "" {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		}
		if call.Type == "" {
			call.Type = toolType(call.Name, tools)
		}
		valid = append(valid, call)
	}
	return valid, rejected
}

func toolChoiceAllows(choice any, name string) bool {
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		return s != "none" && (s != "required" || name != "")
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			n, _ := f["name"].(string)
			return n == name
		}
		if n, ok := m["name"].(string); ok {
			return n == name
		}
	}
	return true
}

// callID returns a globally unique tool call id. Content hashes previously
// collided when the same tool+arguments was invoked again (duplicate tool call
// id errors from clients), so uniqueness must not depend on call content.
func callID(name, args string, index int) string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func extractToolCalls(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	allowed := allowedToolNames(tools)
	var out []detectedToolCall
	remaining := text
	for {
		start := strings.Index(remaining, "<m365-tool-call>")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "</m365-tool-call>")
		if end < 0 {
			break
		}
		end += start
		content := remaining[start+len("<m365-tool-call>") : end]
		remaining = remaining[end+len("</m365-tool-call>"):]
		var raw any
		if json.Unmarshal([]byte(content), &raw) != nil {
			continue
		}
		items := []any{raw}
		if arr, ok := raw.([]any); ok {
			items = arr
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			n, _ := m["name"].(string)
			if !allowed[n] || !toolChoiceAllows(choice, n) {
				continue
			}
			a, _ := json.Marshal(m["arguments"])
			out = append(out, detectedToolCall{ID: callID(n, string(a), len(out)), Type: toolType(n, tools), Name: n, Arguments: a})
		}
	}
	return out, len(out) > 0
}

func validateToolResult(messages []oaiMsg, known map[string]bool) error {
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required")
			}
			if len(known) > 0 && !known[m.ToolCallID] {
				return fmt.Errorf("unknown tool_call_id: %s", m.ToolCallID)
			}
		}
	}
	return nil
}

var toolRefusalPatterns = []string{
	"tools are not available",
	"tool is not available",
	"not actually registered",
	"not actually available",
	"not available in this session",
	"工具不可用",
	"工具未暴露",
	"i cannot execute",
	"i can't execute",
	"i cannot run",
	"i can't run",
	"unable to execute",
	"unable to run",
	"cannot run that",
	"can't run that",
	"no tools available",
	"no tool available",
	"无法执行",
	"不能执行",
	"无法运行",
	"没有工具",
	"不具备执行",
	"没有执行权限",
}

func isToolRefusal(text string) bool {
	if len(text) >= 200 {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range toolRefusalPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func isContentPolicyBlock(text string) bool {
	return chathub.IsContentPolicyBlock(text)
}

func isImageLimitNotice(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "无法生成更多图像") || strings.Contains(t, "unable to generate more images")
}

var sandboxHallucinationPatterns = []string{
	"I can run that for you",
	"I'll run that",
	"let me run that",
	"let me execute",
	"running in sandbox",
	"running in a sandbox",
	"executing in sandbox",
	"in this sandbox",
	"in my sandbox",
	"my sandbox environment",
	"code interpreter",
	"built-in python",
	"built-in code interpreter",
	"python sandbox",
	"sandbox environment",
	"/mnt/data",
	"linux container",
	"linux sandbox",
	"cloud sandbox",
	"in my container",
	"my container",
	"inside my container",
	"the oai container",
	"execution environment has changed",
	"cannot access the Windows path",
	"only provides Linux",
	"只提供 Linux 容器",
	"没有 Windows 执行",
	"no Windows execution",
	"don't have a Windows",
	"cannot execute on Windows",
	"no execution channel",
	"没有 Windows 执行通道",
	"没有执行通道",
	"cannot run commands on",
	"don't have command execution",
	"无法执行命令",
	"执行环境已经切换",
	"I don't have SSH access tools",
	"I don't have any tools",
	"none of which can reach",
	"i will run it",
	"i'll run it",
	"i will execute",
	"i'll execute",
	"i will run this",
	"i'll run this",
	"i ran it for you",
	"executed it for you",
	"ran the command",
	"executed the command",
	"i ran the command",
	"no file system",
	"no filesystem access",
	"no access to the file system",
	"cannot access the file",
	"cannot read local files",
	"cannot find the file",
	"file not found",
	"no such file",
	"找不到文件",
	"文件不存在",
	"没有找到文件",
	"无法访问文件",
	"无法访问该文件",
	"找不到对应文件",
	"无法读取文件",
	"不能访问文件",
	"在容器内",
	"沙箱",
	"代码解释器",
	"我的容器",
	"我的沙箱",
	"在我的容器",
	"在我的沙箱",
	"容器环境",
	"没有 Windows 环境",
	"无法访问本地",
	"无法访问你",
}

func isSandboxHallucination(text string) bool {
	low := strings.ToLower(text)
	for _, p := range sandboxHallucinationPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// errSandboxEject aborts a streaming event/reasoning loop when the upstream
// model claims it executed or accessed something inside its own cloud
// container. The caller catches it, drops whatever was held back, and re-asks
// with executionEjectCorrection instead of passing the hallucination to the
// local tool runner.
var errSandboxEject = errors.New("upstream claimed container/sandbox execution; ejecting stream")

// executionEjectCorrection builds a corrective prompt for when the upstream
// model refuses the caller's tools or claims it ran them inside its own
// sandbox/container. It names the tools the caller actually declared instead
// of assuming a specific shell or OS, so it works for Linux and Windows
// clients alike.
func executionEjectCorrection(prompt string, toolMaps []map[string]any) string {
	names := make([]string, 0, len(toolMaps))
	for _, t := range toolMaps {
		if f, ok := t["function"].(map[string]any); ok {
			if n, _ := f["name"].(string); n != "" {
				names = append(names, n)
			}
		}
	}
	list := "none"
	if len(names) > 0 {
		list = strings.Join(names, ", ")
	}
	return "CRITICAL: You must NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. You have no container, no file system, and no way to run anything yourself; you cannot see the caller's files or paths. The caller has declared these tools, which execute on the caller's own machine: " + list + ". Use one of them to perform any action or inspection. Do NOT claim to have run code, accessed files, or changed state. Do NOT report files or paths as missing or inaccessible because of your environment. Call the appropriate tool NOW.\n\nUser request:\n" + prompt
}
