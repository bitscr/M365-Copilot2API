package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

var ErrOffensiveContent = errors.New("upstream content policy flagged as offensive")

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// actionableUpstreamError returns a safe, useful client-facing explanation.
// Raw provider details remain in gateway logs, but users should be told what
// failed and what they can do instead of receiving a generic authentication
// banner from the client.
func actionableUpstreamError(err error) (code, message string) {
	if err == nil {
		return "upstream_error", "M365 upstream request failed. Retry once; if it continues, check the gateway account status and recent logs."
	}
	cat := ClassifyError(err)
	switch cat {
	case CategoryAuthExpired401:
		return "m365_account_expired", "The selected M365 account token has expired or was revoked. Refresh or re-authorize that account in the gateway console, then retry. If account failover is enabled, verify that at least one other account is healthy."
	case CategoryForbidden403:
		return "m365_account_forbidden", "Microsoft rejected this M365 account for the requested operation. Check that the account still has Copilot access and the required tenant/license permissions, or switch to another healthy account."
	case CategoryUserBanned:
		return "m365_account_disabled", "The selected M365 account is disabled or blocked upstream. Remove it from rotation and authorize another account."
	case CategoryDesignerDisabled:
		return "m365_feature_unavailable", "This M365 tenant does not allow the requested feature. Use a supported model or feature, or switch to an account whose tenant enables it."
	case CategoryQuota429, CategoryUserThrottled, CategoryInsufficientTokens:
		return "rate_limit_error", "The selected M365 account has reached a usage or rate limit. Wait for the reported retry interval or switch to another healthy account."
	case CategoryOverload503:
		return "upstream_overloaded", "Microsoft's upstream service is temporarily overloaded. Retry shortly; the gateway will use another healthy account when possible."
	case CategoryDNS, CategoryTCP, CategoryTLS, CategorySOCKS5, CategoryWSHandshake:
		return "upstream_connection_error", "The gateway could not connect to Microsoft. Check DNS, outbound proxy and TLS connectivity from the server, then retry."
	case CategoryWSReadTimeout:
		return "upstream_timeout", "Microsoft did not complete the response before the gateway timeout. Retry the request or increase the chat timeout in gateway settings."
	case CategoryClientCanceled:
		return "request_canceled", "The request was canceled before completion. Retry it and keep the client connection open until a terminal event is received."
	case CategoryRetryable422:
		return "upstream_rejected_request", "Microsoft temporarily rejected this request. Retry once; if it repeats, reduce the conversation context or start a new conversation."
	default:
		return "upstream_error", "M365 could not complete the request. Retry once; if it continues, check account health and the gateway logs using the request ID."
	}
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// everything else is 502. Unknown upstream failures must never leak internals.
func upstreamStatus(err error) int {
	if errors.Is(err, chathub.ErrOffensiveContent) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, chathub.ErrImageLimit) {
		return http.StatusTooManyRequests
	}
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	cat := ClassifyError(err)
	switch cat {
	case CategoryUserBanned:
		return http.StatusForbidden
	case CategoryUserThrottled:
		return http.StatusTooManyRequests
	case CategoryInsufficientTokens:
		return http.StatusTooManyRequests
	case CategoryRetryable422:
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}

func applyM365Headers(w http.ResponseWriter, err error, accountID string) {
	cat := ClassifyError(err)
	if accountID != "" {
		w.Header().Set("X-M365-Account-Id", accountID)
	} else {
		w.Header().Set("X-M365-Account-Id", "")
	}
	w.Header().Set("X-M365-Proxy-Error", string(cat))
	if GlobalCircuitIsOpen() {
		remaining := int(time.Until(GlobalCircuitOpenUntil()).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-M365-Global-Circuit", fmt.Sprintf("open; retry-after=%d", remaining))
	} else {
		w.Header().Set("X-M365-Global-Circuit", "closed")
	}
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-Retry-After", fmt.Sprintf("%d", retry))
		w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Duration(retry)*time.Second).Unix()))
	} else {
		switch cat {
		case CategoryQuota429:
			w.Header().Set("X-M365-Retry-After", "30")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(30*time.Second).Unix()))
		case CategoryOverload503:
			w.Header().Set("X-M365-Retry-After", "15")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(15*time.Second).Unix()))
		case CategoryAuthExpired401:
			w.Header().Set("X-M365-Retry-After", "120")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(2*time.Minute).Unix()))
		case CategoryForbidden403:
			w.Header().Set("X-M365-Retry-After", "86400")
			w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix()))
		}
	}
	if IsRateLimited(err) {
		w.Header().Set("X-M365-RateLimit-Remaining", "0")
	} else {
		w.Header().Set("X-M365-RateLimit-Remaining", "1")
	}
}

func writeUpstreamErrorWithAccount(w http.ResponseWriter, err error, accountID string) {
	applyM365Headers(w, err, accountID)
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	code, msg := actionableUpstreamError(err)
	log.Printf("upstream request failed: %v", err)
	writeOpenAIError(w, status, code, msg)
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	cat := ClassifyError(err)
	switch cat {
	case CategoryQuota429, CategoryOverload503, CategoryRetryable422,
		CategorySOCKS5, CategoryDNS, CategoryTCP, CategoryTLS,
		CategoryWSHandshake, CategoryWSReadTimeout, CategoryUpstreamStructured,
		CategoryGlobalUnavailable:
		return true
	case CategoryForbidden403, CategoryAuthExpired401,
		CategoryUserBanned, CategoryClientCanceled:
		return false
	default:
		return false
	}
}

func ClassifyErrorCode(code string) ErrorCategory {
	switch code {
	case "ErrorUserBanned":
		return CategoryUserBanned
	case "ErrorUserThrottled":
		return CategoryUserThrottled
	case "InsufficientTokens":
		return CategoryInsufficientTokens
	case "ErrorDisallowedAADUser":
		return CategoryDesignerDisabled
	default:
		return CategoryUnknown
	}
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	applyM365Headers(w, err, "")
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "30")
		}
		if w.Header().Get("X-M365-Retry-After") == "" {
			w.Header().Set("X-M365-Retry-After", w.Header().Get("Retry-After"))
		}
		if errors.Is(err, chathub.ErrImageLimit) {
			writeOpenAIError(w, status, "image_limit_error", "image generation daily limit reached; try again tomorrow")
			return
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if errors.Is(err, chathub.ErrOffensiveContent) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	code, msg := actionableUpstreamError(err)
	log.Printf("upstream request failed: %v", err)
	writeOpenAIError(w, status, code, msg)
}
