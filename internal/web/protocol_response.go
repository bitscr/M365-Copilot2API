package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// utf8DeltaBuffer reassembles multi-byte UTF-8 sequences that the upstream
// splits across SSE deltas. Upstream frames are byte-oriented, so a Chinese
// character (3 bytes) or emoji (4 bytes) can be cut in half; emitting each half
// separately produces replacement characters at the client. The buffer holds an
// incomplete trailing rune until the next delta completes it, and flushes any
// remainder at stream end so no bytes are silently dropped.
type utf8DeltaBuffer struct {
	pending []byte
}

// Push returns the longest prefix that ends on a rune boundary. Any incomplete
// trailing sequence is retained for the next call.
func (b *utf8DeltaBuffer) Push(chunk string) string {
	if chunk == "" {
		return ""
	}
	b.pending = append(b.pending, chunk...)
	n := len(b.pending)
	// Walk back over at most utf8.UTFMax-1 bytes looking for a valid boundary.
	for cut := 0; cut < utf8.UTFMax && cut <= n; cut++ {
		end := n - cut
		if end == 0 {
			break
		}
		if utf8.Valid(b.pending[:end]) {
			out := string(b.pending[:end])
			b.pending = append(b.pending[:0], b.pending[end:]...)
			return out
		}
	}
	// The buffer does not yet contain a complete rune: hold everything.
	if !utf8.Valid(b.pending) {
		return ""
	}
	out := string(b.pending)
	b.pending = b.pending[:0]
	return out
}

// Flush releases whatever is left, replacing any genuinely truncated tail so
// the stream still terminates with valid UTF-8.
func (b *utf8DeltaBuffer) Flush() string {
	if len(b.pending) == 0 {
		return ""
	}
	out := strings.ToValidUTF8(string(b.pending), "\uFFFD")
	b.pending = b.pending[:0]
	return out
}

// responsesSSEStream centralizes the wire format for /v1/responses streaming.
// It owns the monotonic sequence counter that the OpenAI Responses spec
// requires on every event: clients (Codex CLI, openai-python) use it for
// ordering and replay validation, and its absence makes them drop or reject
// events. All writes go through one mutex so the counter stays monotonic even
// when a keepalive goroutine emits concurrently.
type responsesSSEStream struct {
	w        http.ResponseWriter
	f        http.Flusher
	r        *http.Request
	mu       sync.Mutex
	seq      int
	aborted  bool
	respMeta map[string]any
	deltaBuf utf8DeltaBuffer
}

func newResponsesSSEStream(r *http.Request, w http.ResponseWriter, f http.Flusher, model string) *responsesSSEStream {
	return &responsesSSEStream{
		w: w, f: f, r: r,
		respMeta: responsesResponseSkeleton(model),
	}
}

// responsesResponseSkeleton returns the fields the Responses spec guarantees on
// every response object. Emitting a partial skeleton (the previous behaviour)
// makes strict SDKs fail validation at response.created time.
func responsesResponseSkeleton(model string) map[string]any {
	return map[string]any{
		"id":                   "",
		"object":               "response",
		"created_at":           0,
		"status":               "in_progress",
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"tool_choice":          "auto",
		"tools":                []any{},
		"temperature":          1,
		"top_p":                1,
		"usage":                nil,
		"metadata":             map[string]any{},
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"previous_response_id": nil,
	}
}

// emit stamps every event with a monotonically increasing sequence_number and
// serializes the write. Adding the field in one place is what fixes ordering
// for every call site instead of patching them individually.
func (s *responsesSSEStream) emit(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		m["sequence_number"] = s.seq
	}
	s.seq++
	if err := sseWriteFrame(s.r, s.w, s.f, name, v); err != nil {
		s.aborted = true
		return err
	}
	return nil
}

// emitCreated publishes response.created with a complete response skeleton so
// clients can build their typed object before any delta arrives.
func (s *responsesSSEStream) emitCreated(id string, model string, createdAt int64) error {
	resp := map[string]any{}
	for k, v := range s.respMeta {
		resp[k] = v
	}
	resp["id"] = id
	resp["model"] = model
	resp["created_at"] = createdAt
	return s.emit("response.created", map[string]any{"type": "response.created", "response": resp})
}

