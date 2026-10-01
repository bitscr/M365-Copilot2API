package web

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

// Upstream-availability guard.
//
// A token that refreshes does not prove the account can answer. The M365
// substrate accepts the WebSocket handshake, streams a first delta and then
// closes with a non-Success result frame (observed: "ProcessingMessage"); the
// gateway relays that as a 502. accountHealth only books a 30s cooldown for it,
// so a dead account keeps coming back to the rotation and burns one request
// every half minute, forever, while the console happily shows it as Online.
//
// This guard counts CONSECUTIVE upstream rejections per account. Past the
// threshold the account is taken out of rotation with
// auth.ScheduleDisabledByUpstream, a reason a token refresh must never clear
// (the refresh succeeds for these accounts: their credentials are fine, only
// the upstream refuses to serve them). The only way back in is a successful
// real request, which the recovery loop produces by probing the account with a
// one-word chat request.

const (
	// upstreamResultErrorPrefix mirrors the error built by
	// internal/chathub/client.go for a non-Success result frame
	// ("upstream result error: %s"). It is the only trace of that failure mode
	// reaching the web layer; keep both sides in sync if the wording changes.
	upstreamResultErrorPrefix = "upstream result error: "

	accountRejectThresholdDefault = 3

	// accountUpstreamProbeText is deliberately trivial: the probe measures
	// whether the account is served at all, not what it can answer.
	accountUpstreamProbeText = `Say "OK" in one word.`
)

// isUpstreamRejection reports whether the upstream answered with a non-Success
// result frame. Transient AAD/transport errors do NOT match: only a refusal
// that would repeat on every request counts towards an automatic disable.
func isUpstreamRejection(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), upstreamResultErrorPrefix)
}

func accountRejectThreshold() int {
	if v := strings.TrimSpace(os.Getenv("M365_ACCOUNT_REJECT_THRESHOLD")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return accountRejectThresholdDefault
}

// noteUpstreamRejection counts one consecutive rejection and takes the account
// out of rotation once the threshold is crossed. Rejections below the threshold
// only book the ordinary health cooldown, so a single upstream hiccup cannot
// disable an account.
func (s *Server) noteUpstreamRejection(accountID string, err error) {
	if s == nil || accountID == "" || s.tokens == nil {
		return
	}
	threshold := accountRejectThreshold()
	s.accountRejectsMtx.Lock()
	if s.accountRejects == nil {
		s.accountRejects = map[string]int{}
	}
	s.accountRejects[accountID]++
	streak := s.accountRejects[accountID]
	s.accountRejectsMtx.Unlock()

	if streak < threshold {
		log.Printf("[account-availability] account=%s upstream rejection %d/%d: %v", accountID, streak, threshold, err)
		return
	}
	if acc, ok := s.tokens.Get(accountID); ok && acc.ScheduleDisabled {
		return // already out of rotation, whatever the reason
	}
	if derr := s.tokens.DisableScheduleUpstream(accountID); derr != nil {
		log.Printf("[account-availability] account=%s auto-disable failed: %v", accountID, derr)
		return
	}
	log.Printf("[account-availability] account=%s auto-disabled after %d consecutive upstream rejections: %v", accountID, streak, err)
}

// clearUpstreamRejections forgets the streak after the account answered again.
func (s *Server) clearUpstreamRejections(accountID string) {
	if s == nil || accountID == "" {
		return
	}
	s.accountRejectsMtx.Lock()
	delete(s.accountRejects, accountID)
	s.accountRejectsMtx.Unlock()
}

// upstreamRejectStreak reports the current consecutive-rejection count.
func (s *Server) upstreamRejectStreak(accountID string) int {
	if s == nil || accountID == "" {
		return 0
	}
	s.accountRejectsMtx.Lock()
	defer s.accountRejectsMtx.Unlock()
	return s.accountRejects[accountID]
}

// probeUpstreamDisabled was folded into Server.recoverAccounts: both failure
// modes (dead token, refused account) are repaired by one loop that first
// refreshes the token and then proves the account with a real request.

// pinConversationAccount resolves a client-supplied conversation id to the
// account that owns it, so the pool can never round-robin a conversation onto a
// different account (that binds one channel while the answer lands elsewhere).
// When the owning account is no longer usable the cloud thread cannot be
// continued by anyone, so the id is dropped and the caller starts a fresh
// conversation on a healthy account instead of returning a 502. The client
// resends its full history, and the next Bind() repoints the session at the new
// account + conversation.
//
// Returns the account to pin ("" for none) and whether the conversation id
// survived.
func (s *Server) pinConversationAccount(conversationID string) (string, bool) {
	if conversationID == "" || s.sessionResolver == nil {
		return "", true
	}
	sess, ok := s.sessionResolver.GetConversation(conversationID)
	if !ok || sess.AccountID == "" {
		return "", true
	}
	if !s.accountAvailable(sess.AccountID) {
		log.Printf("[session-resolver] conversation=%s belongs to unusable account=%s -> dropping the pin and starting a new cloud conversation", conversationID, sess.AccountID)
		return "", false
	}
	log.Printf("[session-resolver] conversation-bound account=%s conversation=%s", sess.AccountID, conversationID)
	return sess.AccountID, true
}

// probeAccountUpstream runs one minimal chat against an account. The request
// mirrors the model test in the console: default tone, no tools, no history.
func (s *Server) probeAccountUpstream(acc auth.AccountToken) error {
	if s.upstreamProbe != nil {
		return s.upstreamProbe(acc)
	}
	if s.settings == nil {
		return context.Canceled
	}
	tone, err := reasoningTone("", "")
	if err != nil {
		tone = "magic"
	}
	timeout := time.Duration(s.settings.get().ChatTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = s.chatWithAccount(ctx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:         accountUpstreamProbeText,
		Tone:         tone,
		LicenseType:  s.settings.get().LicenseType,
		Scenario:     s.settings.get().Scenario,
		FeatureFlags: s.featureFlags(),
	})
	return err
}
