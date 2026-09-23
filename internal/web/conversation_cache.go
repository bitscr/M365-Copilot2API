package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"m365-copilot2api/internal/chathub"
	"sync"
	"time"
)

type cachedConversation struct {
	ConversationID string
	SessionID      string
	Tone           string
	TurnCount      int
	MessageCount   int
	CreatedAt      time.Time
	LastUsedAt     time.Time
	SystemPrompt   string
	// PrefixFinger is a canonical fingerprint of the stored message set. A
	// lookup only reuses the conversation when the caller's message prefix
	// fingerprints identically, so a different task with the same system
	// prompt and a larger message count can never inherit this conversation.
	PrefixFinger string
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConversation
	maxAge  time.Duration
}

func newConversationCache() *conversationCache {
	return &conversationCache{
		entries: make(map[string]*cachedConversation),
		maxAge:  2 * time.Hour,
	}
}

func (c *conversationCache) key(accountID, model, tenant string) string {
	return tenant + "\x00" + accountID + "\x00" + model
}

// tenantHash derives a stable caller-scoped key from an API key, or "" when
// no key is present. Mirrors tenantFromRequest so the conversation cache never
// needs an http.Request; callers pass tenantFromRequest(r) directly instead.
func tenantHash(raw string) string {
	if raw == "" {
		return ""
	}
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:16])
}

func (c *conversationCache) Lookup(accountID, model, tenant string) *cachedConversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[c.key(accountID, model, tenant)]
	if entry == nil {
		return nil
	}
	if time.Since(entry.LastUsedAt) > c.maxAge {
		delete(c.entries, c.key(accountID, model, tenant))
		return nil
	}
	return entry
}

func (c *conversationCache) Store(accountID, model, tenant string, conv *cachedConversation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv.LastUsedAt = time.Now()
	c.entries[c.key(accountID, model, tenant)] = conv
}

func (c *conversationCache) Invalidate(accountID, model, tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.key(accountID, model, tenant))
}

// InvalidateByConversation drops every cache entry bound to the given cloud
// conversation ID. Used when a cloud conversation is deleted so subsequent
// requests never reuse a dead ConversationID through the fast-path cache.
func (c *conversationCache) InvalidateByConversation(conversationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.entries {
		if v.ConversationID == conversationID {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.entries {
		if now.Sub(v.LastUsedAt) > c.maxAge {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"cached_conversations": len(c.entries)}
}

func systemPromptHash(messages []oaiMsg) string {
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			text := contentToString(m.Content)
			h := sha256.Sum256([]byte(text))
			return hex.EncodeToString(h[:])
		}
	}
	return ""
}

func extractLastUserMessage(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

// messagesFingerprint canonicalizes a message set into a stable hash using the
// same equality rules as the session resolver: role + text content + tool call
// name/arguments, with tool call IDs ignored (clients regenerate IDs on
// replay). Used to prove that a cache hit is a strict continuation of the
// stored conversation, never a different task's look-alike prefix.
func messagesFingerprint(msgs []oaiMsg) string {
	h := sha256.New()
	for _, m := range msgs {
		io.WriteString(h, m.Role)
		io.WriteString(h, "\x00")
		io.WriteString(h, contentToString(m.Content))
		io.WriteString(h, "\x00")
		if m.ToolCalls != nil {
			for _, raw := range m.ToolCalls {
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				io.WriteString(h, name)
				io.WriteString(h, "(")
				io.WriteString(h, args)
				io.WriteString(h, ");")
			}
		}
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// convCacheHit reports whether a cached conversation may be reused for the
// given request messages: same system prompt shape, strictly more messages,
// and an identical canonical prefix fingerprint (a strict continuation of the
// SAME conversation). A different task that happens to share the account,
// model and system prompt template but diverges in content never hits, which
// prevents cross-task memory bleed.
func convCacheHit(cached *cachedConversation, messages []oaiMsg) bool {
	if cached == nil || cached.SystemPrompt == "" || cached.PrefixFinger == "" {
		return false
	}
	if len(messages) <= cached.MessageCount {
		return false
	}
	if systemPromptHash(messages) != cached.SystemPrompt {
		return false
	}
	return messagesFingerprint(messages[:cached.MessageCount]) == cached.PrefixFinger
}

func (s *Server) storeConvCache(tenant, accID, model string, res chathub.Result, tone string, messages []oaiMsg, reused bool) {
	if res.ConversationID == "" {
		return
	}
	cached := s.convCache.Lookup(accID, model, tenant)
	entry := &cachedConversation{
		ConversationID: res.ConversationID,
		SessionID:      res.SessionID,
		Tone:           tone,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
		PrefixFinger:   messagesFingerprint(messages),
	}
	if cached != nil && cached.ConversationID == res.ConversationID {
		entry.TurnCount = cached.TurnCount + 1
	} else {
		entry.TurnCount = 1
	}
	s.convCache.Store(accID, model, tenant, entry)
}

func (s *Server) invalidateConvCache(accID, model, tenant string) {
	s.convCache.Invalidate(accID, model, tenant)
}
