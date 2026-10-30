package auth

// Buff Cloud fork: routing by the room each subscription credential has left.
// The room numbers come from the quota signals ObserveResponseHeadersForProvider
// records from every Claude and Codex response (quota_signals.go).

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Room strategies selectable in routing.strategy.
const (
	RoomStrategyMostRoom     = "most-room"
	RoomStrategySoonestReset = "soonest-reset"
)

// Room is one rate-limit window of a credential as last observed.
type Room struct {
	// Window is "5h" or "7d" for Claude, "primary" or "secondary" for Codex.
	Window string
	// Used is the fraction of the window spent, 0 to 1.
	Used float64
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time
}

// Weekly reports whether the window is the 7-day one.
func (r Room) Weekly() bool {
	return r.Window == "7d" || r.Window == "secondary"
}

// RoomsFromQuota parses the observed windows of a Claude or Codex credential.
// Claude sends anthropic-ratelimit-unified-{5h,7d}-utilization as a 0 to 1
// fraction and -reset in epoch seconds. Codex sends
// x-codex-{primary,secondary}-used-percent as 0 to 100 and -reset-at in epoch
// seconds; primary is the 5 h window and secondary the 7 d one.
func RoomsFromQuota(provider string, quota QuotaState) []Room {
	signals := quota.Signals
	if len(signals) == 0 {
		return nil
	}
	var rooms []Room
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		for _, window := range []string{"5h", "7d"} {
			prefix := "anthropic-ratelimit-unified-" + window
			used, ok := parseSignalFloat(signals, prefix+"-utilization")
			if !ok {
				continue
			}
			rooms = append(rooms, Room{Window: window, Used: used, ResetsAt: parseSignalEpoch(signals, prefix+"-reset")})
		}
	case "codex":
		for _, window := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + window
			percent, ok := parseSignalFloat(signals, prefix+"-used-percent")
			if !ok {
				continue
			}
			resetsAt := parseSignalEpoch(signals, prefix+"-reset-at")
			if resetsAt.IsZero() && !quota.ObservedAt.IsZero() {
				if after, okAfter := parseSignalFloat(signals, prefix+"-reset-after-seconds"); okAfter && after >= 0 {
					resetsAt = quota.ObservedAt.Add(time.Duration(after * float64(time.Second)))
				}
			}
			rooms = append(rooms, Room{Window: window, Used: percent / 100, ResetsAt: resetsAt})
		}
	}
	return rooms
}

// RoomLeft is the room left in the credential's worst window, 0 to 1. A window
// whose reset has passed counts as empty again; no observation counts as full.
func RoomLeft(rooms []Room, now time.Time) float64 {
	left := 1.0
	for _, room := range rooms {
		if !room.ResetsAt.IsZero() && !room.ResetsAt.After(now) {
			continue
		}
		left = math.Min(left, 1-room.Used)
	}
	return math.Max(left, 0)
}

// WeeklyReset is the next reset of the credential's 7-day window, or zero when
// none is known (never observed, or the observed reset already passed).
func WeeklyReset(rooms []Room, now time.Time) time.Time {
	for _, room := range rooms {
		if room.Weekly() && room.ResetsAt.After(now) {
			return room.ResetsAt
		}
	}
	return time.Time{}
}

// parseSignalFloat reads a signal by header name; signals are stored under
// canonical header keys.
func parseSignalFloat(signals map[string]string, name string) (float64, bool) {
	raw, ok := signals[http.CanonicalHeaderKey(name)]
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(raw), "%"), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func parseSignalEpoch(signals map[string]string, key string) time.Time {
	seconds, ok := parseSignalFloat(signals, key)
	if !ok || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0)
}

// RoomSelector picks by room left. most-room takes the credential whose worst
// window has the most room, ties to the least recently used. soonest-reset takes,
// among credentials with room left, the one whose weekly window resets first, so
// room that would expire unused is spent first; credentials with no known weekly
// reset come after those with one and are ordered by most room.
type RoomSelector struct {
	strategy string
	now      func() time.Time

	mu         sync.Mutex
	lastPicked map[string]time.Time
	// sessions holds each session's last account, for the paced strategy's
	// active-session counts (selector_paced.go).
	sessions map[string]pacedSession
}

