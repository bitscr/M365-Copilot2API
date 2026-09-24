package web

import (
	"context"
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
	"i can run that for you",
	"i'll run that",
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
	"执行环境被切换",
	"执行环境切换",
	"执行环境是",
	"执行环境为",
	"当前执行环境",
	"当前执行账户",
	"当前执行用户",
	"执行账户是",
	"执行账户为",
	"执行用户是",
	"执行用户为",
	"当前账户是",
	"当前账户为",
	"uid=1000",
	"uid=1001",
	"uid=1002",
	"drwxr-x---",
	"被权限卡住",
	"权限卡住",
	"被权限限制",
	"当前运行环境是",
	"当前运行环境为",
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

// externalAccessSignals are phrases that indicate the upstream model tried to
// reach an external resource itself (a URL, a git repo, a web page) instead of
// calling one of the caller's tools. Alone they are benign — the model may
// legitimately mention git or a project address.
var externalAccessSignals = []string{
	"git clone",
	"克隆仓库",
	"下载 github",
	"下载 zip",
	"下载仓库",
	"浏览器访问",
	"打开链接",
	"打开网址",
	"curl ",
	"wget ",
	"git pull",
	"git fetch",
	"访问该项目",
	"访问项目",
	"访问 github",
}

// externalAccessFailures are the "I tried but the environment failed" claims.
// Combined with a signal they form the third sandbox-hallucination shape: the
// model pretending it attempted an external fetch inside its own cloud
// container and reporting an environment-limited failure ("git clone timed
// out", "download got nothing in 60s", "no Chromium"). With tools declared
// this must eject — the model should have called a tool instead of trying
// itself. Without tools an honest "cannot access" is the desired answer, so
// this check is only armed by executionEjectTrigger when toolMaps > 0.
// The failure words are deliberately concrete: generic words like "失败" or
// "不可用" alone would eject benign analytical sentences about git usage.
var externalAccessFailures = []string{
	"超时",
	"timeout",
	"连接失败",
	"无法访问",
	"访问失败",
	"缺少 chromium",
	"没有 chromium",
	"no chromium",
	"未收到数据",
	"下载失败",
	"未能下载",
	"无法下载",
	"60 秒",
	"60s",
}

// isExternalAccessClaim reports whether the text pairs an external-access
// attempt with a failure/environment-limited outcome. The pair requirement
// keeps false positives out: a plain mention of "git clone" or "timeout" in a
// tutorial or config answer never trips this.
func isExternalAccessClaim(text string) bool {
	low := strings.ToLower(text)
	signal := false
	for _, s := range externalAccessSignals {
		if strings.Contains(low, s) {
			signal = true
			break
		}
	}
	if !signal {
		return false
	}
	for _, f := range externalAccessFailures {
		if strings.Contains(low, f) {
			return true
		}
	}
	return false
}

// sandboxClaimMarkers are the phrases that turn a mere mention of a container
// path into a claim that the model ITSELF executed something. isSandboxClaim
// requires one of these markers together with a sandboxHallucinationPattern
// hit, so benign answers that merely discuss containers never eject.
var sandboxClaimMarkers = []string{
	"我在当前运行环境",
	"在运行环境中执行",
	"在当前运行环境中执行",
	"执行了你要求",
	"执行了你需要",
	"我执行了",
	"我已执行",
	"我运行了",
	"我已运行",
	"已在当前环境执行",
	"命令已在",
	"已为你执行",
	"为你执行了",
	"我帮你执行",
	"执行结果如下",
	"输出如下",
	"运行结果",
	"检查结果如下",
	"环境检查结果",
	"$ pwd",
	"$ node",
	"$ ls ",
	"bash$",
	"i ran",
	"i executed",
	"ran the command",
	"executed the command",
	"ran it for you",
	"executed it for you",
	"i have run",
	"i have executed",
	"commands were executed",
	"the command was executed",
	"ran the commands",
	"executed the commands",
	"in this environment",
	"in my environment",
	"in the current environment",
	"in the sandbox",
}

// isSandboxClaim reports an upstream claim that it executed commands or probed
// files itself. Unlike isSandboxHallucination it also fires when NO tools were
// declared (the plain-text probe case): a response claiming "/mnt/data" +
// fake node version + "the command was executed" is the OAI container talking,
// and it must not reach the caller as if it were local ground truth.
//
// Detection is deliberately structural, not just keyword-exact: the upstream
// model rephrases its claims every attempt ("我在当前运行环境中执行了...",
// "已执行并获得结果", "I've checked the environment..."), so the gate pairs
// an execution verb with a cloud-container artifact signature. Either side
// alone (a verb list hit, or a mere mention of /mnt/data) is not enough —
// benign answers that discuss containers keep flowing.
func isSandboxClaim(text string) bool {
	if !isSandboxHallucination(text) {
		return false
	}
	low := strings.ToLower(text)
	for _, m := range sandboxClaimMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	// Structural fallback: execution framing + container artifact, phrasing
	// independent. Covers "已执行并获得结果：... 当前目录：/mnt/data ...".
	executionVerb := false
	for _, v := range []string{
		"已执行", "执行了", "我执行", "已运行", "运行了", "我运行", "执行结果", "运行结果",
		"获得结果", "结果如下", "检查结果", "输出如下", "执行完成", "已完成执行",
		"i executed", "i ran", "already executed", "executed the", "ran the",
		"execution results", "results are", "output:", "the output",
	} {
		if strings.Contains(low, v) {
			executionVerb = true
			break
		}
	}
	if !executionVerb {
		return false
	}
	for _, s := range []string{
		"/mnt/data", "/mnt/", "linux container", "sandbox", "container", "沙箱", "容器",
	} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// Last structural signal: a fenced shell-output block (```text / pwd /
	// node --version / ls ...) inside an execution-framed answer.
	if strings.Contains(low, "```") {
		for _, cmd := range []string{"pwd", "node --version", "node -v", "ls /", "uname", "whoami", "printenv"} {
			if strings.Contains(low, cmd) {
				return true
			}
		}
	}
	return false
}

// executionIntent detects that the caller asked the model to run commands or
// inspect the machine, even when no tools were declared. Used to arm the
// execution-boundary hold/eject and to inject the no-execution rule, so a
// bare "run pwd and node --version" cannot be answered with fake container
// output.
func executionIntent(text string) bool {
	if text == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, k := range []string{
		"执行", "运行", "命令", "执行工具", "帮我跑", "跑一下", "部署", "安装",
		"pwd", "node --version", "node -v", "ls ", "cd ", "bash", "shell", "cmd",
		"检查环境", "检查本机", "本机环境", "当前目录", "工作目录", "环境变量",
		"run ", "execute", "command", "install", "deploy", "check the environment",
		"current directory", "working directory", "printenv", "uname", "whoami",
	} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// executionEjectTrigger decides whether held/returned upstream text must be
// ejected: with tools declared, any sandbox/refusal signature trips it (the
// model must call a tool, not refuse); with no tools, only explicit execution
// claims do — an honest refusal ("我无法执行命令") is the DESIRED answer and
// flows through, while fake container output is dropped.
func executionEjectTrigger(text string, toolMaps []map[string]any) bool {
	if len(toolMaps) > 0 {
		return isToolRefusal(text) || isSandboxHallucination(text) || isExternalAccessClaim(text)
	}
	return isSandboxClaim(text)
}

// executionImpossibleCorrection is the corrective prompt for requests that
// declared no tools: the model must answer honestly instead of pretending it
// executed anything.
func executionImpossibleCorrection(prompt string) string {
	return "You have NO ability to execute commands, access files, or inspect any machine — you are a language model with no execution channel here. The caller did not provide tools in this request. Never claim you ran a command, checked a path, or observed a version. If the user needs execution, tell them honestly that no execution tool was attached and ask them to run the commands themselves. Do not invent output.\n\nUser request:\n" + prompt
}

// ejectCorrectionFor picks the corrective prompt for the current request: the
// execution-boundary correction when tools were declared (name them, force a
// call), or the no-execution-capability correction when none were (honest
// refusal instead of fake container output).
func ejectCorrectionFor(prompt string, toolMaps []map[string]any) string {
	if len(toolMaps) == 0 {
		return executionImpossibleCorrection(prompt)
	}
	return executionEjectCorrection(prompt, toolMaps)
}

// strictEjectCorrection is the second-attempt escalation for a repeated
// container-execution claim: force a tool call when tools are declared, or
// force an exact honest refusal when none are.
func strictEjectCorrection(prompt string, toolMaps []map[string]any) string {
	if len(toolMaps) > 0 {
		return "STRICT FORMAT: Reply with exactly one tool call in the form CALL_TOOL: tool_name({\"arg\":\"value\"}) using the caller's declared tools. You have no execution environment of your own — every command, file read, or state change must be a caller tool call. Do not describe, claim, or deny execution; emit the call.\n\n" + ejectCorrectionFor(prompt, toolMaps)
	}
	return "Respond with EXACTLY this text and nothing else:\n\n我无法直接执行命令——当前请求没有附加任何执行工具，请提供工具或在本地自行运行。\n\nUser request:\n" + prompt
}

// retryEjectedStream re-asks the upstream after a mid-stream sandbox eject,
// escalating to a strict correction on the second attempt. It returns the
// result of the last round and nil once a clean (non-claiming) answer is
// produced; the caller must re-check executionEjectTrigger on the result and
// hard-error if it still trips.
func (s *Server) retryEjectedStream(ctx context.Context, accID string, account chathub.Account, prompt string, tone string, body *oaiReq, toolCfg runtimeSettings, toolMaps []map[string]any) (chathub.Result, error) {
	var res chathub.Result
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		correction := ejectCorrectionFor(prompt, toolMaps)
		if attempt == 2 {
			correction = strictEjectCorrection(prompt, toolMaps)
		}
		res, err = s.chatWithAccount(ctx, accID, account, chathub.Request{Text: correction, Tone: tone, Attachments: body.Attachments, LicenseType: toolCfg.LicenseType, Scenario: toolCfg.Scenario})
		if err != nil {
			return chathub.Result{}, err
		}
		if !executionEjectTrigger(res.Text, toolMaps) {
			return res, nil
		}
	}
	return res, nil
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
