package auth

// Buff Cloud fork: the paced strategy. It spreads sessions over every account
// that has room, so the 5-hour rooms of several accounts add up, and it paces
// the weekly windows so that no account runs dry days before its reset while
// another ends its week with room unspent.
//
// Each account has a weekly target rate: weekly room left ÷ hours to its
// weekly reset. A new session goes to the account with the highest target rate
// per active session, scaled down as its 5-hour room runs low. An account with
// little 5-hour room gets no new session while another has more, and a bound
// session leaves an account near its 5-hour limit, or with its week spent,
// before it gets a 429.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// RoomStrategyPaced is the routing.strategy value of the paced strategy.
const RoomStrategyPaced = "paced"

const (
	// pacedNewSessionFloor is the 5-hour room under which an account gets no
	// new session while another account has at least this much.
	pacedNewSessionFloor = 0.20
	// pacedMoveAt is the 5-hour room under which a bound session moves to another
	// account that is above pacedNewSessionFloor. The gap to the floor keeps a
	// moved session from moving back.
	pacedMoveAt = 0.10
	// pacedWeekEmpty is the weekly room under which an account counts as spent.
	// The weekly window has no floor of its own: the target rate already slows
	// an account down as its week runs low, and a floor would leave each week
	// with that much room unspent at its reset.
	pacedWeekEmpty = 0.02
	// pacedRecheckAfter is how old a spent-week reading gets before one new
	// session goes to the account to read fresh headers. A plan upgrade or an
	// early reset only shows in a response, and a spent account gets none.
	pacedRecheckAfter = 3 * time.Hour
	// pacedFullFiveHour is the 5-hour room above which the 5-hour window does
	// not lower an account's score.
	pacedFullFiveHour = 0.5
	// pacedActiveFor is how long after its last request a session counts as active.
	pacedActiveFor = 10 * time.Minute
	// pacedUnknownWeek is the time to reset assumed for a weekly window with no
	// known reset (an account never heard from).
	pacedUnknownWeek = 7 * 24 * time.Hour
)

// IsRoomStrategy reports whether strategy is one of the room selector's.
func IsRoomStrategy(strategy string) bool {
	switch strategy {
	case RoomStrategyMostRoom, RoomStrategySoonestReset, RoomStrategyPaced:
		return true
	}
	return false
}

// Pace is how the paced strategy sees one account for one model.
type Pace struct {
	// FiveHour is the room left in the account's 5-hour windows, 0 to 1,
	// after its reserve.
	FiveHour float64
	// Week is the room left in its weekly windows, 0 to 1, after its reserve.
	Week float64
	// WeekResetsAt is when the weekly window that binds resets; zero when unknown.
	WeekResetsAt time.Time
	// TargetRate is the share of the weekly limit per hour that spends Week
	// exactly by WeekResetsAt.
	TargetRate float64
}

// Gate is the room the floor and the move rules test: the 5-hour room, or 0
// once the week is spent.
func (p Pace) Gate() float64 {
	if p.Week < pacedWeekEmpty {
		return 0
	}
	return p.FiveHour
}

// PaceOf computes an account's pace for a request for model ("" counts only
// the windows that bind every request). Per-model windows such as 7d_opus or
// 5h_sonnet bind only their model family. A window whose reset has passed
// counts as empty, and an account never heard from counts as full.
func PaceOf(auth *Auth, model string, now time.Time) Pace {
	pace := Pace{FiveHour: 1, Week: 1}
	if auth == nil {
		return pace
	}
	reserve := ReserveOf(auth)
	model = strings.ToLower(model)
	for _, room := range reserveRooms(auth.Provider, auth.Quota) {
		if family := reserveWindowModel(room.Window); family != "" && (model == "" || !strings.Contains(model, family)) {
			continue
		}
		if !room.ResetsAt.IsZero() && !room.ResetsAt.After(now) {
			continue
		}
		left := math.Max(1-room.Used-reserve.For(room.Window), 0)
		if pacedWeeklyWindow(room.Window) {
			if left < pace.Week {
				pace.Week, pace.WeekResetsAt = left, room.ResetsAt
			}
			continue
		}
		pace.FiveHour = math.Min(pace.FiveHour, left)
	}
	until := pacedUnknownWeek
	if !pace.WeekResetsAt.IsZero() {
		until = pace.WeekResetsAt.Sub(now)
	}
	pace.TargetRate = pace.Week / math.Max(until.Hours(), 1)
	return pace
}

