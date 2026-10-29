package auth

// Buff Cloud fork: recheck a credential. Its cooldown and its last quota reading
// are cleared, so the router counts it as not heard from and the next request
// on it reads fresh rate-limit headers. Buff Cloud's Recheck and refresh buttons
// call it, and so does the plan watch when an account's plan changes.

import (
	"context"
	"net/http"
	"time"
)

// RecheckAuth clears authID's cooldown and quota reading. It returns the
// updated credential, or nil when there is none with that id.
func (m *Manager) RecheckAuth(ctx context.Context, authID string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	// The reading goes first: ResetQuota persists the auth file, and a reload of
	// it carries over the in-memory and saved readings, which must be gone by then.
	m.mu.Lock()
	auth := m.auths[authID]
	if auth != nil {
		auth.Quota.Signals = nil
		auth.Quota.ObservedAt = time.Time{}
	}
	store := m.quotaSignals
	m.mu.Unlock()
	if auth == nil {
		return nil, nil
	}
	store.forget(authID)
	updated, _, err := m.ResetQuota(ctx, authID)
	return updated, err
}

// ObserveQuota sets a credential's quota reading from rate-limit headers read
// outside a request (Claude Code's /usage, Codex wham/usage), as a response
// with those headers would. It reports false when there is no such credential
// or the headers hold no quota signal.
func (m *Manager) ObserveQuota(authID string, headers http.Header, observedAt time.Time) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[authID]
	if auth == nil || !auth.Quota.ObserveResponseHeadersForProvider(auth.Provider, headers, observedAt) {
		return false
	}
	m.quotaSignals.record(auth.ID, auth.Quota)
	return true
}
