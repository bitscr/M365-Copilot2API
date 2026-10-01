package web

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
)

// Account recovery probe.
//
// An account is marked "expired" when a token refresh fails (auth.Store
// markExpiredLocked). Until this loop existed the only automatic retry was the
// one-shot RefreshExpiredTokens() at startup, so an account that failed once
// (proxy down, DNS blip, transient AAD error) stayed offline until the next
// restart or a manual "refresh" click, even though its refresh token was fine.
//
// This loop re-probes expired accounts that still hold a refresh token and lets
// a successful refresh restore them: Store.Upsert mints the fresh token, flips
// the status back to "online", and clears a scheduling disable that the GATEWAY
// applied (never one the user applied).
//
// The probe is a token refresh, not a chat request: it proves the credentials
// work without spending any M365 quota. Per-account exponential backoff keeps a
// permanently dead refresh token (AADSTS invalid_grant) from being hammered
// every round.

const (
	// accountRecoveryDefaultInterval is how often the repair loop touches a
	// gateway-disabled account. Deliberately slow: stage 2 spends a real (if
	// tiny) upstream request per account per round, and a bulk account that the
	// upstream refuses does not come back within minutes anyway. Override with
	// M365_ACCOUNT_RECOVER_INTERVAL (e.g. 30m) while debugging.
	accountRecoveryDefaultInterval = 12 * time.Hour
	// accountRecoveryMaxBackoff caps the per-account exponential backoff. It must
	// stay ABOVE the base interval, otherwise the cap would make retries more
	// frequent than the normal cadence.
	accountRecoveryMaxBackoff = 24 * time.Hour
)

type accountRecoveryState struct {
	attempts int
	nextAt   time.Time
}

// envFlag reports whether an env switch is enabled. Unset means enabled: these
// guards exist to turn a default-on behaviour OFF.
func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func accountRecoveryInterval() time.Duration {
	interval := accountRecoveryDefaultInterval
	if v := strings.TrimSpace(os.Getenv("M365_ACCOUNT_RECOVER_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return interval
}

func (s *Server) StartAccountRecovery() {
	if !envFlag("M365_ACCOUNT_AUTO_RECOVER") {
		log.Printf("[account-recovery] disabled via M365_ACCOUNT_AUTO_RECOVER")
		return
	}
	interval := accountRecoveryInterval()
	log.Printf("[account-recovery] enabled interval=%s max_backoff=%s (token refresh + real-request probe)", interval, accountRecoveryMaxBackoff)
	go func() {
		for {
			time.Sleep(interval)
			s.recoverAccounts(interval)
		}
	}()
}

// recoverAccounts is the single background repair loop for accounts the gateway
// took out of rotation. Two stages per account, because the two failure modes
// need different proof:
//
//  1. token refresh (free)  -> restores status=online
//  2. real one-word request -> restores ROTATION
//
// Stage 2 is what keeps the pool honest: the bulk accounts refresh their tokens
// happily and are still refused by the upstream, so a successful refresh is NOT
// evidence that an account works. Nothing else re-enables a gateway-disabled
// account (Store.Upsert deliberately no longer does), which is what stops the
// old refresh-ok -> re-enable -> refused -> disable flap.
func (s *Server) recoverAccounts(interval time.Duration) {
	if s == nil || s.tokens == nil {
		return
	}
	now := time.Now()
	candidates := append(s.tokens.ExpiredRefreshable(), s.tokens.GatewayDisabled()...)
	seen := make(map[string]bool, len(candidates))
	for _, a := range candidates {
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		if !s.recoveryDue(a.ID, now) {
			continue
		}
		acc, err := s.tokens.EnsureValid(a.ID)
		if err != nil {
			next := s.recoveryFailed(a.ID, interval, now)
			log.Printf("[account-recovery] account=%s token refresh still failing, next probe in %s: %v", a.Email, next.Sub(now).Truncate(time.Second), err)
			continue
		}
		if !acc.ScheduleDisabled {
			s.recoverySucceeded(a.ID)
			log.Printf("[account-recovery] account=%s recovered: token refreshed, back in rotation", a.Email)
			continue
		}
		if !auth.GatewayDisabledReason(acc.ScheduleDisabledBy) {
			// A user disable (or a legacy one with no reason) is never ours to
			// reverse; the token refresh above was the only useful work here.
			s.recoverySucceeded(a.ID)
			continue
		}
		if perr := s.probeAccountUpstream(acc); perr != nil {
			next := s.recoveryFailed(a.ID, interval, now)
			log.Printf("[account-availability] account=%s disabled by the gateway and still not serving, next probe in %s: %v", a.Email, next.Sub(now).Truncate(time.Second), perr)
			continue
		}
		s.clearUpstreamRejections(a.ID)
		if rerr := s.tokens.ReenableGatewayDisabled(a.ID); rerr != nil {
			log.Printf("[account-availability] account=%s answered a real request but re-enable failed: %v", a.Email, rerr)
			continue
		}
		s.recoverySucceeded(a.ID)
		log.Printf("[account-recovery] account=%s answered a real request, back in rotation", a.Email)
	}
}

func (s *Server) recoveryDue(id string, now time.Time) bool {
	s.accountRecoveryMtx.Lock()
	defer s.accountRecoveryMtx.Unlock()
	st, ok := s.accountRecovery[id]
	if !ok {
		return true
	}
	return !now.Before(st.nextAt)
}

func (s *Server) recoverySucceeded(id string) {
	s.accountRecoveryMtx.Lock()
	defer s.accountRecoveryMtx.Unlock()
	delete(s.accountRecovery, id)
}

// recoveryFailed books the next attempt for an account and returns its time.
// The wait doubles per consecutive failure and is capped, so a dead refresh
// token costs one request per maxBackoff instead of one per round.
func (s *Server) recoveryFailed(id string, interval time.Duration, now time.Time) time.Time {
	s.accountRecoveryMtx.Lock()
	defer s.accountRecoveryMtx.Unlock()
	if s.accountRecovery == nil {
		s.accountRecovery = map[string]*accountRecoveryState{}
	}
	st, ok := s.accountRecovery[id]
	if !ok {
		st = &accountRecoveryState{}
		s.accountRecovery[id] = st
	}
	st.attempts++
	wait := interval
	for i := 1; i < st.attempts && wait < accountRecoveryMaxBackoff; i++ {
		wait *= 2
	}
	if wait > accountRecoveryMaxBackoff {
		wait = accountRecoveryMaxBackoff
	}
	st.nextAt = now.Add(wait)
	return st.nextAt
}
