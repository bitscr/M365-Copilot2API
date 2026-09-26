package web

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// Hermes (and other OpenAI-compatible clients) fire lightweight internal
// "session title" requests: a system prompt that starts with "You name chat
// sessions" plus the user's opening message, expecting a JSON reply
// {"title":"..."}, 0 tools, 2 messages. The M365 upstream returns empty
// completions for these short instruction requests and the gateway burns
// 15-20s retrying/cooldown until it answers 502 — the client then reports
// "Custom endpoint rejected the request" and the real task stalls on a
// cosmetic side-call.
//
// A title is pure metadata: it never needs a strong model. Generate it
// locally and answer immediately. This is the same short-circuit shape as
// publicIdentityAnswer (dialogue-level, no upstream, deterministic).
//
// The system prompt that marks this request type is stable across Hermes
// versions ("You name chat sessions". … "Reply with JSON only: {\"title\":
// \"...\"}"). Match defensively: system must contain the naming instruction
// AND the user message must be short (title requests carry only the opening
// message, usually one line).

var sessionTitleSystemPattern = regexp.MustCompile(`(?is)you\s+name\s+chat\s+sessions|name\s+this\s+conversation|write\s+a\s+title|generate\s+a\s+title|session\s+title`)

const maxTitleSourceLen = 240

// detectTitleRequest reports whether this request is the client's session
// title side-call (system = naming prompt + single short user message).
func detectTitleRequest(messages []oaiMsg) (string, bool) {
	if len(messages) != 2 {
		return "", false
	}
	sys := strings.TrimSpace(contentToString(messages[0].Content))
	if !sessionTitleSystemPattern.MatchString(sys) {
		return "", false
	}
	userText := strings.TrimSpace(contentToString(messages[1].Content))
	if userText == "" || len(userText) > maxTitleSourceLen {
		return "", false
	}
	return userText, true
}

// deriveTitle produces the {"title":"..."} JSON from the user's opening
// message. Rules mirror the client's naming system prompt:
//   - 3-7 "words", sentence case
//   - name what the user wants DONE, not that they asked a question
//   - keep technical terms, filenames, numbers, error codes exact
//   - drop filler words: the, this, my, a, an
//   - no trailing punctuation, no quotes
//   - same language as the user's message
func deriveTitle(userText string) string {
	// Language detection: Han characters make it Chinese-ish, else English.
	hasHan := strings.ContainsFunc(userText, func(r rune) bool {
		return unicode.Is(unicode.Han, r)
	})

	// Strip URLs, leading spaces, trailing punctuation.
	text := stripURLsRegex.ReplaceAllString(userText, " ")
	text = strings.TrimSpace(text)

	if hasHan {
		return deriveChineseTitle(text)
	}
	return deriveEnglishTitle(text)
}

var stripURLsRegex = regexp.MustCompile(`https?://\S+|www\.\S+|\S+\.\S+\s*`)

// deriveChineseTitle: CJK has no word boundaries, so the "3-7 words" rule
// becomes "3-14 chars" (approx 3-7 two-char words), first-run sentence case
// (no case in Han), punctuation stripped.
func deriveChineseTitle(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsPunct(r) && r != '_' && r != '-' {
			continue
		}
		b.WriteRune(r)
	}
	clean := strings.TrimSpace(b.String())
	// Drop leading filler (再次/继续/关于/请问 and similar).
	clean = strings.TrimLeft(clean, "再次继续关于请问请")
	runes := []rune(clean)
	if len(runes) == 0 {
		return "会话"
	}
	const maxRunes = 14
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return strings.TrimSpace(string(runes))
}

var englishFillerWords = map[string]bool{
	// Definite/demonstrative articles only — prepositions ("on", "in", "for")
	// are structural and must stay ("Fix login button on mobile" -> not
	// "Fix login button mobile").
	"the": true, "this": true, "that": true, "my": true, "a": true, "an": true,
}

func deriveEnglishTitle(text string) string {
	fields := strings.Fields(text)
	kept := make([]string, 0, 8)
	for _, w := range fields {
		clean := strings.Trim(w, ".,;:!?")
		if clean == "" {
			continue
		}
		if englishFillerWords[strings.ToLower(clean)] && len(kept) > 0 {
			continue
		}
		kept = append(kept, clean)
		if len(kept) == 7 {
			break
		}
	}
	if len(kept) == 0 {
		return "Chat"
	}
	// sentence case: first word capitalized, rest normal (keep proper
	// nouns/numbers/error codes exact as-is).
	kept[0] = strings.ToUpper(kept[0][:1]) + kept[0][1:]
	title := strings.Join(kept, " ")
	if len(title) > 60 {
		title = title[:60]
	}
	if title == "" {
		return "Chat"
	}
	return title
}

// titleShortcut is the top-level entry used from openaiChat: returns the
// full JSON body for a detected title request, or ("", false).
func titleShortcut(messages []oaiMsg) (string, bool) {
	userText, ok := detectTitleRequest(messages)
	if !ok {
		return "", false
	}
	title := deriveTitle(userText)
	if title == "" {
		title = "Chat"
	}
	b, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return "", false
	}
	return string(b), true
}