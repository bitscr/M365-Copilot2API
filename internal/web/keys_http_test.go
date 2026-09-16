package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newKeysTestServer builds a minimal Server whose admin session is already
// authenticated, so the /api/admin/keys handler can be exercised over HTTP
// without touching the real password store.
func newKeysTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := &Server{apiKeys: newTestKeyStore(t), adminSessions: map[string]time.Time{}}
	s.adminSessions["test-token"] = time.Now().Add(time.Hour)
	return s, "test-token"
}

func doKeys(t *testing.T, s *Server, token, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: token})
	rr := httptest.NewRecorder()
	s.adminKeys(rr, r)
	return rr
}

// The full secret must be returned by an explicit reveal request.
func TestAdminKeysRevealEndpoint(t *testing.T) {
	s, token := newKeysTestServer(t)
	rec, raw, err := s.apiKeys.create("endpoint")
	if err != nil {
		t.Fatal(err)
	}
	rr := doKeys(t, s, token, http.MethodGet, "/api/admin/keys?id="+rec.ID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["key"] != raw {
		t.Fatalf("reveal returned %v, want the full key", out["key"])
	}
}

// A legacy record yields 409 and a machine-readable code, not a fabricated key.
func TestAdminKeysRevealLegacyIsConflict(t *testing.T) {
	s, token := newKeysTestServer(t)
	s.apiKeys.Keys = append(s.apiKeys.Keys, apiKeyRecord{ID: "legacy", Prefix: "m365_legacy", Hash: keyHash("m365_legacy")})
	rr := doKeys(t, s, token, http.MethodGet, "/api/admin/keys?id=legacy", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not_recoverable") {
		t.Fatalf("body = %s, want not_recoverable code", rr.Body.String())
	}
}

// The list endpoint must never embed a secret.
func TestAdminKeysListOmitsSecretOverHTTP(t *testing.T) {
	s, token := newKeysTestServer(t)
	_, raw, err := s.apiKeys.create("endpoint")
	if err != nil {
		t.Fatal(err)
	}
	rr := doKeys(t, s, token, http.MethodGet, "/api/admin/keys", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), raw) {
		t.Fatal("list response leaked the full key")
	}
}

// Rotating through the PUT endpoint returns the new key and invalidates the old one.
func TestAdminKeysRotateEndpoint(t *testing.T) {
	s, token := newKeysTestServer(t)
	rec, oldKey, err := s.apiKeys.create("endpoint")
	if err != nil {
		t.Fatal(err)
	}
	rr := doKeys(t, s, token, http.MethodPut, "/api/admin/keys", `{"id":"`+rec.ID+`","key":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	newKey, _ := out["key"].(string)
	if newKey == "" || newKey == oldKey {
		t.Fatalf("expected a fresh key, got %q", newKey)
	}
	if !s.apiKeys.valid(newKey) || s.apiKeys.valid(oldKey) {
		t.Fatal("rotation must activate the new key and retire the old one")
	}
}

// An invalid custom key is rejected before anything is persisted.
func TestAdminKeysRotateRejectsMalformedCustomKey(t *testing.T) {
	s, token := newKeysTestServer(t)
	rec, _, err := s.apiKeys.create("endpoint")
	if err != nil {
		t.Fatal(err)
	}
	rr := doKeys(t, s, token, http.MethodPut, "/api/admin/keys", `{"id":"`+rec.ID+`","key":"not-a-valid-key"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// Renaming without a key in the body must not disturb the secret.
func TestAdminKeysRenameKeepsSecret(t *testing.T) {
	s, token := newKeysTestServer(t)
	rec, raw, err := s.apiKeys.create("old-name")
	if err != nil {
		t.Fatal(err)
	}
	rr := doKeys(t, s, token, http.MethodPut, "/api/admin/keys", `{"id":"`+rec.ID+`","name":"new-name"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !s.apiKeys.valid(raw) {
		t.Fatal("renaming must not invalidate the existing key")
	}
	if got, _, _ := s.apiKeys.reveal(rec.ID); got != raw {
		t.Fatalf("secret changed on rename: %q -> %q", raw, got)
	}
}