// outputTextDelta emits a text delta after repairing UTF-8 boundaries. Returns
// the bytes actually sent so callers can keep their own reassembled copy in
// sync with what the client received.
func (s *responsesSSEStream) outputTextDelta(outputIndex int, itemID, chunk string) (string, error) {
	safe := s.deltaBuf.Push(chunk)
	if safe == "" {
		return "", nil
	}
	err := s.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": outputIndex,
		"content_index": 0, "item_id": itemID, "delta": safe,
	})
	return safe, err
}

// flushTextDelta releases a held incomplete rune at end of stream so trailing
// text is never lost.
func (s *responsesSSEStream) flushTextDelta(outputIndex int, itemID string) (string, error) {
	tail := s.deltaBuf.Flush()
	if tail == "" {
		return "", nil
	}
	err := s.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": outputIndex,
		"content_index": 0, "item_id": itemID, "delta": tail,
	})
	return tail, err
}

// responsesTextStream owns the single text item of a Responses stream. It is a
// type rather than a few locals in the handler because the invariant that broke
// before -- the same text reaching the client twice, once from the delta loop
// and again from an end-of-stream fallback -- was invisible and untestable while
// it lived inline across two branches.
type responsesTextStream struct {
	ss        *responsesSSEStream
	messageID string
	contentID string
	started   bool
	text      strings.Builder
}

func newResponsesTextStream(ss *responsesSSEStream) *responsesTextStream {
	return &responsesTextStream{
		ss:        ss,
		messageID: "msg_" + uuid.NewString(),
		contentID: "txt_" + uuid.NewString(),
	}
}

// Started reports whether the message item has been emitted.
func (t *responsesTextStream) Started() bool { return t.started }

// ensureItem emits the message item exactly once, immediately before the first
// delta, so output_item.added always precedes the text it describes.
func (t *responsesTextStream) ensureItem() {
	if t.started {
		return
	}
	t.started = true
	_ = t.ss.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "message", "id": t.messageID, "role": "assistant", "status": "in_progress", "content": []any{outputTextBlock(t.contentID, "")}},
	})
}

// Append feeds one upstream content chunk through UTF-8 boundary repair and
// accumulates exactly what reached the wire. Nothing is buffered for a later
// resend: the only text the client ever sees comes from here or from Flush.
func (t *responsesTextStream) Append(chunk string) error {
	if chunk == "" {
		return nil
	}
	t.ensureItem()
	sent, err := t.ss.outputTextDelta(0, t.messageID, chunk)
	if err != nil {
		return err
	}
	t.text.WriteString(sent)
	return nil
}

// Flush releases a rune held back by boundary repair at end of stream.
func (t *responsesTextStream) Flush() error {
	tail, err := t.ss.flushTextDelta(0, t.messageID)
	if err != nil {
		return err
	}
	t.text.WriteString(tail)
	return nil
}

// Finish closes the text item: it releases any held rune, then emits
// output_text.done and output_item.done. Keeping the terminal events in the same
// type as the deltas is what makes "the done text equals the concatenated
// deltas" true by construction rather than by two branches agreeing.
func (t *responsesTextStream) Finish() map[string]any {
	_ = t.Flush()
	text := t.text.String()
	_ = t.ss.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": 0, "content_index": 0,
		"item_id": t.messageID, "text": text,
	})
	item := t.Item()
	_ = t.ss.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0, "item": item,
	})
	return item
}

// Text is the accumulated wire text; it equals the concatenated deltas.
func (t *responsesTextStream) Text() string { return t.text.String() }

// Item is the completed message item for the output array.
func (t *responsesTextStream) Item() map[string]any {
	return map[string]any{
		"type": "message", "id": t.messageID, "role": "assistant", "status": "completed",
		"content": []any{outputTextBlock(t.contentID, t.text.String())},
	}
}

// outputTextBlock builds an output_text content part. logprobs is part of the
// required schema (not optional), and omitting it is what makes openai-python
// raise a validation error when parsing the item.
func outputTextBlock(id, text string) map[string]any {
	return map[string]any{
		"type":        "output_text",
		"id":          id,
		"text":        text,
		"annotations": []any{},
		"logprobs":    []any{},
	}
}

