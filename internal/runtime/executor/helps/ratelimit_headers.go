package helps

import (
	"context"
	"net/http"
	"strings"
)

// Buff Cloud fork: in the honest passthrough modes (disable-claude-cloak-mode,
// disable-codex-cloaking) the client gets the upstream's rate-limit headers, as
// it would talking to the upstream directly. Claude Code builds its
// rate_limit_event from anthropic-ratelimit-unified-*, Codex its limit display
// from x-codex-*. Executors call this on every upstream HTTP response, before
// the handler writes anything, so the headers left on the client response are
// those of the attempt whose answer the client gets, success or error.

// ForwardRateLimitHeaders replaces the rate-limit headers on the client
// response with upstream's when forward is set. It does nothing once the
// client response is written or when the request has no gin context.
func ForwardRateLimitHeaders(ctx context.Context, forward bool, upstream http.Header, isRateLimitHeader func(string) bool) {
	if !forward {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil || ginCtx.Writer == nil || ginCtx.Writer.Written() {
		return
	}
	client := ginCtx.Writer.Header()
	for name := range client {
		if isRateLimitHeader(name) {
			delete(client, name)
		}
	}
	for name, values := range upstream {
		if isRateLimitHeader(name) {
			client[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
}

// IsClaudeRateLimitHeader matches every anthropic-ratelimit-* header and Retry-After.
func IsClaudeRateLimitHeader(name string) bool {
	lower := strings.ToLower(name)
	return lower == "retry-after" || strings.HasPrefix(lower, "anthropic-ratelimit-")
}

// IsCodexRateLimitHeader matches the x-codex-* quota headers, x-ratelimit-* and Retry-After.
func IsCodexRateLimitHeader(name string) bool {
	return isCodexQuotaHeaderName(name)
}
