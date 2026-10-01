package web

import (
	"context"
	"net/http"
	"time"
)

// Eject exhaustion must end the turn with an HONEST answer, not an error frame.
//
// The execution-boundary eject exists to stop the gateway relaying fabricated
// container output ("current dir /mnt/data", "node v24.16.0", "uid=1000(oai)",
// "Permission denied on a path the caller can actually read") as if it were real
// local ground truth. That defense is correct — verified 2026-09-28 against the
// real host: caller is uid=0 and /root/projects/web-ssh is root:root 755, so a
// successful `ls` could never return EACCES. The fabricated errno is the
// model's, not the tool's.
//
// But on non-convergence the eject returned an SSE error frame or a 502. The
// client has no other content to show, retries, and the conversation dies with
// "No reply: ... didn't produce a reply this time, even after retries" (observed
// twice on 2026-09-28, conversation 83a22010 and the earlier web-ssh thread).
//
// So: refuse to relay the fabrication, but still answer. These helpers emit a
// truthful assistant message that names exactly what happened and what did NOT
// happen, giving the turn a real ending the client can display.

// ejectExhaustedHonestText builds the fallback. It must never imply that any
// command ran, and must never present model output as local fact.
func ejectExhaustedHonestText(hasTools bool) string {
	if hasTools {
		return "我没能执行这一步。上游模型没有返回工具调用，而是声称它已经在自己的云容器里执行了命令——那是它那个环境的输出，不是本机的真实结果，所以我没有把它当作执行结果转述给你。\n\n" +
			"本轮没有运行任何命令，也没有修改、构建或重启任何服务。要继续，可以：\n" +
			"1. 在目标机器上直接运行需要的命令，把输出贴回来；\n" +
			"2. 让我换个方式重试（拆小步骤，或先只做只读检查）。"
	}
	return "我无法执行命令：当前请求没有附加任何执行工具。上游模型却声称自己执行了并报告了容器输出——那不是本机的真实结果，本轮没有运行任何命令。\n\n" +
		"要继续，请提供工具，或在本地直接运行命令。"
}

// writeEjectFallbackStream ends a stream with the honest text as real content
// (role + content delta, finish stop, [DONE]) so the client receives an
// assistant message rather than a stream error.
func writeEjectFallbackStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id, model, text string) {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
	_ = sseRaw(ctx, w, flusher, "data: "+mustJSON(chunk)+"\n\n")
	finish := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	}
	_ = sseRaw(ctx, w, flusher, "data: "+mustJSON(finish)+"\n\n")
	_ = sseRaw(ctx, w, flusher, "data: [DONE]\n\n")
}

// writeEjectFallbackCompletion is the non-stream counterpart: a normal
// chat.completion carrying the honest text, status 200.
func writeEjectFallbackCompletion(w http.ResponseWriter, id, model, text string) {
	jsonOut(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}