// responsesError renders a Responses-spec error object. code is always a
// string: the spec types it as a string, and passing an int (the previous
// behaviour for HTTP failures) makes strict clients fail to parse the error
// instead of surfacing it.
func responsesError(code, message string) map[string]any {
	return map[string]any{"code": code, "message": message}
}

var errSSEAborted = errors.New("sse stream aborted")

func openAIChoice(v map[string]any) (map[string]any, string) {
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	finish, _ := c["finish_reason"].(string)
	return m, finish
}

func writeAnthropicResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := "msg_" + uuid.NewString()
	msg, finish := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	blocks := []any{}
	stop := "end_turn"
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning, "signature": ""})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		stop = "tool_use"
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			var input any = map[string]any{}
			if a, ok := fn["arguments"].(string); ok {
				_ = json.Unmarshal([]byte(a), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
		}
	} else {
		switch content := msg["content"].(type) {
		case []any:
			for _, raw := range content {
				part, _ := raw.(map[string]any)
				switch part["type"] {
				case "text":
					if t, _ := part["text"].(string); t != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": t})
					}
				case "image_url":
					img, _ := part["image_url"].(map[string]any)
					if u, _ := img["url"].(string); u != "" {
						if strings.HasPrefix(u, "data:") {
							parts := strings.SplitN(u, ",", 2)
							meta := parts[0]
							b64 := ""
							if len(parts) == 2 {
								b64 = parts[1]
							}
							media := strings.TrimPrefix(meta, "data:")
							media = strings.SplitN(media, ";", 2)[0]
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": b64}})
						} else {
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}})
						}
					}
				}
			}
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprint(content)})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
	}
	_ = finish
	inputTokens := int64(0)
	outputTokens := int64(0)
	if u, ok := src["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"]; ok {
			if n, ok := v.(int64); ok {
				inputTokens = n
			}
			if n, ok := v.(float64); ok {
				inputTokens = int64(n)
			}
		}
		if v, ok := u["completion_tokens"]; ok {
			if n, ok := v.(int64); ok {
				outputTokens = n
			}
			if n, ok := v.(float64); ok {
				outputTokens = int64(n)
			}
		}
	}
	out := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}}
	if !stream {
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(n string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(nil, w, f, n, v); err != nil {
			aborted = true
		}
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}})
	for i, b := range blocks {
		m, _ := b.(map[string]any)
		startBlock := b
		blockType := ""
		if t, _ := m["type"].(string); t != "" {
			blockType = t
		}
		switch blockType {
		case "tool_use":
			startBlock = map[string]any{"type": "tool_use", "id": m["id"], "name": m["name"], "input": map[string]any{}}
		case "thinking":
			startBlock = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		case "image":
			startBlock = map[string]any{"type": "image", "source": m["source"]}
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		switch blockType {
		case "text":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": m["text"]}})
		case "tool_use":
			partial, _ := json.Marshal(m["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
		case "thinking":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": m["thinking"]}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// sseWriteFrame writes one SSE frame and flushes; a write error (client gone,
// deadline exceeded) aborts the stream instead of leaving the handler blocked.
func sseWriteFrame(r *http.Request, w http.ResponseWriter, f http.Flusher, name string, value any) error {
	if r != nil {
		if err := r.Context().Err(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseDataRaw writes a raw "data: ..." frame with the same write deadline.
func sseDataRaw(w http.ResponseWriter, f http.Flusher, data string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseSafeRaw writes a pre-formatted frame (e.g. ": connected" or "[DONE]").
func sseSafeRaw(w http.ResponseWriter, f http.Flusher, payload string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseWriter serializes all writes to one streaming response. Keepalive
// goroutines and the main emit loop would otherwise interleave partial
// frames on the shared ResponseWriter (net/http writes are not goroutine-safe).
type sseWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func newSSEWriter(w http.ResponseWriter, f http.Flusher) *sseWriter {
	return &sseWriter{w: w, f: f}
}

func (s *sseWriter) raw(payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := http.NewResponseController(s.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(s.w, payload); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

func (s *sseWriter) data(data string) error {
	return s.raw("data: " + data + "\n\n")
}
