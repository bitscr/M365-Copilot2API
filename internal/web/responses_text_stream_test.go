package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// collectResponsesStream parses a Responses SSE body into ordered event maps.
func collectResponsesStream(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// TestResponsesTextStreamEmitsEachChunkOnce is the regression guard for the bug
// that duplicated answers: the handler sent the accumulated body from the delta
// loop and then restated it from an end-of-stream fallback.
func TestResponsesTextStreamEmitsEachChunkOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	ss := newResponsesSSEStream(nil, rec, rec, "m")
	ts := newResponsesTextStream(ss)

	chunks := []string{"你好", "，", "世界", "!"}
	for _, c := range chunks {
		if err := ts.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	ts.Finish()

	want := strings.Join(chunks, "")
	var accumulated, doneText string
	addedCount := 0
	for _, ev := range collectResponsesStream(t, rec.Body.String()) {
		switch ev["type"] {
		case "response.output_item.added":
			addedCount++
		case "response.output_text.delta":
			accumulated += ev["delta"].(string)
		case "response.output_text.done":
			doneText = ev["text"].(string)
		}
	}
	if accumulated != want {
		t.Fatalf("deltas did not reproduce the text exactly once:\nwant %q\n got %q", want, accumulated)
	}
	if doneText != want {
		t.Fatalf("done text %q != deltas %q", doneText, want)
	}
	if ts.Text() != want {
		t.Fatalf("accumulated text %q != %q", ts.Text(), want)
	}
	if addedCount != 1 {
		t.Fatalf("output_item.added emitted %d times, want exactly 1", addedCount)
	}
}

// TestResponsesTextStreamItemMatchesDeltas pins the third side of the invariant
// a client rebuilds the answer from.
func TestResponsesTextStreamItemMatchesDeltas(t *testing.T) {
	rec := httptest.NewRecorder()
	ss := newResponsesSSEStream(nil, rec, rec, "m")
	ts := newResponsesTextStream(ss)
	for _, c := range []string{"a", "b", "c"} {
		if err := ts.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	ts.Finish()

	item := ts.Item()
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("item content = %#v", item["content"])
	}
	part, _ := content[0].(map[string]any)
	if got := part["text"]; got != "abc" {
		t.Fatalf("item text = %v, want abc", got)
	}
	if item["status"] != "completed" {
		t.Fatalf("item status = %v, want completed", item["status"])
	}
}

// TestResponsesTextStreamHoldsSplitRuneAcrossChunks covers the mojibake path: a
// multi-byte rune arriving in two pieces must reach the client whole and only
// once.
func TestResponsesTextStreamHoldsSplitRuneAcrossChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	ss := newResponsesSSEStream(nil, rec, rec, "m")
	ts := newResponsesTextStream(ss)

	if err := ts.Append("\xe6\xb5"); err != nil {
		t.Fatal(err)
	}
	if ts.Text() != "" {
		t.Fatalf("half a rune leaked to the wire: %q", ts.Text())
	}
	if err := ts.Append("\x8b"); err != nil {
		t.Fatal(err)
	}
	ts.Finish()
	if ts.Text() != "测" {
		t.Fatalf("reassembled = %q, want 测", ts.Text())
	}

	var accumulated string
	for _, ev := range collectResponsesStream(t, rec.Body.String()) {
		if ev["type"] == "response.output_text.delta" {
			accumulated += ev["delta"].(string)
		}
	}
	if accumulated != "测" {
		t.Fatalf("wire text = %q, want 测", accumulated)
	}
}

// TestResponsesTextStreamSkipsEmptyChunks keeps a keepalive-shaped empty delta
// from emitting a spurious message item.
func TestResponsesTextStreamSkipsEmptyChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	ss := newResponsesSSEStream(nil, rec, rec, "m")
	ts := newResponsesTextStream(ss)
	if err := ts.Append(""); err != nil {
		t.Fatal(err)
	}
	if ts.Started() {
		t.Fatal("empty chunk started the text item")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("empty chunk wrote %q", rec.Body.String())
	}
}
