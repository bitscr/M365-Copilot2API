package web

import (
	"m365-copilot2api/internal/chathub"
	"net/http"
	"time"
	"unicode/utf8"
)

func writeToolResponse(w http.ResponseWriter, id, model string, stream bool, sendUsage bool, calls []detectedToolCall, res chathub.Result) error {
	// Local-exec / no-tool final answer: no tool calls were emitted, so this
	// is a plain assistant text result (the gateway already ran the tools and
	// received the follow-up answer). Serialize it as a normal completion —
	// serializing it as a tool_calls response would drop res.Text entirely
	// (content: nil, finish_reason: "tool_calls").
	if len(calls) == 0 && res.Text != "" {
		msg := map[string]any{"role": "assistant", "content": sanitizePublicAssistantText(res.Text)}
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		pt := EstimateTokens(res.Text)
		ct := EstimateTokens(res.Text)
		usage := map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)
			created := time.Now().Unix()
			first := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}}}
			if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
				first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["reasoning_content"] = reasoning
			}
			_ = sseSafeRaw(w, flusher, "data: "+mustJSON(first)+"\n\n")
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": sanitizePublicAssistantText(res.Text)}, "finish_reason": nil}}}
			_ = sseSafeRaw(w, flusher, "data: "+mustJSON(chunk)+"\n\n")
			if sendUsage {
				usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}, "usage": usage}
				_ = sseSafeRaw(w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
			}
			finish := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
			_ = sseSafeRaw(w, flusher, "data: "+mustJSON(finish)+"\n\n")
			_ = sseSafeRaw(w, flusher, "data: [DONE]\n\n")
			return nil
		}
		jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}}, "m365": compatM365Metadata(res), "usage": usage})
		return nil
	}
	toolCalls := toolCallMaps(calls)
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
	if res.Reasoning != "" {
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
	}
	pt := EstimateTokens(res.Text)
	for _, tc := range calls {
		pt += EstimateTokens(string(tc.Arguments))
	}
	ct := EstimateTokens(res.Text)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		emit := func(v any) {
			if err := sseDataRaw(w, flusher, mustJSON(v)); err != nil {
				return
			}
		}
		base := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		firstDelta := map[string]any{"role": "assistant", "content": nil}
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			firstDelta["reasoning_content"] = reasoning
		}
		emit(base(firstDelta, nil))
		const chunkSize = 512
		for i, tc := range calls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			isLast := i == len(calls)-1
			emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": ""}}}}, nil))
			args := string(tc.Arguments)
			for off := 0; off < len(args); off += chunkSize {
				end := off + chunkSize
				if end > len(args) {
					end = len(args)
				}
				for end < len(args) && !utf8.RuneStart(args[end]) {
					end++
				}
				argChunk := args[off:end]
				isLastArgChunk := off+chunkSize >= len(args)
				var finish any
				if isLast && isLastArgChunk {
					finish = "tool_calls"
				}
				emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": argChunk}}}}, finish))
			}
			if len(args) == 0 && isLast {
				emit(base(map[string]any{}, "tool_calls"))
			}
		}
		if sendUsage {
			usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
			_ = sseSafeRaw(w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
		}
		_ = sseSafeRaw(w, flusher, "data: [DONE]\n\n")
		return nil
	}
	jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "m365": compatM365Metadata(res), "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}})
	return nil
}
