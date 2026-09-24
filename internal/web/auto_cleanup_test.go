package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestServerForAutoCleanup(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	return &Server{
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(30 * time.Minute),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		convCache:           newConversationCache(),
	}
}

func TestAutoCleanupActiveSetProtectsInUse(t *testing.T) {
	s := newTestServerForAutoCleanup(t)

	s.conversationManager.Record("conv-active", "acc1", "active convo")
	s.conversationManager.Record("conv-idle", "acc1", "idle convo")
	s.userSessions.Put("", "alice", "conv-user", "sess-user", "acc1")

	// Simulate activity: active convo touched recently, idle long ago.
	cm := s.conversationManager
	cm.mu.Lock()
	entry := cm.data["conv-active"]
	entry.LastUsedAt = time.Now().UTC()
	cm.data["conv-active"] = entry
	idle := cm.data["conv-idle"]
	idle.LastUsedAt = time.Now().UTC().Add(-48 * time.Hour)
	cm.data["conv-idle"] = idle
	cm.mu.Unlock()

	active := s.activeConversationSet(24 * time.Hour)

	if !active["conv-active"] {
		t.Error("conv-active (recently used) should be protected")
	}
	if !active["conv-user"] {
		t.Error("conv-user (active user session) should be protected")
	}
	if active["conv-idle"] {
		t.Error("conv-idle (48h unused) should be cleanable")
	}
}

func TestAutoCleanupWhitelistProtects(t *testing.T) {
	s := newTestServerForAutoCleanup(t)
	s.conversationManager.Record("conv-pinned", "acc1", "pinned")
	s.conversationManager.Whitelist("conv-pinned")

	active := s.activeConversationSet(0)
	if !active["conv-pinned"] {
		t.Error("whitelisted conversation must never be cleaned")
	}
}

func TestUnbindByConversationRemovesBindings(t *testing.T) {
	s := newTestServerForAutoCleanup(t)

	// Two different tenants each hold a binding to the same cloud conversation
	// conv-x. UnbindByConversation is global maintenance (a deleted cloud
	// conversation must be unbound for everyone), so it removes both.
	reqFor := func(key string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}
	tenantA := tenantFromRequest(reqFor("key-a"))
	tenantB := tenantFromRequest(reqFor("key-b"))
	s.sessionResolver.Bind("sess-1", "conv-x", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hi"}}}, "", reqFor("key-a"))
	s.sessionResolver.Bind("sess-2", "conv-x", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}, "", reqFor("key-b"))
	s.sessionResolver.Bind("sess-3", "conv-y", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "other"}}}, "", reqFor("key-a"))

	if removed := s.sessionResolver.UnbindByConversation("conv-x"); removed != 2 {
		t.Fatalf("expected 2 unbinds, got %d", removed)
	}
	if _, ok := s.sessionResolver.GetSession(tenantA, "sess-1"); ok {
		t.Error("sess-1 should be gone")
	}
	if _, ok := s.sessionResolver.GetSession(tenantB, "sess-2"); ok {
		t.Error("sess-2 should be gone")
	}
	if _, ok := s.sessionResolver.GetSession(tenantA, "sess-3"); !ok {
		t.Error("sess-3 bound to conv-y must survive")
	}
}

func TestWhitelistPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")
	t.Setenv("M365_CONVERSATION_CACHE", path)

	cm1 := openConversationManager()
	cm1.Record("conv-pinned", "acc1", "pinned")
	cm1.Whitelist("conv-pinned")
	cm1.Record("conv-plain", "acc1", "plain")
	if err := cm1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	cm2 := openConversationManager()
	if !cm2.IsWhitelisted("conv-pinned") {
		t.Error("whitelist lost after reload")
	}
	found := false
	for _, id := range cm2.WhitelistedIDs() {
		if id == "conv-pinned" {
			found = true
		}
	}
	if !found {
		t.Error("WhitelistedIDs missing pinned conversation")
	}
}

