package helps

// Buff Cloud fork: tell a refusal of the request apart from a rate limit.

import (
	"net/http"
	"strings"
)

// IsClaudeRefusal reports whether a Claude 429 is Anthropic refusing this
// request rather than a rate limit. A quota 429 carries the
// anthropic-ratelimit-unified-* headers (unified-status: rejected and the
// resets); Anthropic's refusal of a client other than Claude Code on a
// subscription login carries no rate-limit header and no Retry-After. Another
// credential would get the same answer, so the refusal must neither cool the
// credential down nor move the request to another one.
func IsClaudeRefusal(statusCode int, headers http.Header) bool {
	return statusCode == http.StatusTooManyRequests && !hasHeaderWithPrefix(headers, "retry-after", "anthropic-ratelimit-")
}

// IsCodexRefusal is the Codex counterpart of IsClaudeRefusal: a 429 without
// Retry-After and without x-codex-primary-* or x-codex-secondary-* usage headers.
// The caller also rules out a usage_limit_reached body.
func IsCodexRefusal(statusCode int, headers http.Header) bool {
	return statusCode == http.StatusTooManyRequests && !hasHeaderWithPrefix(headers, "retry-after", "x-codex-primary-", "x-codex-secondary-")
}

func hasHeaderWithPrefix(headers http.Header, prefixes ...string) bool {
	for key := range headers {
		lower := strings.ToLower(key)
		for _, prefix := range prefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}
