package auth

// Buff Cloud fork: routing.pinned and routing.pinned-fallback.

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// routingPin returns the configured pinned credential id and whether other
// credentials may serve its provider's requests when it cannot.
func (m *Manager) routingPin() (string, bool) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return "", true
	}
	fallback := cfg.Routing.PinnedFallback == nil || *cfg.Routing.PinnedFallback
	return strings.TrimSpace(cfg.Routing.Pinned), fallback
}

// routingPinLocked returns the credential a request must be limited to because of
// routing.pinned, or "" when the pin does not apply. The pin applies only to
// requests for the pinned credential's provider. With fallback on it applies
// only while the pinned credential can serve the request; with fallback off it
// always applies, so the request fails with the pinned credential's own error
// rather than moving to another one. An id that names no credential is ignored,
// since its provider is unknown. Requires m.mu.
func (m *Manager) routingPinLocked(providerSet map[string]struct{}, model string, eligibility authSelectionEligibility, tried map[string]struct{}, now time.Time) string {
	pinnedID, fallback := m.routingPin()
	if pinnedID == "" {
		return ""
	}
	pinned := m.auths[pinnedID]
	if pinned == nil {
		return ""
	}
	if _, ok := providerSet[canonicalSchedulingProvider(executorKeyFromAuth(pinned))]; !ok {
		return ""
	}
	if !fallback {
		return pinnedID
	}
	if _, used := tried[pinnedID]; used || pinned.Disabled || !eligibility.allows(pinned) {
		return ""
	}
	if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), pinned, model) {
		return ""
	}
	if blocked, _, _ := isAuthBlockedForModel(pinned, m.selectionModelForAuth(pinned, model), now); blocked {
		return ""
	}
	return pinnedID
}
