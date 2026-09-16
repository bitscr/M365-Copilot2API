package web

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// errKeyNotFound is returned by mutators when the requested record is gone.
var errKeyNotFound = errors.New("key not found")

// publicKeyPrefix is the display prefix shown in the console (m365_ + 6 hex).
// It stays stable for the same secret so the UI can match a row to a key.
func publicKeyPrefix(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) <= 12 {
		return raw
	}
	return raw[:12]
}

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"hash"`
	Raw        string     `json:"raw,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type apiKeyStore struct {
	mu   sync.Mutex
	Keys []apiKeyRecord `json:"keys"`
	// Path is runtime state resolved from M365_API_KEYS at startup. It must
	// NEVER be serialized: a previously embedded relative value used to
	// override the env-var path on load, so flushes landed in whatever
	// directory the process happened to run in. That divergence let one
	// stale snapshot overwrite the live key set at restart.
	Path string `json:"-"`

	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-copilot2api", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		// Repair records written before hashing existed: derive the hash from
		// the stored plaintext. Raw is KEPT so the console can reveal the key
		// later; the file is already mode 0600 and the same directory holds
		// OAuth tokens.
		migrated := false
		for i := range s.Keys {
			if s.Keys[i].Hash == "" && s.Keys[i].Raw != "" {
				s.Keys[i].Hash = keyHash(s.Keys[i].Raw)
				s.Keys[i].Prefix = publicKeyPrefix(s.Keys[i].Raw)
				migrated = true
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	// Keep one snapshot of the previous content: an accidental bad write
	// (stale process, wrong path) stays one rename away from being undone.
	if old, e := os.ReadFile(s.Path); e == nil && !bytes.Equal(old, b) {
		_ = writeFileAtomic(s.Path+".bak", old, 0600)
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }

// validateCustomKey enforces the shape of a caller-supplied replacement key.
// A custom value must keep the m365_ namespace and stay hex so keys remain
// interchangeable with generated ones and never collide on a display prefix.
func validateCustomKey(raw string) error {
	if !strings.HasPrefix(raw, "m365_") {
		return errors.New("key must start with m365_")
	}
	body := raw[len("m365_"):]
	if len(body) < 32 {
		return errors.New("key body must be at least 32 hex characters")
	}
	if len(body) > 128 {
		return errors.New("key body must be at most 128 hex characters")
	}
	if _, err := hex.DecodeString(body); err != nil {
		return errors.New("key body must be hexadecimal")
	}
	return nil
}
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	// Raw is persisted so the console can reveal the key after creation. The
	// store file is mode 0600 and already holds OAuth refresh tokens.
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: publicKeyPrefix(raw), Hash: keyHash(raw), Raw: raw, CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		s.Keys = s.Keys[:len(s.Keys)-1]
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	r.Hash = ""
	r.Raw = ""
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		// Raw is never included in list responses: the console asks for it
		// explicitly via reveal() so the full secret does not ride along in
		// every dashboard refresh (browser history, proxy logs, devtools).
		out[i].Raw = ""
		out[i].Hash = ""
	}
	return out
}

// reveal returns the full plaintext key for an active or disabled record.
// Keys created before raw storage was added have no recoverable value; the
// caller must rotate those instead (ok=false, recoverable=false).
func (s *apiKeyStore) reveal(id string) (raw string, found, recoverable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		if s.Keys[i].Raw == "" {
			return "", true, false
		}
		return s.Keys[i].Raw, true, true
	}
	return "", false, false
}

// setRaw replaces the key material for a record, preserving its ID and
// creation time. Passing an empty custom value generates a fresh random key.
// The caller is responsible for validating a caller-supplied value.
func (s *apiKeyStore) setRaw(id, custom string) (apiKeyRecord, string, error) {
	raw := strings.TrimSpace(custom)
	if raw == "" {
		b := make([]byte, 32)
		if _, e := rand.Read(b); e != nil {
			return apiKeyRecord{}, "", e
		}
		raw = "m365_" + hex.EncodeToString(b)
	}
	s.mu.Lock()
	idx := -1
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return apiKeyRecord{}, "", errKeyNotFound
	}
	oldRaw, oldPrefix, oldHash := s.Keys[idx].Raw, s.Keys[idx].Prefix, s.Keys[idx].Hash
	s.Keys[idx].Raw = raw
	s.Keys[idx].Prefix = publicKeyPrefix(raw)
	s.Keys[idx].Hash = keyHash(raw)
	rec := s.Keys[idx]
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Raw, s.Keys[i].Prefix, s.Keys[i].Hash = oldRaw, oldPrefix, oldHash
				break
			}
		}
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	rec.Raw = ""
	rec.Hash = ""
	return rec, raw, nil
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			s.mu.Unlock()
			if err := s.persist.flushNowBlocking(); err != nil {
				s.mu.Lock()
				s.Keys[i].Revoked = false
				s.mu.Unlock()
				return false, err
			}
			return true, nil
		}
	}
	s.mu.Unlock()
	return false, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		removed := s.Keys[i]
		s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
		s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = append(s.Keys[:i], append([]apiKeyRecord{removed}, s.Keys[i:]...)...)
			s.mu.Unlock()
			return false, err
		}
		return true, nil
	}
	s.mu.Unlock()
	return false, nil
}

func (s *apiKeyStore) update(id, name string, revoked *bool) (bool, error) {
	s.mu.Lock()
	found := false
	var oldName string
	var oldRevoked bool
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		oldName = s.Keys[i].Name
		oldRevoked = s.Keys[i].Revoked
		if name != "" {
			s.Keys[i].Name = name
		}
		if revoked != nil {
			s.Keys[i].Revoked = *revoked
		}
		found = true
		break
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Name = oldName
				s.Keys[i].Revoked = oldRevoked
				break
			}
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}
func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	h := keyHash(raw)
	found := false
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			found = true
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.persist.markDirty()
	}
	return found
}