// NewRoomSelector returns a selector for RoomStrategyMostRoom, RoomStrategySoonestReset
// or RoomStrategyPaced.
func NewRoomSelector(strategy string) *RoomSelector {
	if !IsRoomStrategy(strategy) {
		strategy = RoomStrategyMostRoom
	}
	return &RoomSelector{strategy: strategy, now: time.Now, lastPicked: make(map[string]time.Time), sessions: make(map[string]pacedSession)}
}

// Strategy returns the selector's routing.strategy value.
func (s *RoomSelector) Strategy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strategy
}

// AffinityRoomStrategy returns the strategy of the room selector inside a
// session-affinity selector, or "" when it wraps another selector.
func AffinityRoomStrategy(affinity *SessionAffinitySelector) string {
	if affinity == nil {
		return ""
	}
	if room, ok := affinity.fallback.(*RoomSelector); ok && room != nil {
		return room.Strategy()
	}
	return ""
}

// SetRoomStrategy switches the active room selector between its strategies in place, so a strategy change keeps the session-affinity
// bindings that replacing the selector would drop. It reports false when the
// active selector is not a room selector.
func (m *Manager) SetRoomStrategy(strategy string) bool {
	if !IsRoomStrategy(strategy) {
		return false
	}
	selector := m.Selector()
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	room, ok := selector.(*RoomSelector)
	if !ok || room == nil {
		return false
	}
	room.mu.Lock()
	room.strategy = strategy
	room.mu.Unlock()
	return true
}

type roomCandidate struct {
	auth     *Auth
	rooms    []Room
	left     float64
	reset    time.Time
	lastUsed time.Time
}

// Pick implements Selector.
func (s *RoomSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := s.now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strategy == RoomStrategyPaced {
		chosen := s.pickPacedLocked(ctx, model, opts, available, now)
		s.notePickedLocked(chosen.ID, now)
		return chosen, nil
	}
	candidates := make([]roomCandidate, 0, len(available))
	for _, auth := range available {
		rooms := RoomsFromQuota(auth.Provider, auth.Quota)
		lastUsed := auth.Quota.ObservedAt
		if picked := s.lastPicked[auth.ID]; picked.After(lastUsed) {
			lastUsed = picked
		}
		candidates = append(candidates, roomCandidate{auth: auth, rooms: rooms, left: RoomLeft(rooms, now), reset: WeeklyReset(rooms, now), lastUsed: lastUsed})
	}
	best := 0
	for i := 1; i < len(candidates); i++ {
		if s.better(candidates[i], candidates[best]) {
			best = i
		}
	}
	chosen := candidates[best]
	s.notePickedLocked(chosen.auth.ID, now)
	selectorLogEntry(ctx).Infof("room-routing: %s picked %s (%s) | %s", s.strategy, chosen.auth.ID, chosen.auth.Label, describeRoomCandidates(candidates, now))
	return chosen.auth, nil
}

func (s *RoomSelector) notePickedLocked(authID string, now time.Time) {
	s.lastPicked[authID] = now
	if len(s.lastPicked) > 4096 {
		s.lastPicked = map[string]time.Time{authID: now}
	}
}

func (s *RoomSelector) better(a, b roomCandidate) bool {
	if s.strategy == RoomStrategySoonestReset {
		aOpen, bOpen := a.left > 0, b.left > 0
		if aOpen != bOpen {
			return aOpen
		}
		aKnown, bKnown := !a.reset.IsZero(), !b.reset.IsZero()
		if aOpen && aKnown != bKnown {
			return aKnown
		}
		if aOpen && aKnown && !a.reset.Equal(b.reset) {
			return a.reset.Before(b.reset)
		}
	}
	if a.left != b.left {
		return a.left > b.left
	}
	if !a.lastUsed.Equal(b.lastUsed) {
		return a.lastUsed.Before(b.lastUsed)
	}
	return a.auth.ID < b.auth.ID
}

func describeRoomCandidates(candidates []roomCandidate, now time.Time) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		windows := make([]string, 0, len(c.rooms))
		for _, room := range c.rooms {
			reset := "?"
			if !room.ResetsAt.IsZero() {
				reset = room.ResetsAt.UTC().Format(time.RFC3339)
			}
			windows = append(windows, fmt.Sprintf("%s used=%.2f reset=%s", room.Window, room.Used, reset))
		}
		if len(windows) == 0 {
			windows = append(windows, "not heard from")
		}
		parts = append(parts, fmt.Sprintf("%s room=%.2f [%s]", c.auth.ID, c.left, strings.Join(windows, ", ")))
	}
	return strings.Join(parts, "; ")
}
