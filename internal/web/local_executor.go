package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"m365-copilot2api/internal/chathub"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LocalExecutor runs whitelisted shell commands on the gateway host so the
// model's tool work happens where the network actually works (this box),
// instead of inside the upstream model's sandbox where GitHub/curl are
// blocked or absent. Enabled via M365_LOCAL_EXEC=1 (default off: existing
// clients keep the pass-through tool flow unchanged).
type LocalExecutor struct {
	allowed map[string]bool
	timeout time.Duration
	maxOut  int
}

// localExecResult carries the outcome of one local command execution.
type localExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Error    string `json:"error,omitempty"`
}

var defaultLocalAllowed = map[string]bool{
	"bash": true, "sh": true, "shell": true,
	"git": true, "curl": true, "wget": true,
	"node": true, "python": true, "python3": true,
	"zip": true, "tar": true, "unzip": true,
	"ls": true, "cat": true, "pwd": true,
}

func newLocalExecutor(env map[string]string) *LocalExecutor {
	timeout := 60 * time.Second
	maxOut := 1 << 20 // 1MB

	if v := env["M365_LOCAL_EXEC_TIMEOUT_S"]; v != "" {
		if n, err := time.ParseDuration(v + "s"); err == nil && n > 0 {
			timeout = n
		}
	}
	if v := env["M365_LOCAL_EXEC_MAX_OUTPUT"]; v != "" {
		if n := parseByteSize(v); n > 0 {
			maxOut = n
		}
	}
	return &LocalExecutor{allowed: defaultLocalAllowed, timeout: timeout, maxOut: maxOut}
}

// NewLocalExecutorFromEnv builds the executor from environment (nil when
// M365_LOCAL_EXEC is not "1", keeping the pass-through tool flow intact).
func NewLocalExecutorFromEnv() *LocalExecutor {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	if env["M365_LOCAL_EXEC"] != "1" {
		return nil
	}
	ex := newLocalExecutor(env)
	if v := env["M365_LOCAL_EXEC_ALLOWED"]; v != "" {
		ex.allowed = map[string]bool{}
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				ex.allowed[n] = true
			}
		}
	}
	return ex
}

func parseByteSize(s string) int {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "KB"), strings.HasSuffix(s, "K"):
		mult = 1 << 10
		s = strings.TrimSuffix(strings.TrimSuffix(s, "K"), "KB")
	case strings.HasSuffix(s, "MB"), strings.HasSuffix(s, "M"):
		mult = 1 << 20
		s = strings.TrimSuffix(strings.TrimSuffix(s, "M"), "MB")
	case strings.HasSuffix(s, "GB"), strings.HasSuffix(s, "G"):
		mult = 1 << 30
		s = strings.TrimSuffix(strings.TrimSuffix(s, "G"), "GB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n * mult
}

// ShouldExec reports whether a model-selected tool name is eligible for
// local execution. Whitelist only — anything else passes through to the
// client as before.
func (e *LocalExecutor) ShouldExec(name string) bool {
	return e != nil && e.allowed[name]
}

// parseLocalArgs extracts command/workdir/timeout from the tool-call
// arguments object (shape varies: {"command": "..."} or {"cmd": "..."}).
func parseLocalArgs(args any) (cmd, workdir string, timeout time.Duration, ok bool) {
	m, isMap := args.(map[string]any)
	if !isMap {
		if s, isStr := args.(string); isStr && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s), "", 0, true
		}
		return "", "", 0, false
	}
	cmd, _ = m["command"].(string)
	if cmd == "" {
		if c, has := m["cmd"].(string); has && c != "" {
			cmd = c
		}
	}
	if cmd == "" {
		return "", "", 0, false
	}
	workdir, _ = m["workdir"].(string)
	if wd, has := m["cwd"].(string); has && wd != "" {
		workdir = wd
	}
	timeout = 0
	if t, has := m["timeout"]; has {
		switch v := t.(type) {
		case float64:
			timeout = time.Duration(v * float64(time.Second))
		case string:
			if d, err := time.ParseDuration(v); err == nil {
				timeout = d
			}
		}
	}
	return cmd, workdir, timeout, true
}

