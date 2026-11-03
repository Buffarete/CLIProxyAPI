package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// Buff fork: Claude subscription accounts serve Claude Code only.
//
// Anthropic's consumer terms allow a Claude subscription in Claude Code, not in other tools or as an
// API. So when the pool holds a Claude subscription (an OAuth account, auth_kind "oauth"), the claude
// provider is taken out of a request's providers unless the client is Claude Code (User-Agent
// claude-cli/<version>, which the CLI, the Agent SDK and T3 Code's Claude driver send). A pool with
// only Claude API keys is not limited.
// Other providers (Codex, OpenCode Go and the rest) still serve any client. The User-Agent is the
// client's own claim: this guard stops mistakes, not a client that pretends to be Claude Code.

// ErrorCodeClaudeCodeOnly marks a request that asked for a Claude model from a client that is not
// Claude Code.
const ErrorCodeClaudeCodeOnly = "claude_code_only"

func isClaudeCodeUserAgent(ua string) bool {
	product, _, _ := strings.Cut(strings.TrimSpace(ua), " ")
	name, _, _ := strings.Cut(product, "/")
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "claude-cli", "claude-code":
		return true
	}
	return false
}

// hasClaudeSubscription reports whether the pool holds a Claude OAuth (subscription) account.
func (m *Manager) hasClaudeSubscription() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.auths {
		if a != nil && strings.EqualFold(strings.TrimSpace(a.Provider), "claude") &&
			strings.EqualFold(strings.TrimSpace(a.Attributes["auth_kind"]), "oauth") {
			return true
		}
	}
	return false
}

// claudeCodeOnly returns providers without "claude" when the pool holds a Claude subscription and the
// request's client is not Claude Code. When that leaves nothing, it returns an error that says why.
func (m *Manager) claudeCodeOnly(ctx context.Context, providers []string) ([]string, error) {
	if !containsProvider(providers, "claude") || isClaudeCodeUserAgent(logging.GetClientRequestMetadata(ctx).UserAgent) || !m.hasClaudeSubscription() {
		return providers, nil
	}
	kept := providers[:0:0]
	for _, provider := range providers {
		if provider != "claude" {
			kept = append(kept, provider)
		}
	}
	if len(kept) == 0 && len(providers) > 0 {
		return nil, &Error{
			Code:       ErrorCodeClaudeCodeOnly,
			Message:    "Claude subscription accounts serve Claude Code only; use a Codex model (for example gpt-6.1-sol) from this client",
			HTTPStatus: http.StatusForbidden,
		}
	}
	return kept, nil
}
