package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

// recoveryEndpointStub mimics the AAD token endpoint: it fails while healthy is
// false and mints a fresh token once it is true, counting every request.
func recoveryEndpointStub(healthy *atomic.Bool, calls *int32) *httptest.Server {
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

func seedAccountsStore(t *testing.T, accountsJSON string) *auth.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte(accountsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRecoverAccountsNeedsARealRequestToRestoreRotation(t *testing.T) {
	var healthy atomic.Bool
	var calls int32
	srv := recoveryEndpointStub(&healthy, &calls)
	defer srv.Close()
	t.Setenv("M365_TOKEN_ENDPOINT", srv.URL)

	store := seedAccountsStore(t, `{"accounts":[{"id":"exp-1","email":"exp@example.com","status":"expired","accessToken":"stale","refreshToken":"dead","expiresAt":"2020-01-01T00:00:00Z","scheduleDisabled":true,"scheduleDisabledBy":"auto"}]}`)
	var probes int
	probeErr := error(nil)
	s := &Server{tokens: store, accountPool: newAccountHealth(), settings: availabilitySettings(t)}
	s.upstreamProbe = func(a auth.AccountToken) error {
		probes++
		return probeErr
	}
	interval := 10 * time.Minute

	s.recoverAccounts(interval)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("first round must try the token refresh once, calls=%d", got)
	}
	if probes != 0 {
		t.Fatalf("no real request may be spent while the token is still dead, probes=%d", probes)
	}
	s.accountRecoveryMtx.Lock()
	st := s.accountRecovery["exp-1"]
	attempts, nextAt := 0, time.Time{}
	if st != nil {
		attempts, nextAt = st.attempts, st.nextAt
	}
	s.accountRecoveryMtx.Unlock()
	if attempts != 1 || !nextAt.After(time.Now()) {
		t.Fatalf("failed probe must book a future retry: attempts=%d nextAt=%s", attempts, nextAt)
	}

	// Still inside the backoff window: the next round must not touch the network.
	s.recoverAccounts(interval)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("backoff must suppress the retry, calls=%d", got)
	}

	// Backoff elapses and the refresh token starts working again. That alone is
	// NOT enough to restore rotation: the upstream still refuses the account.
	s.accountRecoveryMtx.Lock()
	s.accountRecovery["exp-1"].nextAt = time.Now().Add(-time.Second)
	s.accountRecoveryMtx.Unlock()
	healthy.Store(true)
	probeErr = rejectionErr()
	s.recoverAccounts(interval)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("due retry must refresh again, calls=%d", got)
	}
	if probes != 1 {
		t.Fatalf("a refreshed gateway-disabled account must be tested with a real request, probes=%d", probes)
	}
	acc, _ := store.Get("exp-1")
	if acc.Status != "online" {
		t.Fatalf("a successful refresh restores the credential, status=%q", acc.Status)
	}
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != auth.ScheduleDisabledByAuto {
		t.Fatalf("a refresh alone must NOT put the account back in rotation: %+v", acc)
	}
	if store.ScheduleEnabled("exp-1") {
		t.Fatal("refused account must stay out of rotation")
	}

	// Only a real request that succeeds restores rotation.
	probeErr = nil
	s.accountRecoveryMtx.Lock()
	s.accountRecovery["exp-1"].nextAt = time.Now().Add(-time.Second)
	s.accountRecoveryMtx.Unlock()
	s.recoverAccounts(interval)
	if probes != 2 {
		t.Fatalf("due probe must run again, probes=%d", probes)
	}
	acc, _ = store.Get("exp-1")
	if acc.ScheduleDisabled || acc.ScheduleDisabledBy != "" {
		t.Fatalf("a serving account must be back in rotation: %+v", acc)
	}
	if _, ok := store.Next(); !ok {
		t.Fatal("restored account must be selectable for new requests")
	}
	s.accountRecoveryMtx.Lock()
	_, stillTracked := s.accountRecovery["exp-1"]
	s.accountRecoveryMtx.Unlock()
	if stillTracked {
		t.Fatal("successful recovery must clear the backoff state")
	}
}

