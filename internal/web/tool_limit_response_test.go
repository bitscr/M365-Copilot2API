package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestConfiguredLimitSerializesOneToolCall(t *testing.T) {
	calls := []detectedToolCall{{ID: "call_1", Name: "first", Arguments: json.RawMessage(`{"x":1}`)}, {ID: "call_2", Name: "second", Arguments: json.RawMessage(`{"y":2}`)}}
	w := httptest.NewRecorder()
	limited := limitToolCalls(calls, 1)
	if err := writeToolResponse(w, "chatcmpl_test", "test", false, true, limited, chathub.Result{}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	got := msg["tool_calls"].([]any)
	if len(got) != 1 {
		t.Fatalf("serialized %d calls: %s", len(got), w.Body.String())
	}
	fn := got[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "first" {
		t.Fatalf("wrong call: %#v", fn)
	}
}

// Local-exec (or any no-tool) final answer must NOT be serialized as a
// tool_calls response: the gateway already ran the tools and holds the final
// answer text. Regression for the empty-content + finish_reason=tool_calls
// response seen after the local executor landed.
func TestWriteToolResponseNoCallsSerializesFinalAnswer(t *testing.T) {
	res := chathub.Result{Text: "git version 2.43.0 outputs on the gateway host.", ConversationID: "conv_local"}
	for _, stream := range []bool{false, true} {
		w := httptest.NewRecorder()
		if err := writeToolResponse(w, "chatcmpl_test", "test", stream, false, nil, res); err != nil {
			t.Fatal(err)
		}
		if stream {
			body := w.Body.String()
			if !strings.Contains(body, "git version 2.43.0") {
				t.Fatalf("stream body missing answer text: %s", body)
			}
			if !strings.Contains(body, `"finish_reason":"stop"`) {
				t.Fatalf("stream body missing stop finish: %s", body)
			}
			if strings.Contains(body, `"tool_calls"`) {
				t.Fatalf("stream body leaked tool_calls: %s", body)
			}
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		choices := out["choices"].([]any)
		choice := choices[0].(map[string]any)
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason=%v want stop: %s", choice["finish_reason"], w.Body.String())
		}
		msg := choice["message"].(map[string]any)
		if msg["content"] != "git version 2.43.0 outputs on the gateway host." {
			t.Fatalf("wrong content: %#v", msg["content"])
		}
		if _, has := msg["tool_calls"]; has {
			t.Fatalf("final answer message leaked tool_calls: %s", w.Body.String())
		}
	}
}
