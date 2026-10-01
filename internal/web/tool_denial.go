package web

import "strings"

// A model that was HANDED tools and then says "this session has no tools" is
// stating something false by construction — the gateway knows the tool list,
// the model does not get to deny it. Observed 2026-09-29 11:19 on conversation
// 677634d3: a request with tools=34 produced an 834-character answer opening
// with "当前会话没有这些工具，无法接管活动浏览器、SSH 会话或服务器" and
// attributing the caller's tools to a differently-named machine
// ("Hermes clawdi-runtime"). None of the existing tiers caught it:
//
//   - isToolRefusal bails on any text >= 200 chars, so an 834-char denial passes.
//   - sandboxHallucinationPatterns keys on container/sandbox/file claims; a
//     denial never mentions those, so zero substrings matched.
//
// Unlike the keyword tiers this is decidable from gateway state, not from tone:
// the claim is only false when len(toolMaps) > 0. Callers that genuinely
// attach no tools are unaffected, and executionEjectTrigger already routes
// them to the isSandboxClaim branch instead.
var toolDenialPatterns = []string{
	// Chinese — session/this-conversation denies having the caller's tools.
	"当前会话没有这些工具",
	"当前会话没有工具",
	"本会话没有这些工具",
	"本会话没有工具",
	"这个会话没有工具",
	"本次会话没有工具",
	"当前对话没有工具",
	"本轮没有工具",
	"当前轮没有工具",
	"没有挂载任何工具",
	"未挂载工具",
	"没有可用的工具",
	"没有可调用的工具",
	"无法调用工具",
	"无法使用工具",
	"不能调用工具",
	"无法接管活动浏览器",
	"无法接管浏览器",
	"没有终端工具",
	"没有执行工具",
	"没有 shell",
	"拿不到终端",
	"接管的会话没有",
	// English equivalents.
	"this session has no tools",
	"this conversation has no tools",
	"i have no tools",
	"i don't have any tools",
	"no tools available to me",
	"i cannot call tools",
	"i can't call tools",
	"i have no terminal",
	"i have no shell access",
	"i cannot access the terminal",
	"i can't access the terminal",
	"another machine",
	"a different machine",
	"on a different host",
}

// isToolDenialClaim reports whether text denies the caller-provided tools.
// Only meaningful when tools were actually declared — the caller gates it.
func isToolDenialClaim(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range toolDenialPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
