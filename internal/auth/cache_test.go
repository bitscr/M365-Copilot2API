package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// tokenEndpointStub returns a token endpoint that fails while healthy is false
// and mints a fresh token afterwards. It counts the requests it receives.
func tokenEndpointStub(healthy *atomic.Bool, calls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if !healthy.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token is dead"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
}

func TestUpsertAndList(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken:  "a",
		RefreshToken: "r",
		Email:        "a@example.com",
		DisplayName:  "A",
		HomeOID:      "oid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Email != "a@example.com" {
		t.Fatalf("unexpected email: %s", acc.Email)
	}
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
}

func TestDeleteAccountsRemovesBatchAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, oid := range []string{"oid-1", "oid-2", "oid-3"} {
		if _, err := store.Upsert(TokenSet{AccessToken: "a", RefreshToken: "r", Email: oid + "@example.com", HomeOID: oid, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DeleteAccounts(nil); err == nil {
		t.Fatal("empty id list must be rejected")
	}
	if _, err := store.DeleteAccounts([]string{"  ", ""}); err == nil {
		t.Fatal("blank ids must be rejected")
	}
	n, err := store.DeleteAccounts([]string{"oid-1", "oid-1", " missing ", "oid-2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted=%d want 2 (duplicates and unknown ids ignored)", n)
	}
	if _, ok := store.Get("oid-3"); !ok {
		t.Fatal("unselected account must survive")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	list := reopened.List()
	if len(list) != 1 || list[0].ID != "oid-3" {
		t.Fatalf("deletion was not persisted, list=%v", list)
	}
}

func TestFailedRefreshTakesAccountOutOfRotation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_DISABLE", "")
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Expired access token and NO refresh token: the account can never be
	// refreshed, so it must be marked expired and pulled out of rotation.
	if _, err := store.Upsert(TokenSet{AccessToken: "stale", Email: "gone@example.com", HomeOID: "oid-gone", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureValid("oid-gone"); err == nil {
		t.Fatal("account without a refresh token must fail validation")
	}
	acc, _ := store.Get("oid-gone")
	if acc.Status != "expired" {
		t.Fatalf("status=%q want expired", acc.Status)
	}
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByAuto {
		t.Fatalf("expired account must be auto-disabled: disabled=%v by=%q", acc.ScheduleDisabled, acc.ScheduleDisabledBy)
	}
	if store.ScheduleEnabled("oid-gone") {
		t.Fatal("expired account must report scheduling disabled")
	}
	if _, ok := store.Next(); ok {
		t.Fatal("expired account must not be drawn for new requests")
	}
	// It holds no refresh token, so no probe can revive it.
	if got := store.ExpiredRefreshable(); len(got) != 0 {
		t.Fatalf("ExpiredRefreshable=%v want empty (no refresh token to probe)", got)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if acc, _ := reopened.Get("oid-gone"); !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByAuto {
		t.Fatalf("auto-disable was not persisted: %+v", acc)
	}
}

func TestUserDisableIsNeverUndoneByReauth(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_DISABLE", "")
	store, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok := TokenSet{AccessToken: "a", RefreshToken: "r", Email: "user@example.com", HomeOID: "oid-u", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := store.Upsert(tok); err != nil {
		t.Fatal(err)
	}
	if err := store.SetScheduleEnabled("oid-u", false); err != nil {
		t.Fatal(err)
	}
	if acc, _ := store.Get("oid-u"); acc.ScheduleDisabledBy != ScheduleDisabledByUser {
		t.Fatalf("console disable must be tagged user, got %q", acc.ScheduleDisabledBy)
	}
	// A later successful sign-in must NOT re-enable what the user turned off.
	if _, err := store.Upsert(TokenSet{AccessToken: "a2", RefreshToken: "r2", Email: "user@example.com", HomeOID: "oid-u", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	acc, _ := store.Get("oid-u")
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByUser {
		t.Fatalf("user disable was overridden: %+v", acc)
	}
}

func TestSuccessfulRefreshAloneDoesNotRestoreRotation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_DISABLE", "")
	var healthy atomic.Bool
	var calls int32
	srv := tokenEndpointStub(&healthy, &calls)
	defer srv.Close()
	t.Setenv("M365_TOKEN_ENDPOINT", srv.URL)

	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{AccessToken: "stale", RefreshToken: "dead", Email: "e@example.com", HomeOID: "oid-e", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureValid("oid-e"); err == nil {
		t.Fatal("dead refresh token must fail validation")
	}
	acc, _ := store.Get("oid-e")
	if acc.Status != "expired" || !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByAuto {
		t.Fatalf("after failed refresh: %+v", acc)
	}
	if got := store.ExpiredRefreshable(); len(got) != 1 || got[0].ID != "oid-e" {
		t.Fatalf("ExpiredRefreshable=%v want the expired account that still holds a refresh token", got)
	}
	if got := store.GatewayDisabled(); len(got) != 1 || got[0].ID != "oid-e" {
		t.Fatalf("GatewayDisabled=%v want the account the gateway disabled", got)
	}

	// A successful refresh restores the CREDENTIAL only. The account is still
	// refused by the upstream until a real request proves otherwise, so it must
	// stay out of rotation (this is what stops the re-enable/refuse flap).
	healthy.Store(true)
	refreshed, err := store.EnsureValid("oid-e")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.Status != "online" || refreshed.AccessToken != "fresh-access" {
		t.Fatalf("refreshed=%+v want online with the fresh token", refreshed)
	}
	acc, _ = store.Get("oid-e")
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByAuto {
		t.Fatalf("a refresh alone must NOT restore rotation: %+v", acc)
	}
	if store.ScheduleEnabled("oid-e") {
		t.Fatal("refreshed account must stay out of rotation until a real request succeeds")
	}
	if got := store.GatewayDisabled(); len(got) != 1 || got[0].ID != "oid-e" {
		t.Fatalf("GatewayDisabled=%v want the account still awaiting a real-request probe", got)
	}
	if len(store.ExpiredRefreshable()) != 0 {
		t.Fatal("refreshed account must leave the token-probe list")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if acc, _ := reopened.Get("oid-e"); acc.Status != "online" {
		t.Fatalf("refresh was not persisted: %+v", acc)
	}
}

// TestUpsertPreservesAGatewayDisable is the anti-flap invariant: minting a fresh
// token must NOT put a gateway-disabled account back into rotation, for EITHER
// gateway reason. Only a real request that succeeds may (Server.recoverAccounts).
func TestUpsertPreservesAGatewayDisable(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_DISABLE", "")
	path := filepath.Join(t.TempDir(), "tokens.json")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	seed := `{"accounts":[
		{"id":"oid-auto","email":"a@example.com","status":"online","accessToken":"live","refreshToken":"live-rt","expiresAt":"` + expires + `","scheduleDisabled":true,"scheduleDisabledBy":"auto"},
		{"id":"oid-up","email":"u@example.com","status":"online","accessToken":"live","refreshToken":"live-rt","expiresAt":"` + expires + `","scheduleDisabled":true,"scheduleDisabledBy":"auto-upstream"}]}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh sign-in / successful refresh lands here with a brand-new token.
	for _, oid := range []string{"oid-auto", "oid-up"} {
		if _, err := store.Upsert(TokenSet{AccessToken: "brand-new", RefreshToken: "brand-new-rt", Email: oid + "@example.com", HomeOID: oid, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		acc, _ := store.Get(oid)
		if !acc.ScheduleDisabled || !GatewayDisabledReason(acc.ScheduleDisabledBy) {
			t.Fatalf("%s: a new token must not clear a gateway disable: %+v", oid, acc)
		}
		if store.ScheduleEnabled(oid) {
			t.Fatalf("%s: gateway-disabled account must stay out of rotation after a refresh", oid)
		}
	}
	if got := store.GatewayDisabled(); len(got) != 2 {
		t.Fatalf("GatewayDisabled=%v want both gateway-disabled accounts", got)
	}

	// Only an explicit gateway re-enable (after a real request succeeded) does.
	if err := store.ReenableGatewayDisabled("oid-auto"); err != nil {
		t.Fatal(err)
	}
	if acc, _ := store.Get("oid-auto"); acc.ScheduleDisabled {
		t.Fatalf("an explicit gateway re-enable must work: %+v", acc)
	}
	if got := store.GatewayDisabled(); len(got) != 1 || got[0].ID != "oid-up" {
		t.Fatalf("GatewayDisabled=%v want only oid-up left", got)
	}
}

func TestLegacyDisableIsNeverAutoReEnabled(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_DISABLE", "")
	var healthy atomic.Bool
	var calls int32
	srv := tokenEndpointStub(&healthy, &calls)
	defer srv.Close()
	t.Setenv("M365_TOKEN_ENDPOINT", srv.URL)

	// A record disabled BEFORE this feature existed: scheduleDisabled is set but
	// no reason is recorded. Its origin is unknowable, so the only safe reading
	// is "treat it as the user's choice" — it must never be re-tagged as an
	// automatic disable, and never be switched back on.
	path := filepath.Join(t.TempDir(), "tokens.json")
	seed := `{"accounts":[{"id":"legacy-1","email":"legacy@example.com","status":"expired","accessToken":"stale","refreshToken":"dead","expiresAt":"2020-01-01T00:00:00Z","scheduleDisabled":true}]}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// First a FAILED refresh, so the expiry path runs over an already-disabled
	// record; then a successful one, which must still not re-enable it.
	if _, err := store.EnsureValid("legacy-1"); err == nil {
		t.Fatal("dead refresh token must fail validation")
	}
	if acc, _ := store.Get("legacy-1"); acc.ScheduleDisabledBy != "" {
		t.Fatalf("an already-disabled account must not be re-tagged auto: %+v", acc)
	}
	healthy.Store(true)
	if _, err := store.EnsureValid("legacy-1"); err != nil {
		t.Fatalf("refresh should succeed: %v", err)
	}
	acc, _ := store.Get("legacy-1")
	if acc.Status != "online" {
		t.Fatalf("status=%q want online", acc.Status)
	}
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != "" {
		t.Fatalf("a disable with no recorded reason must survive recovery untouched: %+v", acc)
	}
	if store.ScheduleEnabled("legacy-1") {
		t.Fatal("legacy-disabled account must stay out of rotation")
	}
}

func TestScheduleEnabledPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := TokenSet{AccessToken: "a", RefreshToken: "r", Email: "a@example.com", HomeOID: "oid-1", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if !store.ScheduleEnabled("oid-1") {
		t.Fatal("new account scheduling disabled")
	}
	if err := store.SetScheduleEnabled("oid-1", false); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("account scheduling still enabled")
	}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("upsert reset scheduling state")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("scheduling state was not persisted")
	}
}

func TestUpstreamDisableIsSeparateFromTokenDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err := store.Upsert(TokenSet{AccessToken: "live", RefreshToken: "live-rt", Email: "u@example.com", HomeOID: "oid-u", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{AccessToken: "live2", RefreshToken: "live-rt2", Email: "m@example.com", HomeOID: "oid-m", ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetScheduleEnabled("oid-m", false); err != nil {
		t.Fatal(err)
	}

	if err := store.DisableScheduleUpstream("oid-u"); err != nil {
		t.Fatal(err)
	}
	acc, _ := store.Get("oid-u")
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByUpstream {
		t.Fatalf("upstream disable must carry its own reason: %+v", acc)
	}
	if got := store.GatewayDisabled(); len(got) != 1 || got[0].ID != "oid-u" {
		t.Fatalf("GatewayDisabled=%v want only oid-u", got)
	}

	// These accounts refresh fine; a refresh must NOT put them back in rotation.
	if _, err := store.EnsureValid("oid-u"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	acc, _ = store.Get("oid-u")
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByUpstream {
		t.Fatalf("a successful token refresh must not clear an upstream disable: %+v", acc)
	}
	if store.ScheduleEnabled("oid-u") {
		t.Fatal("upstream-disabled account must stay out of rotation")
	}

	// Re-enabling only ever reverses the upstream reason.
	if err := store.ReenableGatewayDisabled("oid-m"); err != nil {
		t.Fatal(err)
	}
	if manual, _ := store.Get("oid-m"); !manual.ScheduleDisabled || manual.ScheduleDisabledBy != ScheduleDisabledByUser {
		t.Fatalf("ReenableGatewayDisabled must not touch a user disable: %+v", manual)
	}
	if err := store.ReenableGatewayDisabled("oid-u"); err != nil {
		t.Fatal(err)
	}
	acc, _ = store.Get("oid-u")
	if acc.ScheduleDisabled || acc.ScheduleDisabledBy != "" {
		t.Fatalf("ReenableGatewayDisabled must clear the upstream disable: %+v", acc)
	}
	if _, ok := store.Next(); !ok {
		t.Fatal("restored account must be selectable again")
	}

	// The reason is persisted, not in-memory state: it survives a reopen.
	if err := store.DisableScheduleUpstream("oid-u"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if acc, _ := reopened.Get("oid-u"); !acc.ScheduleDisabled || acc.ScheduleDisabledBy != ScheduleDisabledByUpstream {
		t.Fatalf("upstream disable must persist: %+v", acc)
	}
}

func TestGatewayDisabledListIgnoresOtherReasons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	for _, ts := range []TokenSet{
		{AccessToken: "a", RefreshToken: "a-rt", Email: "a@example.com", HomeOID: "a", ExpiresAt: expires},
		{AccessToken: "b", RefreshToken: "b-rt", Email: "b@example.com", HomeOID: "b", ExpiresAt: expires},
		{AccessToken: "c", RefreshToken: "c-rt", Email: "c@example.com", HomeOID: "c", ExpiresAt: expires},
	} {
		if _, err := store.Upsert(ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetScheduleEnabled("b", false); err != nil { // user choice
		t.Fatal(err)
	}
	if err := store.DisableScheduleUpstream("c"); err != nil {
		t.Fatal(err)
	}
	got := store.GatewayDisabled()
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("GatewayDisabled=%v want only c (a is enabled, b is a user disable)", got)
	}
}
