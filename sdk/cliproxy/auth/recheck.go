package auth

// Buff Cloud fork: recheck a credential. Its cooldown and its last quota reading
// are cleared, so the router counts it as not heard from and the next request
// on it reads fresh rate-limit headers. Buff Cloud's Recheck and refresh buttons
// call it, and so does the plan watch when an account's plan changes.

import (
	"context"
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