// pacedWeeklyWindow reports whether a window name is a weekly one: 7d,
// secondary, or a per-model 7d_<family>.
func pacedWeeklyWindow(window string) bool {
	return window == "7d" || window == "secondary" || strings.HasPrefix(window, "7d_")
}

// pacedScore ranks an account for a new session: its weekly target rate per
// active session, lowered while its 5-hour room is under pacedFullFiveHour.
func pacedScore(p Pace, active int) float64 {
	return p.TargetRate * math.Min(p.FiveHour/pacedFullFiveHour, 1) / float64(1+active)
}

type pacedSession struct {
	authID string
	seen   time.Time
}

// NoteSession records that a session's request went to authID, for the
// active-session counts. Requires s.mu.
func (s *RoomSelector) noteSessionLocked(sessionID, authID string, now time.Time) {
	if sessionID == "" || authID == "" {
		return
	}
	if s.sessions == nil {
		s.sessions = make(map[string]pacedSession)
	}
	s.sessions[sessionID] = pacedSession{authID: authID, seen: now}
	if len(s.sessions) > 4096 {
		s.pruneSessionsLocked(now)
	}
}

func (s *RoomSelector) pruneSessionsLocked(now time.Time) {
	for id, session := range s.sessions {
		if now.Sub(session.seen) > pacedActiveFor {
			delete(s.sessions, id)
		}
	}
}

// activeLocked counts the active sessions per account, leaving out the
// session being picked for. Requires s.mu.
func (s *RoomSelector) activeLocked(exceptSession string, now time.Time) map[string]int {
	s.pruneSessionsLocked(now)
	active := make(map[string]int)
	for id, session := range s.sessions {
		if id != exceptSession {
			active[session.authID]++
		}
	}
	return active
}

// NoteSession records that a session's request went to authID.
func (s *RoomSelector) NoteSession(sessionID, authID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSessionLocked(sessionID, authID, s.now())
}

// ActiveSessions counts the sessions per account that sent a request in the
// last ten minutes.
func (s *RoomSelector) ActiveSessions() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked("", s.now())
}

// ActiveSessions returns the paced selector's active-session counts per
// account, or nil when the active selector does not count sessions.
func (m *Manager) ActiveSessions() map[string]int {
	selector := m.Selector()
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	room, ok := selector.(*RoomSelector)
	if !ok || room == nil {
		return nil
	}
	return room.ActiveSessions()
}

// pacedCandidate is one account the paced strategy weighs.
type pacedCandidate struct {
	auth   *Auth
	pace   Pace
	active int
	score  float64
}

// recheckDue reports whether the account's week reads as spent from a reading
// older than pacedRecheckAfter.
func recheckDue(auth *Auth, p Pace, now time.Time) bool {
	return p.Week < pacedWeekEmpty && !auth.Quota.ObservedAt.IsZero() && now.Sub(auth.Quota.ObservedAt) >= pacedRecheckAfter
}

