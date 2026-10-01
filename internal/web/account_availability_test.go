package web

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

// rejectionErr mirrors what internal/chathub returns for a non-Success result
// frame; the exact wording is the contract between the two packages.
func rejectionErr() error { return fmt.Errorf("upstream result error: ProcessingMessage") }

func onlineAccountJSON(id, email, reason string) string {
	disabled := ""
	if reason != "" {
		disabled = `,"scheduleDisabled":true,"scheduleDisabledBy":"` + reason + `"`
	}
	return fmt.Sprintf(`{"id":%q,"email":%q,"status":"online","accessToken":"live","refreshToken":"live-rt","expiresAt":%q%s}`,
		id, email, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), disabled)
}

// availabilitySettings builds a real settings store: markAccountResult and the
// upstream probe read settings, so a bare &Server{} would panic.
func availabilitySettings(t *testing.T) *settingsStore {
	t.Helper()
	return &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), v: defaultRuntimeSettings()}
}

// availabilityServer builds a Server with a real settings store.
func availabilityServer(t *testing.T, store *auth.Store) *Server {
	t.Helper()
	return &Server{
		tokens:      store,
		accountPool: newAccountHealth(),
		settings:    availabilitySettings(t),
	}
}

func TestUpstreamRejectionDisablesAccountOnlyAfterThreshold(t *testing.T) {
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "3")
	store := seedAccountsStore(t, `{"accounts":[`+onlineAccountJSON("acc-1", "a@example.com", "")+`]}`)
	s := availabilityServer(t, store)

	for i := 1; i < 3; i++ {
		s.markAccountResult("acc-1", rejectionErr())
		acc, _ := store.Get("acc-1")
		if acc.ScheduleDisabled {
			t.Fatalf("rejection %d must stay below the threshold: %+v", i, acc)
		}
	}
	s.markAccountResult("acc-1", rejectionErr())

	acc, _ := store.Get("acc-1")
	if !acc.ScheduleDisabled || acc.ScheduleDisabledBy != auth.ScheduleDisabledByUpstream {
		t.Fatalf("threshold reached, account must be out of rotation: %+v", acc)
	}
	if store.ScheduleEnabled("acc-1") {
		t.Fatal("disabled account must not be selectable")
	}
	if got := store.GatewayDisabled(); len(got) != 1 || got[0].ID != "acc-1" {
		t.Fatalf("GatewayDisabled=%v want acc-1", got)
	}
}

func TestTransientErrorsNeverReachTheRejectionThreshold(t *testing.T) {
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "2")
	store := seedAccountsStore(t, `{"accounts":[`+onlineAccountJSON("acc-2", "b@example.com", "")+`]}`)
	s := availabilityServer(t, store)

	// Transport/AAD failures are not upstream refusals: they must not disable.
	for i := 0; i < 6; i++ {
		s.markAccountResult("acc-2", errors.New("dial tcp 10.0.0.1:443: connect: connection refused"))
	}
	if acc, _ := store.Get("acc-2"); acc.ScheduleDisabled {
		t.Fatalf("transient errors must not disable an account: %+v", acc)
	}
	if got := s.upstreamRejectStreak("acc-2"); got != 0 {
		t.Fatalf("transient errors must not count as rejections, streak=%d", got)
	}
}

func TestSuccessfulRequestClearsTheRejectionStreak(t *testing.T) {
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "3")
	store := seedAccountsStore(t, `{"accounts":[`+onlineAccountJSON("acc-3", "c@example.com", "")+`]}`)
	s := availabilityServer(t, store)

	s.markAccountResult("acc-3", rejectionErr())
	s.markAccountResult("acc-3", rejectionErr())
	if got := s.upstreamRejectStreak("acc-3"); got != 2 {
		t.Fatalf("streak=%d want 2", got)
	}
	s.markAccountResult("acc-3", nil) // answered again
	if got := s.upstreamRejectStreak("acc-3"); got != 0 {
		t.Fatalf("a success must reset the streak, streak=%d", got)
	}
	s.markAccountResult("acc-3", rejectionErr())
	s.markAccountResult("acc-3", rejectionErr())
	if acc, _ := store.Get("acc-3"); acc.ScheduleDisabled {
		t.Fatal("two rejections after a success must not disable the account")
	}
}

func TestUpstreamProbeRestoresGatewayDisabledAccountOnly(t *testing.T) {
	store := seedAccountsStore(t, `{"accounts":[`+
		onlineAccountJSON("dead", "dead@example.com", auth.ScheduleDisabledByUpstream)+`,`+
		onlineAccountJSON("manual", "manual@example.com", auth.ScheduleDisabledByUser)+`]}`)
	var probed []string
	s := availabilityServer(t, store)
	s.upstreamProbe = func(a auth.AccountToken) error {
		probed = append(probed, a.ID)
		return nil
	}
	s.accountRejects = map[string]int{"dead": 3}

	s.recoverAccounts(10 * time.Minute)

	if len(probed) != 1 || probed[0] != "dead" {
		t.Fatalf("only the gateway-disabled account may be probed, probed=%v", probed)
	}
	acc, _ := store.Get("dead")
	if acc.ScheduleDisabled || acc.ScheduleDisabledBy != "" {
		t.Fatalf("a working account must return to rotation: %+v", acc)
	}
	if _, ok := store.Next(); !ok {
		t.Fatal("restored account must be selectable")
	}
	if got := s.upstreamRejectStreak("dead"); got != 0 {
		t.Fatalf("recovery must clear the streak, streak=%d", got)
	}
	s.accountRecoveryMtx.Lock()
	_, tracked := s.accountRecovery["dead"]
	s.accountRecoveryMtx.Unlock()
	if tracked {
		t.Fatal("recovery must clear the probe backoff state")
	}
	if manual, _ := store.Get("manual"); !manual.ScheduleDisabled || manual.ScheduleDisabledBy != auth.ScheduleDisabledByUser {
		t.Fatalf("a user disable must never be touched by the probe: %+v", manual)
	}
}

