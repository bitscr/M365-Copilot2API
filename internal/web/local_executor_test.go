package web

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalExecutorDisabledByDefault(t *testing.T) {
	if got := NewLocalExecutorFromEnv(); got != nil {
		t.Fatalf("M365_LOCAL_EXEC unset: expected nil executor, got %+v", got)
	}
}

func TestLocalExecutorEnabledByEnv(t *testing.T) {
	t.Setenv("M365_LOCAL_EXEC", "1")
	ex := NewLocalExecutorFromEnv()
	if ex == nil {
		t.Fatal("M365_LOCAL_EXEC=1: expected executor, got nil")
	}
	if !ex.ShouldExec("git") {
		t.Fatal("git should be whitelisted by default")
	}
	if ex.ShouldExec("rm") {
		t.Fatal("rm must NOT be whitelisted by default")
	}
}

func TestLocalExecutorAllowedOverride(t *testing.T) {
	t.Setenv("M365_LOCAL_EXEC", "1")
	t.Setenv("M365_LOCAL_EXEC_ALLOWED", "git,ls")
	ex := NewLocalExecutorFromEnv()
	if ex == nil {
		t.Fatal("expected executor")
	}
	if !ex.ShouldExec("git") || !ex.ShouldExec("ls") {
		t.Fatal("git/ls should be allowed after override")
	}
	if ex.ShouldExec("node") {
		t.Fatal("node must NOT be allowed after override")
	}
}

func TestRunHappyPath(t *testing.T) {
	ex := newLocalExecutor(map[string]string{})
	res := ex.Run(context.Background(), "sh", map[string]any{"command": "echo hello"}, "")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("stdout=%q want %q", res.Stdout, "hello")
	}
}

func TestRunNotFound(t *testing.T) {
	ex := newLocalExecutor(map[string]string{})
	res := ex.Run(context.Background(), "git", map[string]any{"command": "git --version"}, "")
	if res.Error != "" {
		t.Fatalf("unexpected error: %v", res.Error)
	}
}

func TestRunTimeout(t *testing.T) {
	ex := &LocalExecutor{
		allowed: defaultLocalAllowed,
		timeout: 300 * time.Millisecond,
		maxOut:  1 << 20,
	}
	res := ex.Run(context.Background(), "sh", map[string]any{"command": "sleep 5"}, "")
	if !res.TimedOut {
		t.Fatalf("expected timeout, got exit=%d err=%s", res.ExitCode, res.Error)
	}
}

func TestRunOutputTruncation(t *testing.T) {
	ex := &LocalExecutor{
		allowed: defaultLocalAllowed,
		timeout: 5 * time.Second,
		maxOut:  100,
	}
	res := ex.Run(context.Background(), "sh", map[string]any{"command": "head -c 100000 /dev/zero | tr '\\0' 'x'"}, "")
	if !strings.Contains(res.Stdout, "truncated") {
		t.Fatalf("expected truncation marker in stdout (len=%d)", len(res.Stdout))
	}
	if len(res.Stdout) > 250 {
		t.Fatalf("stdout too large after truncation: %d", len(res.Stdout))
	}
}

func TestAllLocal(t *testing.T) {
	ex := newLocalExecutor(map[string]string{})
	calls := []detectedToolCall{
		{Name: "git"}, {Name: "curl"},
	}
	if !ex.allLocal(calls) {
		t.Fatal("git+curl should be all-local")
	}
	calls = append(calls, detectedToolCall{Name: "rm"})
	if ex.allLocal(calls) {
		t.Fatal("batch containing rm must NOT be all-local")
	}
}

func TestParseLocalArgs(t *testing.T) {
	cmd, wd, dur, ok := parseLocalArgs(map[string]any{
		"command": "git clone x", "workdir": "/tmp", "timeout": 5.0,
	})
	if !ok || cmd != "git clone x" || wd != "/tmp" || dur != 5*time.Second {
		t.Fatalf("got cmd=%q wd=%q dur=%v ok=%v", cmd, wd, dur, ok)
	}
	// alias fields
	cmd, wd, _, ok = parseLocalArgs(map[string]any{"cmd": "ls", "cwd": "/root"})
	if !ok || cmd != "ls" || wd != "/root" {
		t.Fatalf("alias parse failed: cmd=%q wd=%q ok=%v", cmd, wd, ok)
	}
	// string body
	cmd, _, _, ok = parseLocalArgs("pwd")
	if !ok || cmd != "pwd" {
		t.Fatalf("string body parse failed: %q ok=%v", cmd, ok)
	}
}
