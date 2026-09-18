package web

import (
	"path/filepath"
	"testing"
)

// sessionStore and sessionResolver must never share a file: they persist
// incompatible JSON shapes (object vs array) and would clobber each other.
func TestActiveSessionCachePathIsDistinctFromResolver(t *testing.T) {
	dir := t.TempDir()
	resolverPath := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", resolverPath)
	t.Setenv("M365_ACTIVE_SESSION_CACHE", "")

	got := activeSessionCachePath()
	if got == resolverPath {
		t.Fatalf("sessionStore still shares the resolver file: %s", got)
	}
	if filepath.Dir(got) != filepath.Dir(resolverPath) {
		t.Fatalf("derived path should live in the same data dir: %s", got)
	}
}

func TestActiveSessionCachePathExplicitOverride(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", "/data/sessions.json")
	t.Setenv("M365_ACTIVE_SESSION_CACHE", "/other/active.json")
	if got := activeSessionCachePath(); got != "/other/active.json" {
		t.Fatalf("explicit override ignored: %s", got)
	}
}

func TestActiveSessionCachePathDefault(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", "")
	t.Setenv("M365_ACTIVE_SESSION_CACHE", "")
	if got := activeSessionCachePath(); got != "active-sessions.json" {
		t.Fatalf("unexpected default: %s", got)
	}
}