func TestUpstreamProbeKeepsFailingAccountOutWithBackoff(t *testing.T) {
	store := seedAccountsStore(t, `{"accounts":[`+
		onlineAccountJSON("dead", "dead@example.com", auth.ScheduleDisabledByUpstream)+`]}`)
	var calls int
	s := availabilityServer(t, store)
	s.upstreamProbe = func(a auth.AccountToken) error {
		calls++
		return rejectionErr()
	}

	s.recoverAccounts(10 * time.Minute)
	if calls != 1 {
		t.Fatalf("first round must probe once, calls=%d", calls)
	}
	if acc, _ := store.Get("dead"); !acc.ScheduleDisabled || acc.ScheduleDisabledBy != auth.ScheduleDisabledByUpstream {
		t.Fatalf("a still-broken account must stay out: %+v", acc)
	}
	s.recoverAccounts(10 * time.Minute)
	if calls != 1 {
		t.Fatalf("backoff must suppress the immediate retry, calls=%d", calls)
	}
	s.accountRecoveryMtx.Lock()
	s.accountRecovery["dead"].nextAt = time.Now().Add(-time.Second)
	s.accountRecoveryMtx.Unlock()
	s.recoverAccounts(10 * time.Minute)
	if calls != 2 {
		t.Fatalf("a due retry must probe again, calls=%d", calls)
	}
}

func TestAccountAvailableRejectsExpiredAndDisabledAccounts(t *testing.T) {
	store := seedAccountsStore(t, `{"accounts":[`+
		onlineAccountJSON("live", "live@example.com", "")+`,`+
		`{"id":"dead","email":"dead@example.com","status":"expired","accessToken":"stale","refreshToken":"rt","expiresAt":"2020-01-01T00:00:00Z"},`+
		onlineAccountJSON("manual", "manual@example.com", auth.ScheduleDisabledByUser)+`]}`)
	s := availabilityServer(t, store)

	if !s.accountAvailable("live") {
		t.Fatal("a healthy account must be available")
	}
	if s.accountAvailable("dead") {
		t.Fatal("an expired account must never be pinned: every request on it dies on the token refresh")
	}
	if s.accountAvailable("manual") {
		t.Fatal("a disabled account must not be available")
	}
	if s.accountAvailable("missing") {
		t.Fatal("an unknown account must not be available")
	}
}

func TestConversationPinDropsUnusableAccount(t *testing.T) {
	store := seedAccountsStore(t, `{"accounts":[`+
		onlineAccountJSON("live", "live@example.com", "")+`,`+
		`{"id":"dead","email":"dead@example.com","status":"expired","accessToken":"stale","refreshToken":"rt","expiresAt":"2020-01-01T00:00:00Z"}]}`)
	s := availabilityServer(t, store)
	s.sessionResolver = &sessionResolver{sessions: map[string]sessionBinding{
		"a": {SessionID: "a", ConversationID: "conv-live", AccountID: "live"},
		"b": {SessionID: "b", ConversationID: "conv-dead", AccountID: "dead"},
	}}

	if pinned, keep := s.pinConversationAccount("conv-live"); pinned != "live" || !keep {
		t.Fatalf("a usable binding must be pinned: pinned=%q keep=%v", pinned, keep)
	}
	// The cloud thread lives on an account that is gone. Pinning it would turn
	// the request into a 502, so the id must be dropped and the request allowed
	// to start a fresh conversation on a healthy account.
	if pinned, keep := s.pinConversationAccount("conv-dead"); pinned != "" || keep {
		t.Fatalf("an unusable binding must be dropped: pinned=%q keep=%v", pinned, keep)
	}
	if pinned, keep := s.pinConversationAccount("conv-unknown"); pinned != "" || !keep {
		t.Fatalf("an unknown conversation must be left alone: pinned=%q keep=%v", pinned, keep)
	}
}

func TestUpstreamRejectThresholdEnv(t *testing.T) {
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "")
	if got := accountRejectThreshold(); got != accountRejectThresholdDefault {
		t.Fatalf("unset must use the default, got %d", got)
	}
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "5")
	if got := accountRejectThreshold(); got != 5 {
		t.Fatalf("explicit value ignored, got %d", got)
	}
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "0")
	if got := accountRejectThreshold(); got != accountRejectThresholdDefault {
		t.Fatalf("zero must fall back to the default, got %d", got)
	}
	t.Setenv("M365_ACCOUNT_REJECT_THRESHOLD", "nonsense")
	if got := accountRejectThreshold(); got != accountRejectThresholdDefault {
		t.Fatalf("garbage must fall back to the default, got %d", got)
	}
}