// pickPacedLocked picks for a new session. An account whose spent-week reading
// is due a recheck takes one new session first, at most once per
// pacedRecheckAfter; if it is still spent, its 429 moves the request on at once.
// Otherwise accounts whose Gate is at least
// pacedNewSessionFloor go first; among them the highest score wins. When no
// account has that much, the one with the most room wins. Requires s.mu.
func (s *RoomSelector) pickPacedLocked(ctx context.Context, model string, opts cliproxyexecutor.Options, available []*Auth, now time.Time) *Auth {
	sessionID, _ := opts.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey].(string)
	active := s.activeLocked(sessionID, now)
	candidates := make([]pacedCandidate, 0, len(available))
	for _, auth := range available {
		pace := PaceOf(auth, model, now)
		n := active[auth.ID]
		candidates = append(candidates, pacedCandidate{auth: auth, pace: pace, active: n, score: pacedScore(pace, n)})
	}
	best := 0
	for i := 1; i < len(candidates); i++ {
		if s.pacedBetter(candidates[i], candidates[best]) {
			best = i
		}
	}
	for _, c := range candidates {
		if recheckDue(c.auth, c.pace, now) && now.Sub(s.rechecked[c.auth.ID]) >= pacedRecheckAfter {
			if s.rechecked == nil {
				s.rechecked = make(map[string]time.Time)
			}
			s.rechecked[c.auth.ID] = now
			s.noteSessionLocked(sessionID, c.auth.ID, now)
			selectorLogEntry(ctx).Infof("room-routing: paced rechecks %s (%s): its spent-week reading is %s old", c.auth.ID, c.auth.Label, now.Sub(c.auth.Quota.ObservedAt).Round(time.Minute))
			return c.auth
		}
	}
	chosen := candidates[best]
	s.noteSessionLocked(sessionID, chosen.auth.ID, now)
	selectorLogEntry(ctx).Infof("room-routing: paced picked %s (%s) | %s", chosen.auth.ID, chosen.auth.Label, describePacedCandidates(candidates, now))
	return chosen.auth
}

func (s *RoomSelector) pacedBetter(a, b pacedCandidate) bool {
	aOpen, bOpen := a.pace.Gate() >= pacedNewSessionFloor, b.pace.Gate() >= pacedNewSessionFloor
	if aOpen != bOpen {
		return aOpen
	}
	if aOpen {
		if a.score != b.score {
			return a.score > b.score
		}
	} else if a.pace.Gate() != b.pace.Gate() {
		return a.pace.Gate() > b.pace.Gate()
	}
	if a.active != b.active {
		return a.active < b.active
	}
	aUsed, bUsed := s.lastUsedLocked(a.auth), s.lastUsedLocked(b.auth)
	if !aUsed.Equal(bUsed) {
		return aUsed.Before(bUsed)
	}
	return a.auth.ID < b.auth.ID
}

func (s *RoomSelector) lastUsedLocked(auth *Auth) time.Time {
	lastUsed := auth.Quota.ObservedAt
	if picked := s.lastPicked[auth.ID]; picked.After(lastUsed) {
		lastUsed = picked
	}
	return lastUsed
}

func describePacedCandidates(candidates []pacedCandidate, now time.Time) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		reset := "?"
		if !c.pace.WeekResetsAt.IsZero() {
			reset = fmt.Sprintf("%.0fh", c.pace.WeekResetsAt.Sub(now).Hours())
		}
		parts = append(parts, fmt.Sprintf("%s 5h=%.2f week=%.2f reset=%s rate=%.4f/h active=%d score=%.5f",
			c.auth.ID, c.pace.FiveHour, c.pace.Week, reset, c.pace.TargetRate, c.active, c.score))
	}
	return strings.Join(parts, "; ")
}

// pacedSelector returns the paced room selector inside the affinity selector,
// or nil when another strategy is active.
func (s *SessionAffinitySelector) pacedSelector() *RoomSelector {
	room, ok := s.fallback.(*RoomSelector)
	if !ok || room == nil || room.Strategy() != RoomStrategyPaced {
		return nil
	}
	return room
}

// dropNearLimit leaves out the accounts near their 5-hour limit, or with their
// week spent, for model while another usable account has 5-hour room, so a session bound to one of them moves
// now, before it gets a 429. Moving costs one prompt-cache rebuild; a 429
// costs the session its turn. When no account has room, nothing is dropped.
func dropNearLimit(auths []*Auth, model string, now time.Time) ([]*Auth, []string) {
	roomy := false
	for _, auth := range auths {
		if blocked, _, _ := isAuthBlockedForModel(auth, model, now); blocked {
			continue
		}
		if PaceOf(auth, model, now).Gate() >= pacedNewSessionFloor {
			roomy = true
			break
		}
	}
	if !roomy {
		return auths, nil
	}
	kept := make([]*Auth, 0, len(auths))
	var dropped []string
	for _, auth := range auths {
		if pace := PaceOf(auth, model, now); auth != nil && pace.Gate() < pacedMoveAt && !recheckDue(auth, pace, now) {
			dropped = append(dropped, auth.ID)
			continue
		}
		kept = append(kept, auth)
	}
	sort.Strings(dropped)
	return kept, dropped
}
