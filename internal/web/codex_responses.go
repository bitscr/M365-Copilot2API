package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// writeSSEHeaders sets the streaming headers. The explicit charset matters:
// SSE defaults to UTF-8 but clients that sniff Content-Type fall back to
// ISO-8859-1 without it, which is what turns non-ASCII deltas into mojibake
// for externally connected clients.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
}

// writeResponsesResult projects an internal OpenAI-style result into the
// Responses events and completion shape consumed by Codex.
func writeResponsesResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := firstNonEmpty(fmt.Sprint(src["m365_response_id"]), "resp_"+uuid.NewString())
	msg, _ := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	var output []any
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if tc["type"] == "custom" {
				output = append(output, map[string]any{"type": "custom_tool_call", "id": "ctc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "input": customToolInput(fn["arguments"]), "status": "completed"})
				continue
			}
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
		}
	} else {
		text, _ := msg["content"].(string)
		messageID := "msg_" + uuid.NewString()
		output = append(output, map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{outputTextBlock("txt_"+uuid.NewString(), text)}})
	}
	usage, _ := src["usage"].(map[string]any)
	usageSource, _ := src["m365_usage_source"].(string)
	if usage == nil {
		estimate := estimateResponsesUsage(model, nil, nil, nil, fmt.Sprint(msg["content"]))
		usage = estimate.Values
		usageSource = estimate.Source
	}
	if usageSource == "" {
		usageSource = usageSourceHeuristic
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage, "m365": localUsageMetadata(usageSource)}
	if !stream {
		jsonOut(w, resp)
		return
	}
	writeSSEHeaders(w)
	f, _ := w.(http.Flusher)
	ss := newResponsesSSEStream(nil, w, f, model)
	_ = ss.emitCreated(id, model, time.Now().Unix())
	for i, item := range output {
		m, _ := item.(map[string]any)
		addedItem := item
		if m["type"] == "function_call" {
			// Arguments arrive in function_call_arguments.delta. Including them
			// here too would make conforming clients append duplicate JSON.
			added := make(map[string]any, len(m))
			for k, v := range m {
				added[k] = v
			}
			added["arguments"] = ""
			added["status"] = "in_progress"
			addedItem = added
		}
		_ = ss.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": addedItem})
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			if len(content) > 0 {
				c, _ := content[0].(map[string]any)
				text := fmt.Sprint(c["text"])
				_ = ss.emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "item_id": m["id"], "delta": text})
				_ = ss.emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": i, "content_index": 0, "item_id": m["id"], "text": text})
			}
		} else if m["type"] == "function_call" {
			args, _ := m["arguments"].(string)
			_ = ss.emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": m["id"], "delta": args})
			_ = ss.emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": m["id"], "arguments": args})
		} else if m["type"] == "custom_tool_call" {
			input, _ := m["input"].(string)
			_ = ss.emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": m["id"], "delta": input})
			_ = ss.emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": m["id"], "input": input})
		}
		_ = ss.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	_ = ss.emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func customToolInput(arguments any) string {
	if s, ok := arguments.(string); ok {
		var v struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v.Input
		}
	}
	return ""
}
