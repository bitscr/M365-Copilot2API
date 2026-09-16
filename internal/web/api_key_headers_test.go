package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Agent clients disagree about which header carries the API key. Codex CLI and
// CC Switch send "api-key"; the OpenAI SDKs send "Authorization: Bearer"; the
// web console sends "X-API-Key". Accepting only two of the three made Codex
// fail with 401 while the key was valid.
func TestPresentedAPIKeyAcceptsAgentHeaderForms(t *testing.T) {
	const key = "m365_deadbeef"
	cases := []struct {
		name    string
		headers map[string]string
		want    string
		wantOK  bool
	}{
		{"api-key (Codex CLI, CC Switch)", map[string]string{"api-key": key}, key, true},
		{"openai-api-key (some Codex builds)", map[string]string{"openai-api-key": key}, key, true},
		{"OpenAI-Api-Key (canonical casing)", map[string]string{"OpenAI-Api-Key": key}, key, true},
		{"Api-Key (canonical MIME form)", map[string]string{"Api-Key": key}, key, true},
		{"X-API-Key (web console)", map[string]string{"X-API-Key": key}, key, true},
		{"Authorization Bearer (OpenAI SDK)", map[string]string{"Authorization": "Bearer " + key}, key, true},
		{"lowercase bearer scheme", map[string]string{"Authorization": "bearer " + key}, key, true},
		{"no credentials", map[string]string{}, "", false},
		{"non-bearer Authorization is ignored", map[string]string{"Authorization": "Basic " + key}, "", false},
		{"blank api-key falls through", map[string]string{"api-key": "   "}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got, ok := presentedAPIKey(r)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("presentedAPIKey = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// rawAPIKey must return the complete secret, not a truncated prefix, because
// callers use it as a tenant isolation identity.
func TestRawAPIKeyIsNotTruncated(t *testing.T) {
	const key = "m365_0123456789abcdef0123456789abcdef"
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("api-key", key)
	if got := rawAPIKey(r); got != key {
		t.Fatalf("rawAPIKey = %q, want the full key", got)
	}
}

// extractAPIKey feeds usage records, where only the prefix is shown.
func TestExtractAPIKeyTruncatesForDisplay(t *testing.T) {
	const key = "m365_0123456789abcdef0123456789abcdef"
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("api-key", key)
	if got, want := extractAPIKey(r), key[:8]+"..."; got != want {
		t.Fatalf("extractAPIKey = %q, want %q", got, want)
	}
}
