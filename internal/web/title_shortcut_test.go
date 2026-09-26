package web

import (
	"encoding/json"
	"strings"
	"testing"
)

const titleSystemPrompt = `You name chat sessions. Given the user's opening message, write a title that lets them find this conversation again in a list.

Rules:
- 3 to 7 words, sentence case (capitalize only the first word and proper nouns).
- Name what the user wants DONE, not that they asked a question.
- Keep technical terms, filenames, numbers, and error codes exact.
- Drop filler words: the, this, my, a, an.
- No trailing punctuation, no quotes, no tool names, no 'Title:' prefix.
- Never answer the message. Name it.
- Always produce something, even for a bare greeting.
- Write the title in the same language as the user's message.
Good: {"title": "Fix login button on mobile"}
Too vague: {"title": "Code changes"}
Reply with JSON only: {"title": "..."}`

func TestDetectTitleRequest(t *testing.T) {
	cases := []struct {
		name     string
		messages []oaiMsg
		wantOK   bool
	}{
		{
			name: "hermes title call",
			messages: []oaiMsg{
				{Role: "system", Content: titleSystemPrompt},
				{Role: "user", Content: "恢复tg不通的情况"},
			},
			wantOK: true,
		},
		{
			name: "hermes title call english",
			messages: []oaiMsg{
				{Role: "system", Content: titleSystemPrompt},
				{Role: "user", Content: "Fix login button on mobile"},
			},
			wantOK: true,
		},
		{
			name: "normal chat not title",
			messages: []oaiMsg{
				{Role: "system", Content: "You are a helpful assistant."},
				{Role: "user", Content: "恢复tg不通的情况"},
			},
			wantOK: false,
		},
		{
			name: "security reviewer not title",
			messages: []oaiMsg{
				{Role: "system", Content: "You are a security reviewer for an AI coding agent. You assess whether shell commands are safe to execute."},
				{Role: "user", Content: "The following command was flagged as: script execution via heredoc"},
			},
			wantOK: false,
		},
		{
			name: "multi message not title",
			messages: []oaiMsg{
				{Role: "system", Content: titleSystemPrompt},
				{Role: "user", Content: "恢复tg不通"},
				{Role: "assistant", Content: "checking..."},
			},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := titleShortcut(c.messages)
			if ok != c.wantOK {
				t.Fatalf("titleShortcut() ok = %v, want %v", ok, c.wantOK)
			}
		})
	}
}

func TestTitleShortcutOutput(t *testing.T) {
	cases := []struct {
		name      string
		userText  string
		wantTitle string // "" means only validate JSON shape
	}{
		{"chinese short", "恢复tg不通的情况", ""},
		{"chinese task with url", "https://same-loan-heights-answered.trycloudflare.com/再次链接,proot修理,tg链接不通的问题", ""},
		{"chinese mc task", "挂上假人mc端口93.177.102.113 : 26319,名称为yraltd", ""},
		{"english task", "Fix login button on mobile", "Fix login button on mobile"},
		{"english with url", "check https://example.com status please", ""},
		{"english filler heavy", "the this my a an and of to in fix this bug", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := titleShortcut([]oaiMsg{
				{Role: "system", Content: titleSystemPrompt},
				{Role: "user", Content: c.userText},
			})
			if !ok {
				t.Fatal("titleShortcut() not detected")
			}
			var parsed struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("output %q is not valid JSON: %v", out, err)
			}
			if parsed.Title == "" {
				t.Fatalf("empty title from %q", c.userText)
			}
			if c.wantTitle != "" && parsed.Title != c.wantTitle {
				t.Fatalf("title = %q, want %q", parsed.Title, c.wantTitle)
			}
			if strings.Contains(parsed.Title, "http") {
				t.Fatalf("title %q leaked a URL", parsed.Title)
			}
			if len(parsed.Title) > 80 {
				t.Fatalf("title %q too long", parsed.Title)
			}
		})
	}
}

func TestDeriveChineseTitle(t *testing.T) {
	cases := []struct{ in, wantSub string }{
		{"恢复tg不通的情况", "恢复tg不通"},
		{"再次链接proot修理tg链接不通的问题", "proot"}, // URL stripped, filler dropped
		{"挂上假人mc端口93.177.102.113:26319,名称为yraltd", "挂上假人"},
	}
	for _, c := range cases {
		got := deriveChineseTitle(c.in)
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("deriveChineseTitle(%q) = %q, want substring %q", c.in, got, c.wantSub)
		}
	}
}