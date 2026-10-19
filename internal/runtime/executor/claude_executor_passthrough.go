package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Buff Cloud fork: with disable-claude-cloak-mode set, a Claude request leaves
// the proxy as the caller sent it. Upstream still applies the Claude Code CLI
// header profile, a rebuilt beta list, a derived session id, a rewritten
// metadata.user_id and CCH re-signing to every OAuth request when cloaking is
// off; this fork never disguises a client, so all of that is skipped. Only
// translation, the model mapping, user payload rules, the credential and the
// oauth beta Anthropic requires for an OAuth bearer are applied.

// claudePassthroughDropped are headers describing the caller's hop or its
// credential for this proxy, never forwarded upstream.
var claudePassthroughDropped = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Proxy-Authorization": true, "Cookie": true,
	"Host": true, "Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true,
	"Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true,
	"Forwarded": true, "Via": true, "X-Real-Ip": true,
}

func claudePassthrough(cfg *config.Config) bool {
	return cfg != nil && cfg.DisableClaudeCloakMode
}

// claudePassthroughBody snapshots the translated body before the cloaking and
// fingerprinting steps, or returns nil when passthrough is off.
func claudePassthroughBody(cfg *config.Config, body []byte) []byte {
	if !claudePassthrough(cfg) {
		return nil
	}
	return append([]byte(nil), body...)
}

// finishClaudePassthroughBody applies what the request needs to be valid and
// nothing a native Claude Code request lacks: max_tokens if missing, and
// stream when the transport needs an upstream stream.
func finishClaudePassthroughBody(body []byte, baseModel string, stream bool) []byte {
	body = ensureModelMaxTokens(body, baseModel)
	if stream {
		body = helps.SetBoolIfDifferent(body, "stream", true)
	}
	return body
}

func applyClaudePassthroughHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, extraBetas []string, incoming http.Header) {
	for name, values := range incoming {
		canonical := http.CanonicalHeaderKey(name)
		if claudePassthroughDropped[canonical] || strings.HasPrefix(canonical, "X-Forwarded-") || strings.HasPrefix(canonical, "Tailscale-") {
			continue
		}
		r.Header[canonical] = append([]string(nil), values...)
	}
	oauth := claudeCredentialUsesOAuth(auth, apiKey)
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		if oauth || !isAnthropicUpstreamURL(r.URL) {
			r.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			r.Header.Set("X-Api-Key", apiKey)
		}
	}

	var betas []string
	seen := map[string]bool{}
	addBetas := func(values ...string) {
		for _, value := range values {
			for _, beta := range strings.Split(value, ",") {
				if beta = strings.TrimSpace(beta); beta != "" && !seen[beta] {
					seen[beta] = true
					betas = append(betas, beta)
				}
			}
		}
	}
	addBetas(incoming.Values("Anthropic-Beta")...)
	addBetas(extraBetas...)
	if oauth {
		addBetas(claudeOAuthBeta)
	}
	r.Header.Del("Anthropic-Beta")
	if len(betas) > 0 {
		r.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
	}

	misc.EnsureHeader(r.Header, incoming, "Content-Type", "application/json")
	misc.EnsureHeader(r.Header, incoming, "Anthropic-Version", "2023-06-01")
	misc.EnsureHeader(r.Header, incoming, "Accept", "application/json")
	misc.EnsureHeader(r.Header, incoming, "Accept-Encoding", "gzip, deflate, br, zstd")
	// A caller that sent no User-Agent must not go out as Go's default; name the hop honestly.
	misc.EnsureHeader(r.Header, incoming, "User-Agent", "CLIProxyAPI/"+buildinfo.Version)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, incoming)
}
