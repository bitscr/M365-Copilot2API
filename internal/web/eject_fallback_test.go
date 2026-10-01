package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The execution-boundary eject must REFUSE the fabrication, but it must not end
// the turn with an error frame. Verified 2026-09-28: the caller is uid=0 and
// /root/projects/web-ssh is root:root 755, so "Permission denied" on that path
// was model-invented. Ejecting it was correct; returning 502 / an SSE error
// after the corrections failed is what produced "No reply ... even after
// retries" and killed the conversation. The turn must end with a truthful
// assistant message instead.

func TestEjectExhaustedHonestTextNeverClaimsExecution(t *testing.T) {
	for _, hasTools := range []bool{true, false} {
		txt := ejectExhaustedHonestText(hasTools)
		if txt == "" {
			t.Fatal("fallback must never be empty — an empty body is what killed the turn")
		}
		// It must not assert that anything ran on this machine.
		for _, bad := range []string{
			"我已执行", "已经执行", "执行成功", "已完成部署", "已重启", "命令已运行",
		} {
			if strings.Contains(txt, bad) {
				t.Errorf("hasTools=%v: fallback must not claim execution: found %q in\n%s", hasTools, bad, txt)
			}
		}
		// It must say plainly that no command ran.
		if !strings.Contains(txt, "没有运行") && !strings.Contains(txt, "没有执行") {
			t.Errorf("hasTools=%v: fallback must state that nothing ran:\n%s", hasTools, txt)
		}
		// It must distinguish the model's cloud container from local reality.
		if !strings.Contains(txt, "容器") {
			t.Errorf("hasTools=%v: fallback must name the container fabrication:\n%s", hasTools, txt)
		}
	}
}

func TestEjectExhaustedHonestTextOffersAPathForward(t *testing.T) {
	withTools := ejectExhaustedHonestText(true)
	if !strings.Contains(withTools, "重试") && !strings.Contains(withTools, "命令") {
		t.Errorf("tools branch must offer a next step:\n%s", withTools)
	}
	noTools := ejectExhaustedHonestText(false)
	if !strings.Contains(noTools, "没有附加任何执行工具") {
		t.Errorf("no-tools branch must say no execution tool was attached:\n%s", noTools)
	}
}

// The fabricated errnos the eject exists to catch must never be echoed back as
// if they were local ground truth.
func TestEjectFallbackNeverEchoesFabricatedErrno(t *testing.T) {
	txt := ejectExhaustedHonestText(true) + ejectExhaustedHonestText(false)
	for _, bad := range []string{"/mnt/data", "uid=1000", "node v24", "Permission denied"} {
		if strings.Contains(txt, bad) {
			t.Errorf("fallback must not echo fabricated %q", bad)
		}
	}
}

// Wire-format proof: the non-stream fallback must be a valid 200 chat.completion
// carrying assistant content (not a 502), and the stream fallback must emit a
// content delta + finish stop + [DONE] (not an SSE error frame). These are what
// the client needs in order NOT to show "No reply".
func TestEjectFallbackWireFormats(t *testing.T) {
	t.Run("non-stream is a 200 completion with content", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeEjectFallbackCompletion(rec, "chatcmpl-x", "m365-copilot", ejectExhaustedHonestText(true))
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body must be valid JSON: %v", err)
		}
		if _, isErr := body["error"]; isErr {
			t.Fatal("fallback must not be an error envelope")
		}
		choices, _ := body["choices"].([]any)
		if len(choices) == 0 {
			t.Fatal("need choices")
		}
		first, _ := choices[0].(map[string]any)
		msg, _ := first["message"].(map[string]any)
		if msg["role"] != "assistant" {
			t.Fatalf("role must be assistant, got %v", msg["role"])
		}
		if c, _ := msg["content"].(string); strings.TrimSpace(c) == "" {
			t.Fatal("content must be non-empty or the client shows 'No reply'")
		}
	})

	t.Run("stream is content delta then finish then DONE", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeEjectFallbackStream(context.Background(), rec, http.Flusher(rec), "chatcmpl-y", "m365-copilot", ejectExhaustedHonestText(true))
		out := rec.Body.String()
		if strings.Contains(out, `"error"`) {
			t.Fatal("stream fallback must not emit an SSE error frame")
		}
		if !strings.Contains(out, `"content"`) {
			t.Fatal("stream fallback must emit a content delta")
		}
		if !strings.Contains(out, `"finish_reason":"stop"`) {
			t.Fatal("stream fallback must close with finish_reason=stop")
		}
		if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
			t.Fatalf("stream must end with [DONE], got tail: %q", out[max(0, len(out)-60):])
		}
	})
}
