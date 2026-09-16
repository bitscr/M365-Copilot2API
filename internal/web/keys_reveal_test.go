package web

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestKeyStore(t *testing.T) *apiKeyStore {
	t.Helper()
	return newAPIKeyStore(filepath.Join(t.TempDir(), "api-keys.json"))
}

// A freshly created key must be revealable afterwards: the console shows it
// again on demand instead of only once at creation time.
func TestCreatedKeyIsRevealable(t *testing.T) {
	s := newTestKeyStore(t)
	rec, raw, err := s.create("default")
	if err != nil {
		t.Fatal(err)
	}
	got, found, recoverable := s.reveal(rec.ID)
	if !found || !recoverable {
		t.Fatalf("reveal found=%v recoverable=%v", found, recoverable)
	}
	if got != raw {
		t.Fatalf("revealed %q, want the created key %q", got, raw)
	}
	if !s.valid(got) {
		t.Fatal("revealed key must authenticate")
	}
}

// list() must never carry the secret, so a dashboard refresh does not leak it.
func TestListOmitsSecrets(t *testing.T) {
	s := newTestKeyStore(t)
	rec, raw, err := s.create("default")
	if err != nil {
		t.Fatal(err)
	}
	_ = rec
	for _, k := range s.list() {
		if k.Raw != "" {
			t.Fatalf("list leaked raw key %q", k.Raw)
		}
		if k.Hash != "" {
			t.Fatalf("list leaked hash %q", k.Hash)
		}
		if strings.Contains(k.Prefix, raw) {
			t.Fatal("prefix must be a truncation, not the whole key")
		}
	}
}

// Rotating with an empty value generates a new random key and invalidates the old one.
func TestSetRawGeneratesNewKeyAndInvalidatesOld(t *testing.T) {
	s := newTestKeyStore(t)
	rec, oldKey, err := s.create("default")
	if err != nil {
		t.Fatal(err)
	}
	_, newKey, err := s.setRaw(rec.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if newKey == oldKey {
		t.Fatal("rotation returned the same key")
	}
	if !s.valid(newKey) {
		t.Fatal("new key must authenticate")
	}
	if s.valid(oldKey) {
		t.Fatal("old key must stop authenticating after rotation")
	}
	if got, _, _ := s.reveal(rec.ID); got != newKey {
		t.Fatalf("reveal = %q, want rotated key %q", got, newKey)
	}
}

// A caller-supplied key is stored verbatim and must authenticate.
func TestSetRawAcceptsCustomKey(t *testing.T) {
	s := newTestKeyStore(t)
	rec, _, err := s.create("default")
	if err != nil {
		t.Fatal(err)
	}
	custom := "m365_" + strings.Repeat("ab", 20)
	_, got, err := s.setRaw(rec.ID, custom)
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Fatalf("setRaw returned %q, want %q", got, custom)
	}
	if !s.valid(custom) {
		t.Fatal("custom key must authenticate")
	}
}

func TestSetRawUnknownID(t *testing.T) {
	s := newTestKeyStore(t)
	if _, _, err := s.setRaw("nope", ""); err != errKeyNotFound {
		t.Fatalf("err = %v, want errKeyNotFound", err)
	}
}

// Revealing a record that predates raw storage reports unrecoverable rather
// than fabricating a value.
func TestRevealLegacyRecordIsNotRecoverable(t *testing.T) {
	s := newTestKeyStore(t)
	s.Keys = append(s.Keys, apiKeyRecord{ID: "legacy", Prefix: "m365_old", Hash: keyHash("m365_old")})
	if _, found, recoverable := s.reveal("legacy"); !found || recoverable {
		t.Fatalf("found=%v recoverable=%v, want found and not recoverable", found, recoverable)
	}
}

func TestValidateCustomKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"generated shape", "m365_" + strings.Repeat("a1", 32), false},
		{"missing prefix", strings.Repeat("a1", 32), true},
		{"too short", "m365_" + strings.Repeat("a1", 8), true},
		{"non-hex body", "m365_" + strings.Repeat("zz", 20), true},
		{"too long", "m365_" + strings.Repeat("a1", 80), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCustomKey(tc.key); (err != nil) != tc.wantErr {
				t.Fatalf("validateCustomKey(%q) err = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
		})
	}
}