func TestRecoverAccountsSkipsUnprobeableAndRespectsUserDisable(t *testing.T) {
	// no-rt   : expired with no refresh token, nothing can revive it
	// manual  : expired but the USER disabled scheduling, so recovery must not re-enable it
	store := seedAccountsStore(t, `{"accounts":[
		{"id":"no-rt","email":"n@example.com","status":"expired","accessToken":"stale","expiresAt":"2020-01-01T00:00:00Z"},
		{"id":"manual","email":"m@example.com","status":"expired","accessToken":"stale","refreshToken":"dead","expiresAt":"2020-01-01T00:00:00Z","scheduleDisabled":true,"scheduleDisabledBy":"user"}]}`)
	var healthy atomic.Bool
	healthy.Store(true)
	var calls int32
	srv := recoveryEndpointStub(&healthy, &calls)
	defer srv.Close()
	t.Setenv("M365_TOKEN_ENDPOINT", srv.URL)

	s := &Server{tokens: store, accountPool: newAccountHealth()}
	s.recoverAccounts(10 * time.Minute)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("only the refreshable account may be probed, calls=%d", got)
	}
	if acc, _ := store.Get("no-rt"); acc.Status != "expired" {
		t.Fatalf("account without a refresh token must stay expired, got %q", acc.Status)
	}
	acc, _ := store.Get("manual")
	if acc.Status != "online" {
		t.Fatalf("successful probe must restore online, got %q", acc.Status)
	}
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != auth.ScheduleDisabledByUser {
		t.Fatalf("a user-initiated disable must survive recovery: %+v", acc)
	}
	if store.ScheduleEnabled("manual") {
		t.Fatal("user-disabled account must stay out of rotation after recovery")
	}
}

func TestAccountRecoveryEnvSwitches(t *testing.T) {
	t.Setenv("M365_ACCOUNT_AUTO_RECOVER", "off")
	if envFlag("M365_ACCOUNT_AUTO_RECOVER") {
		t.Fatal("falsey value must disable the loop")
	}
	t.Setenv("M365_ACCOUNT_AUTO_RECOVER", "")
	if !envFlag("M365_ACCOUNT_AUTO_RECOVER") {
		t.Fatal("unset must mean enabled")
	}
	t.Setenv("M365_ACCOUNT_RECOVER_INTERVAL", "90s")
	if got := accountRecoveryInterval(); got != 90*time.Second {
		t.Fatalf("interval=%s want 1m30s", got)
	}
	t.Setenv("M365_ACCOUNT_RECOVER_INTERVAL", "5")
	if got := accountRecoveryInterval(); got != 5*time.Minute {
		t.Fatalf("bare number must mean minutes, got %s", got)
	}
	t.Setenv("M365_ACCOUNT_RECOVER_INTERVAL", "nonsense")
	if got := accountRecoveryInterval(); got != accountRecoveryDefaultInterval {
		t.Fatalf("invalid value must fall back to the default, got %s", got)
	}
	t.Setenv("M365_ACCOUNT_RECOVER_INTERVAL", "12h")
	if got := accountRecoveryInterval(); got != 12*time.Hour {
		t.Fatalf("hour units must parse, got %s", got)
	}
	if accountRecoveryDefaultInterval != 12*time.Hour {
		t.Fatalf("the default probe cadence is 12h, got %s", accountRecoveryDefaultInterval)
	}
	if accountRecoveryMaxBackoff <= accountRecoveryDefaultInterval {
		t.Fatalf("the backoff cap (%s) must stay above the base interval (%s), otherwise the cap retries MORE often than the cadence", accountRecoveryMaxBackoff, accountRecoveryDefaultInterval)
	}
}
