package web

import (
	"strings"
	"testing"
)

// The 2026-09-28 "No reply" deadlock: conversation 83a22010 answered with the
// bare NO_TOOL_NEEDED stop token three times in a row (22:36:07, 22:36:26,
// 22:36:56), then a 4th attempt hit three empty completions -> 502. The client
// surfaced "No reply: gpt-5.6-reasoning didn't produce a reply this time, even
// after retries". The gateway now re-asks instead of dead-ending on empty.

// TestNoToolNeededSummaryCorrectionForbidsTheDeadEnds guards the corrective
// prompt: it must forbid the stop token, forbid tool calls, and forbid
// execution-environment narration, or the re-ask reproduces the same dead end.
func TestNoToolNeededSummaryCorrectionForbidsTheDeadEnds(t *testing.T) {
	c := noToolNeededSummaryCorrection("CONVERSATION-BODY")
	for _, want := range []string{
		"NO_TOOL_NEEDED",
		"Do not call any tool",
		"Do not describe an execution environment",
		"never an empty reply",
		"CONVERSATION-BODY",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("correction missing %q", want)
		}
	}
}

// TestIsBareNoToolNeededUnchanged pins the detector semantics so the new
// recovery path cannot widen what counts as the bare token.
func TestIsBareNoToolNeededUnchanged(t *testing.T) {
	bare := []string{"NO_TOOL_NEEDED", "  NO_TOOL_NEEDED  ", "no_tool_needed"}
	for _, s := range bare {
		if !isBareNoToolNeeded(s) {
			t.Errorf("should detect bare token: %q", s)
		}
	}
	real := []string{
		"NO_TOOL_NEEDED, 已完成部署并重启服务。",
		"总结：本轮未修改任何文件。",
		"",
	}
	for _, s := range real {
		if isBareNoToolNeeded(s) {
			t.Errorf("must not treat real answer as bare token: %q", s)
		}
	}
}
