package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用户要求：上游报错要回复有用的信息，而不是笼统的认证横幅。
// 这两个测试锁定：401/403/429 分类错误必须返回可操作文案与正确 HTTP 状态。

func TestWriteUpstreamErrorWithAccountActionable(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantHint   string
	}{
		{"expired token", &UpstreamHTTPError{Status: 401, Body: `{"error":{"code":"InvalidToken"}}`},
			http.StatusUnauthorized, "m365_account_expired", "Refresh or re-authorize"},
		// 既有契约：IsAuthFailure 覆盖 401/403，两者都映射为 401；区分靠 body 里的 code。
		{"forbidden", &UpstreamHTTPError{Status: 403, Body: "forbidden"},
			http.StatusUnauthorized, "m365_account_forbidden", "switch to another healthy account"},
		// 429 在 writeUpstreamError* 的 429 分支短路，文案保持简短，靠 Retry-After 传信号。
		{"quota", &UpstreamHTTPError{Status: 429, Body: "throttled"},
			http.StatusTooManyRequests, "rate_limit_error", "try again shortly"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeUpstreamErrorWithAccount(rec, c.err, "acc-1")
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, c.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.wantCode) {
				t.Fatalf("body missing code %q: %s", c.wantCode, body)
			}
			if !strings.Contains(body, c.wantHint) {
				t.Fatalf("body missing hint %q: %s", c.wantHint, body)
			}
			if strings.Contains(body, "upstream request failed") {
				t.Fatalf("generic message leaked: %s", body)
			}
			if rec.Header().Get("X-M365-Account-Id") != "acc-1" {
				t.Fatalf("account header missing: %s", rec.Header().Get("X-M365-Account-Id"))
			}
			if c.err.(*UpstreamHTTPError).Status == 429 && rec.Header().Get("Retry-After") == "" {
				t.Fatalf("429 response must carry Retry-After")
			}
		})
	}
}

func TestWriteUpstreamErrorPlainActionable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, &UpstreamHTTPError{Status: 401})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "m365_account_expired") {
		t.Fatalf("expected actionable code in body: %s", rec.Body.String())
	}
}
