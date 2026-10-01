package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// The standing continuity probe asserted "a prefix match happened with the same
// account and conversation". A wrong-thread bind satisfies that too, so it
// never caught this. The proof test that does exist,
// TestBindPrefixInheritanceDoesNotMergeDifferentThreads, uses a TWO-message
// shared prefix while real Hermes tool loops run 100-200 messages — two orders
// of magnitude too small to reach the failing regime.
//
// These tests use the real scale and the real failure shape observed
// 2026-09-29: three sessions in one tenant+fingerprint sharing 197 identical
// messages and diverging only at the user message.

// sharedPreamble builds the ~197-message history that every concurrent Hermes
// thread on one box accumulates: same system prompt, same memory digest, same
// long tool loop. Only the final user message distinguishes the threads.
func sharedPreamble(n int) []oaiMsg {
	msgs := make([]oaiMsg, 0, n)
	msgs = append(msgs, oaiMsg{Role: "system", Content: "You are Hermes on host basic. Memory digest: ... browser panel ..."})
	for i := 0; i < n-3; i++ {
		switch i % 3 {
		case 0:
			msgs = append(msgs, oaiMsg{Role: "user", Content: fmt.Sprintf("step %d: continue the migration", i)})
		case 1:
			msgs = append(msgs, oaiMsg{Role: "assistant", Content: fmt.Sprintf("running check %d", i)})
		default:
			msgs = append(msgs, oaiMsg{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", i), Content: fmt.Sprintf("ok %d", i)})
		}
	}
	return msgs
}

func resolverRig(t *testing.T) (*sessionResolver, *http.Request) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	return openSessionResolver(), resolverTestRequest("203.0.113.10", "client-a", "alice")
}

// TestAmbiguousSharedPrefixRefusesToGuess is the core regression.
//
// The failing shape is NOT "two different threads that happen to share a
// prefix" — those diverge at the user message, so only one is a prefix of any
// given continuation and there is no tie at all. The real shape is a FORK:
// Hermes sends the same full history twice (main thread + the skill-review
// sub-call both carry it), so two sessions end up storing BYTE-IDENTICAL
// history of the same length. Any later continuation is then a prefix of
// BOTH, and the old recency tiebreak picked whichever was touched last.
//
// Fixtures that diverge at the tail pass on the old code and prove nothing —
// that is precisely the gap in the previous 2-message test.
func TestAmbiguousSharedPrefixRefusesToGuess(t *testing.T) {
	sr, req := resolverRig(t)

	// One history, forked into two sessions, exactly as the live state was:
	// two sessions storing BYTE-IDENTICAL contextHistory of the same length,
	// differing only in conversationId. Bind() dedupes identical history, so
	// the fork is installed directly — that is the state on disk today
	// (b08f1b6f / d17b75f2 / d87ca694 all sharing 197 messages).
	forked := append(sharedPreamble(197), oaiMsg{Role: "user", Content: "deploy the web-ssh panel"})
	tenant := tenantFromRequest(req)
	fp := clientIPFingerprint(req)
	now := time.Now().UTC()
	for _, conv := range []string{"cA", "cB"} {
		sr.sessions[conv] = sessionBinding{
			SessionID:      "sess-" + conv,
			ConversationID: conv,
			AccountID:      "acc-x",
			CreatedAt:      now,
			LastUsedAt:     now,
			IPFingerprint:  fp,
			Tenant:         tenant,
			ContextHistory: forked,
		}
	}
	_ = req
	if got := len(sr.sessions); got != 2 {
		t.Fatalf("fork must create 2 sessions, got %d", got)
	}

	// A continuation of the shared history matches BOTH by prefix.
	continuation := append(append([]oaiMsg{}, forked...), oaiMsg{Role: "assistant", Content: "checking the token header"})

	idA, _ := sr.matchContextLocked(tenant, fp, continuation)
	if idA != "" {
		t.Fatalf("tie must not resolve by recency — got %q", idA)
	}

	got := sr.Resolve(req, &oaiReq{Messages: continuation})
	if got.ConversationID == "cB" {
		t.Fatalf("CROSSED: continuation silently bound to the other fork %q", got.ConversationID)
	}
}

// TestUnambiguousPrefixStillBinds guards the ordinary single-thread tool loop:
// with one candidate the bind must still happen, or every request would open a
// new cloud conversation and lose continuity.
func TestUnambiguousPrefixStillBinds(t *testing.T) {
	sr, req := resolverRig(t)

	hist := append(sharedPreamble(40), oaiMsg{Role: "user", Content: "deploy the gateway"})
	sr.Bind("", "solo", "acc-x", &oaiReq{Messages: hist}, "", req)

	next := append(append([]oaiMsg{}, hist...), oaiMsg{Role: "assistant", Content: "deploying"})
	got := sr.Resolve(req, &oaiReq{Messages: next})
	if got.ConversationID != "solo" {
		t.Fatalf("single-thread continuation must still bind, got conversation=%q matchedBy=%q", got.ConversationID, got.MatchedBy)
	}
	if got.HistoryLen != len(hist) {
		t.Fatalf("HistoryLen should be the stored prefix length %d, got %d", len(hist), got.HistoryLen)
	}
}

// TestDivergingThreadsStillResolveByPrefix proves the fix did not over-correct:
// two threads that share a long prefix but DIVERGE at the user message are
// still told apart by prefix length, because only one is a prefix of the
// continuation. This is the ordinary multi-task case and must keep binding.
func TestDivergingThreadsStillResolveByPrefix(t *testing.T) {
	sr, req := resolverRig(t)

	preamble := sharedPreamble(197)
	threadA := append(append([]oaiMsg{}, preamble...), oaiMsg{Role: "user", Content: "删除文件时报 missing token，帮我看看"})
	threadB := append(append([]oaiMsg{}, preamble...), oaiMsg{Role: "user", Content: "Review the conversation above and update the skill library."})
	sr.Bind("", "cA", "acc-x", &oaiReq{Messages: threadA}, "", req)
	sr.Bind("", "cB", "acc-x", &oaiReq{Messages: threadB}, "", req)

	cont := append(append([]oaiMsg{}, threadA...), oaiMsg{Role: "assistant", Content: "on it"})
	got := sr.Resolve(req, &oaiReq{Messages: cont})
	if got.ConversationID != "cA" {
		t.Fatalf("A's continuation must bind to A, got %q (matchedBy=%q)", got.ConversationID, got.MatchedBy)
	}
}