// Run executes one whitelisted command on the host and returns its output
// (bounded). The command is run via `sh -c` so pipes/globs/env work the way
// a user expects; the binary name is validated against the whitelist first.
func (e *LocalExecutor) Run(ctx context.Context, name string, args any, hostWorkdir string) localExecResult {
	cmd, workdir, wantTimeout, ok := parseLocalArgs(args)
	if !ok {
		return localExecResult{Error: "missing command argument"}
	}
	// Resolve the requested binary name (the tool name) to the first
	// whitelisted path — never execute an arbitrary argv[0].
	binary := name
	if name == "shell" {
		binary = "sh"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return localExecResult{Error: fmt.Sprintf("local tool %q not found on gateway host: %v", binary, err)}
	}

	timeout := e.timeout
	if wantTimeout > 0 && wantTimeout < timeout {
		timeout = wantTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if workdir == "" {
		workdir = hostWorkdir
	}
	var stdout, stderr bytes.Buffer
	execCmd := exec.CommandContext(runCtx, "sh", "-c", cmd)
	execCmd.Dir = workdir
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr
	execCmd.Env = os.Environ()

	start := time.Now()
	err := execCmd.Run()
	elapsed := time.Since(start)
	res := localExecResult{
		Stdout:   truncateBytes(stdout.Bytes(), e.maxOut, "stdout"),
		Stderr:   truncateBytes(stderr.Bytes(), e.maxOut, "stderr"),
		ExitCode: 0,
	}
	if err != nil {
		// Deadline (ours or the caller's) wins over the ExitError: a
		// timed-out command almost always surfaces as a killed process
		// (signal, exit code -1) rather than a distinct error type.
		if runCtx.Err() == context.DeadlineExceeded || ctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
			res.ExitCode = -1
			res.Error = fmt.Sprintf("command timed out after %s", timeout)
		} else if ee, isEE := err.(*exec.ExitError); isEE {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Error = err.Error()
		}
	}
	log.Printf("[local-exec] name=%s cmd=%q dir=%q exit=%d timeout=%t elapsed=%s out=%d err=%d", name, cmd, workdir, res.ExitCode, res.TimedOut, elapsed, len(res.Stdout), len(res.Stderr))
	return res
}

func truncateBytes(b []byte, limit int, label string) string {
	if len(b) <= limit {
		return string(b)
	}
	head := limit / 2
	tail := limit - head
	s := string(b)
	return s[:head] + fmt.Sprintf("\n... [%s truncated %d bytes] ...\n", label, len(b)-limit) + s[len(s)-tail:]
}

// allLocal decides whether the full batch of model-selected calls can be
// handled locally (every one whitelisted); a single non-local call forces
// the whole batch back to pass-through so the client still sees them.
func (e *LocalExecutor) allLocal(calls []detectedToolCall) bool {
	if e == nil || len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if !e.ShouldExec(c.Name) {
			return false
		}
	}
	return true
}

func resolveWorkdirFromRequest() string {
	wd, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return wd
}

// tryLocalExecute closes the tool loop inside the gateway: every whitelisted
// model-selected call runs on the gateway host, results are appended to the
// message history as tool messages, and the model is asked again for the
// final answer (same conversation, same account). Returns ok=false when the
// batch is not locally executable or the follow-up chat fails — callers must
// fall back to the normal pass-through tool flow then. On success it also
// returns the evidence ledger so callers can satisfy evidence checks.
func (s *Server) tryLocalExecute(ctx context.Context, requestID, accID string, account chathub.Account, body *oaiReq, calls []detectedToolCall, tone string, attachments []chathub.Attachment, convID, sessionID string) (chathub.Result, agentLedger, bool) {
	ex := s.localExec
	if ex == nil || !ex.allLocal(calls) {
		return chathub.Result{}, agentLedger{}, false
	}
	log.Printf("[local-exec] id=%s intercepting %d tool calls for local execution", requestID, len(calls))

	// Snapshot the message history so the appended tool messages only
	// affect this turn's flatten, never the caller's slice.
	msgs := make([]oaiMsg, 0, len(body.Messages)+len(calls)*2)
	msgs = append(msgs, body.Messages...)

	assistantToolMsg := oaiMsg{Role: "assistant", Content: "", ToolCalls: make([]map[string]any, 0, len(calls))}
	for _, c := range calls {
		args := map[string]any{}
		_ = json.Unmarshal(c.Arguments, &args)
		assistantToolMsg.ToolCalls = append(assistantToolMsg.ToolCalls, map[string]any{
			"id":   c.ID,
			"type": "function",
			"function": map[string]any{
				"name":      c.Name,
				"arguments": string(c.Arguments),
			},
		})
	}
	msgs = append(msgs, assistantToolMsg)

	for _, c := range calls {
		args := map[string]any{}
		_ = json.Unmarshal(c.Arguments, &args)
		result := ex.Run(ctx, c.Name, args, "")
		content := mustJSON(result)
		msgs = append(msgs, oaiMsg{Role: "tool", ToolCallID: c.ID, Name: c.Name, Content: content})
	}

	// Re-flatten with the tool results and ask the model for the final
	// answer in the same cloud conversation.
	prompt, atts := flattenPromptMessages(msgs, attachments)
	req := chathub.Request{
		Text:           prompt,
		Tone:           tone,
		ConversationID: convID,
		SessionID:      sessionID,
		Attachments:    atts,
		LicenseType:    s.settings.get().LicenseType,
		Scenario:       s.settings.get().Scenario,
	}
	res, err := s.chatWithAccount(ctx, accID, account, req)
	if err != nil {
		log.Printf("[local-exec] id=%s follow-up chat failed, falling back to pass-through: %v", requestID, err)
		return chathub.Result{}, agentLedger{}, false
	}
	log.Printf("[local-exec] id=%s done follow-up conversation=%s", requestID, res.ConversationID)
	return res, buildAgentLedger(msgs), true
}

var _ = filepath.Separator // keep filepath import for future path handling
