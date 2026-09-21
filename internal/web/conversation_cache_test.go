package web

import (
	"testing"
)

func convCacheTestMessages(prefix string, n int) []oaiMsg {
	out := make([]oaiMsg, 0, n+1)
	out = append(out, oaiMsg{Role: "system", Content: "system-prompt-template"})
	for i := 0; i < n; i++ {
		out = append(out, oaiMsg{Role: "user", Content: prefix + "-q" + string(rune('a'+i))}, oaiMsg{Role: "assistant", Content: prefix + "-a" + string(rune('a'+i))})
	}
	return out
}

func convCacheTestEntry(messages []oaiMsg, conversationID string) *cachedConversation {
	return &cachedConversation{
		ConversationID: conversationID,
		SessionID:      "sess-" + conversationID,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
		PrefixFinger:   messagesFingerprint(messages),
	}
}

// TestConvCacheHitSameTaskContinuation verifies the incremental-resume benefit
// survives: an identical message prefix plus more messages is a hit.
func TestConvCacheHitSameTaskContinuation(t *testing.T) {
	stored := convCacheTestMessages("taskA", 2)
	entry := convCacheTestEntry(stored, "conv-a")
	next := convCacheTestMessages("taskA", 3) // same task, one more turn
	if !convCacheHit(entry, next) {
		t.Fatal("same-task continuation must hit the cache")
	}
}

// TestConvCacheHitCrossTaskRejected is the regression test for the memory
// cross-talk bug: two different tasks sharing the same system prompt template
// where the second task merely has MORE messages must never reuse the first
// task's cloud conversation (that inheritance made the upstream model cite
// files/names from the wrong task).
func TestConvCacheHitCrossTaskRejected(t *testing.T) {
	stored := convCacheTestMessages("taskA", 2)
	entry := convCacheTestEntry(stored, "conv-a")
	taskB := convCacheTestMessages("taskB", 3) // different content, more messages
	if convCacheHit(entry, taskB) {
		t.Fatal("cross-task request must never reuse another task's conversation")
	}
}

// TestConvCacheHitDifferentSystemPromptRejected verifies the system prompt
// shape is still part of the gate.
func TestConvCacheHitDifferentSystemPromptRejected(t *testing.T) {
	stored := convCacheTestMessages("taskA", 2)
	entry := convCacheTestEntry(stored, "conv-a")
	other := convCacheTestMessages("taskA", 3)
	other[0].Content = "another-system-prompt-template"
	if convCacheHit(entry, other) {
		t.Fatal("different system prompt must not hit")
	}
}

// TestConvCacheHitShorterMessagesRejected verifies the gate refuses reuse when
// the request carries no new messages beyond the stored history.
func TestConvCacheHitShorterMessagesRejected(t *testing.T) {
	stored := convCacheTestMessages("taskA", 3)
	entry := convCacheTestEntry(stored, "conv-a")
	shorter := convCacheTestMessages("taskA", 2)
	if convCacheHit(entry, shorter) {
		t.Fatal("no-growth request must not hit")
	}
}

// TestConvCacheTenantIsolation verifies two API keys (tenants) sharing the
// same account+model never share a cache slot at all.
func TestConvCacheTenantIsolation(t *testing.T) {
	c := newConversationCache()
	stored := convCacheTestMessages("taskA", 1)
	entry := convCacheTestEntry(stored, "conv-a")
	c.Store("acc1", "m365-copilot", "tenant-1", entry)

	if got := c.Lookup("acc1", "m365-copilot", "tenant-2"); got != nil {
		t.Fatal("different tenant must not see another tenant's cache slot")
	}
	if got := c.Lookup("acc1", "m365-copilot", "tenant-1"); got == nil {
		t.Fatal("own tenant must see its cache slot")
	}
}

// TestConvCacheInvalidateIsTenantScoped verifies invalidation only drops the
// matching tenant's slot.
func TestConvCacheInvalidateIsTenantScoped(t *testing.T) {
	c := newConversationCache()
	stored := convCacheTestMessages("taskA", 1)
	entry := convCacheTestEntry(stored, "conv-a")
	c.Store("acc1", "m365-copilot", "tenant-1", entry)
	c.Store("acc1", "m365-copilot", "tenant-2", entry)
	c.Invalidate("acc1", "m365-copilot", "tenant-1")

	if got := c.Lookup("acc1", "m365-copilot", "tenant-1"); got != nil {
		t.Fatal("tenant-1 slot must be gone")
	}
	if got := c.Lookup("acc1", "m365-copilot", "tenant-2"); got == nil {
		t.Fatal("tenant-2 slot must survive")
	}
}
