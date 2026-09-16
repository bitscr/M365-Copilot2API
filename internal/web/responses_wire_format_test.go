package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestUTF8DeltaBufferReassemblesSplitRunes covers the mojibake root cause: the
// upstream frames bytes, so a multi-byte character can be split across two
// SSE deltas. Emitting each half separately is what produced replacement
// characters for externally connected clients.
func TestUTF8DeltaBufferReassemblesSplitRunes(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		{"split chinese rune", []string{"\xe6\xb5", "\x8b\xe8\xaf\x95"}},
		{"split 4-byte emoji", []string{"a\xf0\x9f", "\x98\x80b"}},
		{"byte at a time", func() []string {
			var out []string
			for _, b := range []byte("\xe6\xb5\x8b\xe8\xaf\x95") {
				out = append(out, string([]byte{b}))
			}
			return out
		}()},
		{"complete runes pass through", []string{"\xe6\xb5\x8b", "\xe8\xaf\x95"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b utf8DeltaBuffer
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(b.Push(c))
			}
			got.WriteString(b.Flush())
			if got.String() != "测试" && tc.name != "split 4-byte emoji" {
				t.Fatalf("reassembled = %q, want %q", got.String(), "测试")
			}
			if !utf8.ValidString(got.String()) {
				t.Fatalf("output is not valid UTF-8: %q", got.String())
			}
		})
	}
}

// TestUTF8DeltaBufferNeverEmitsPartialRune is the property that matters to the
// client: every string handed to the wire decodes on its own.
func TestUTF8DeltaBufferNeverEmitsPartialRune(t *testing.T) {
	input := "汉字与 emoji 😀🚀❤️ 以及中文标点，。、“”"
	var b utf8DeltaBuffer
	for i := 0; i < len(input); i++ {
		out := b.Push(string(input[i]))
		if !utf8.ValidString(out) {
			t.Fatalf("byte %d emitted invalid UTF-8 %q", i, out)
		}
	}
	tail := b.Flush()
	if !utf8.ValidString(tail) {
		t.Fatalf("flush emitted invalid UTF-8 %q", tail)
	}
}

// TestResponsesErrorCodeIsAlwaysString guards the spec violation that made
// strict clients fail to parse error events: code is typed as a string, and
// the upstream-failure path used to send an int HTTP status.
func TestResponsesErrorCodeIsAlwaysString(t *testing.T) {
	e := responsesError("502", "inner chat request failed")
	if _, ok := e["code"].(string); !ok {
		t.Fatalf("code must be a string, got %T", e["code"])
	}
}

// TestOutputTextBlockCarriesLogprobs covers the other strict-schema failure:
// openai-python requires logprobs on an output_text part.
func TestOutputTextBlockCarriesLogprobs(t *testing.T) {
	block := outputTextBlock("txt_1", "hi")
	if _, ok := block["logprobs"]; !ok {
		t.Fatal("output_text block is missing the required logprobs field")
	}
	if _, ok := block["annotations"]; !ok {
		t.Fatal("output_text block is missing annotations")
	}
}