func TestWhitelistPersistsWithoutOtherActivity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")
	t.Setenv("M365_CONVERSATION_CACHE", path)

	cm1 := openConversationManager()
	cm1.Whitelist("conv-a")
	cm1.Whitelist("conv-b")
	cm1.Unwhitelist("conv-b")
	if err := cm1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	cm2 := openConversationManager()
	if !cm2.IsWhitelisted("conv-a") {
		t.Error("whitelist entry conv-a must survive reload without unrelated Record activity")
	}
	if cm2.IsWhitelisted("conv-b") {
		t.Error("unwhitelisted conv-b must not survive reload")
	}
}

func TestAutoCleanupDisabledEnv(t *testing.T) {
	t.Setenv("M365_AUTO_CLEANUP", "0")
	s := newTestServerForAutoCleanup(t)
	s.StartAutoCleanup()
}

func TestLegacyConversationFileLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conversations.json")
	legacy := `{
  "conv-old": {
    "id": "conv-old",
    "accountId": "acc1",
    "createdAt": "2026-08-01T00:00:00Z",
    "lastUsedAt": "2026-08-01T00:00:00Z",
    "title": "legacy"
  }
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_CONVERSATION_CACHE", path)

	cm := openConversationManager()
	if _, ok := cm.data["conv-old"]; !ok {
		t.Error("legacy conversation file must still load")
	}
}

// TestDropConversationCleansAllStores is the regression test for the
// intermittent "sometimes continues, sometimes doesn't, new chat always
// works" symptom: when a cloud conversation is deleted (manual delete,
// auto-cleanup, or keep-N eviction) the session resolver binding and the
// conversation cache fast-path entry must be dropped with it. Otherwise the
// next request with full history resolves to a dead ConversationID and
// 502s, while a brand-new conversation (no history prefix) works fine.
func TestDropConversationCleansAllStores(t *testing.T) {
	s := newTestServerForAutoCleanup(t)

	reqFor := func(key string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}

	// Simulate a live conversation: bound in the resolver and cached in the
	// conv-cache fast path, exactly as a completed request leaves it.
	msgs := []oaiMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	s.sessionResolver.Bind("sess-dead", "conv-dead", "acc1", &oaiReq{Messages: msgs}, "", reqFor("key-a"))
	s.convCache.Store("acc1", "gpt-5.6-reasoning", tenantFromRequest(reqFor("key-a")), &cachedConversation{
		ConversationID: "conv-dead",
		SessionID:      "sess-dead",
	})
	// A second live conversation must survive.
	s.sessionResolver.Bind("sess-live", "conv-live", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "other"}}}, "", reqFor("key-a"))
	s.convCache.Store("acc1", "gpt-5.6-reasoning", tenantFromRequest(reqFor("key-a")), &cachedConversation{
		ConversationID: "conv-live",
		SessionID:      "sess-live",
	})

	s.dropConversation("conv-dead")

	// Resolver binding gone.
	if _, ok := s.sessionResolver.GetSession(tenantFromRequest(reqFor("key-a")), "sess-dead"); ok {
		t.Error("resolver binding for dead conversation must be removed")
	}
	if _, ok := s.sessionResolver.GetSession(tenantFromRequest(reqFor("key-a")), "sess-live"); !ok {
		t.Error("resolver binding for live conversation must survive")
	}
	// Conv-cache fast-path entries: dead one gone, live one survives.
	if got := s.convCache.Lookup("acc1", "gpt-5.6-reasoning", tenantFromRequest(reqFor("key-a"))); got != nil && got.ConversationID == "conv-dead" {
		t.Error("conv-cache entry for dead conversation must be invalidated")
	}
	if got := s.convCache.Lookup("acc1", "gpt-5.6-reasoning", tenantFromRequest(reqFor("key-a"))); got == nil || got.ConversationID != "conv-live" {
		t.Error("conv-cache entry for live conversation must survive")
	}
	if _, ok := s.conversationManager.data["conv-dead"]; ok {
		t.Error("conversation manager record for dead conversation must be dropped")
	}
}

// TestBindConversationCleanupUnbindsDeadSessions covers the auto-clean path
// in bindConversation: conversationManager.Cleanup() returns ids the manager
// evicted, and each one must be dropped from resolver + conv-cache too.
func TestBindConversationCleanupUnbindsDeadSessions(t *testing.T) {
	s := newTestServerForAutoCleanup(t)
	// Keep-N mode with a capacity of 2: recording a third conversation evicts
	// the least-recently-used one.
	s.conversationManager.SetMode(CleanupKeepN)
	s.conversationManager.keepN = 2

	reqFor := func(key string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}
	tenant := tenantFromRequest(reqFor("key-a"))

	// Two LRU evictions.
	for i, cv := range []string{"conv-old", "conv-live"} {
		s.sessionResolver.Bind("sess-"+cv, cv, "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: cv}}}, "", reqFor("key-a"))
		s.convCache.Store("acc1", "gpt-5.6-reasoning", tenant, &cachedConversation{ConversationID: cv, SessionID: "sess-" + cv})
		s.conversationManager.Record(cv, "acc1", cv)
		// Backdate the first one so it becomes LRU when the third lands.
		if i == 0 {
			s.conversationManager.mu.Lock()
			old := s.conversationManager.data[cv]
			old.LastUsedAt = time.Now().UTC().Add(-time.Hour)
			s.conversationManager.data[cv] = old
			s.conversationManager.mu.Unlock()
		}
	}

	// The third binding + auto-clean trigger.
	s.sessionResolver.Bind("sess-new", "conv-new", "acc1", &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "conv-new"}}}, "", reqFor("key-a"))
	s.conversationManager.Record("conv-new", "acc1", "conv-new")
	if s.conversationManager.ShouldCleanup() {
		if cleaned := s.conversationManager.Cleanup(); len(cleaned) > 0 {
			for _, cvID := range cleaned {
				s.dropConversation(cvID)
			}
		}
	}

	// conv-old was evicted: resolver binding and conv-cache entry gone.
	if _, ok := s.sessionResolver.GetSession(tenant, "sess-conv-old"); ok {
		t.Error("evicted conversation binding must be unbound from resolver")
	}
	if got := s.convCache.Lookup("acc1", "gpt-5.6-reasoning", tenant); got != nil && got.ConversationID == "conv-old" {
		t.Error("conv-cache must not reuse an evicted conversation")
	}
	// The live and new conversations survive.
	if _, ok := s.sessionResolver.GetSession(tenant, "sess-conv-live"); !ok {
		t.Error("live conversation binding must survive cleanup")
	}
	if _, ok := s.sessionResolver.GetSession(tenant, "sess-new"); !ok {
		t.Error("new conversation binding must survive cleanup")
	}
}

// TestDefaultCleanupModeDoesNotReapIdleConversations guards the regression that
// made cloud conversations live ~30s: the default mode used to be
// CleanupAfterResponse, whose branch deletes every conversation idle longer
// than 30s — and bindConversation calls Cleanup() after EVERY response. Session
// reuse could therefore never hit (list emptied on refresh, every turn
// round-robined to a new account). The default must not be after_response, and
// a conversation touched 45s ago must survive a default-mode Cleanup().
func TestDefaultCleanupModeDoesNotReapIdleConversations(t *testing.T) {
	t.Setenv("M365_CLEANUP_MODE", "")
	s := newTestServerForAutoCleanup(t)

	if got := s.conversationManager.Mode(); got == CleanupAfterResponse {
		t.Fatalf("default cleanup mode must not be after_response (got %q)", got)
	}
	if s.conversationManager.ShouldCleanup() {
		t.Fatalf("default mode %q must not trigger request-path cleanup", s.conversationManager.Mode())
	}

	s.conversationManager.Record("conv-idle-45s", "acc1", "idle 45s")
	cm := s.conversationManager
	cm.mu.Lock()
	entry := cm.data["conv-idle-45s"]
	entry.LastUsedAt = time.Now().UTC().Add(-45 * time.Second)
	cm.data["conv-idle-45s"] = entry
	cm.mu.Unlock()

	if cleaned := cm.Cleanup(); len(cleaned) != 0 {
		t.Fatalf("default mode reaped a 45s-idle conversation: %v", cleaned)
	}
	if _, ok := cm.data["conv-idle-45s"]; !ok {
		t.Fatal("conversation idle 45s must survive default-mode cleanup")
	}
}
