package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store's Path field is runtime state resolved from M365_API_KEYS. If it
// is ever serialized, a later load lets the stale embedded value override the
// env-var path — writes then land under the process CWD and a restart from a
// different directory can overwrite the live key set. Regression test for the
// bug that silently dropped API keys.
func TestAPIKeyStorePathIsNotSerialized(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "api-keys.json")
	s := newAPIKeyStore(p)
	if _, _, err := s.create("keepme"); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"Path"`) || strings.Contains(string(b), `"path"`) {
		t.Fatalf("serialized store leaked a Path field: %s", b)
	}
	var t2 apiKeyStore
	if err := json.Unmarshal(b, &t2); err != nil {
		t.Fatal(err)
	}
	if t2.Path != "" {
		t.Fatalf("unmarshal overrode Path with %q", t2.Path)
	}
}

// flush() keeps a .bak of the previous content whenever the payload changes.
func TestAPIKeyFlushKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "api-keys.json")
	s := newAPIKeyStore(p)
	if _, _, err := s.create("first"); err != nil {
		t.Fatal(err)
	}
	bak := p + ".bak"
	if fi, err := os.Stat(bak); err == nil && fi.Size() > 0 {
		t.Fatal("no .bak expected after first-ever write")
	}
	if _, _, err := s.create("second"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(bak)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("expected a non-empty %s after the second write, err=%v", bak, err)
	}
	// The backup must contain the earlier state, not the current one.
	var snap apiKeyStore
	b, _ := os.ReadFile(bak)
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("backup is not valid store json: %v", err)
	}
	if len(snap.Keys) != 1 || snap.Keys[0].Name != "first" {
		t.Fatalf("backup holds %d keys, want the pre-write snapshot", len(snap.Keys))
	}
}
