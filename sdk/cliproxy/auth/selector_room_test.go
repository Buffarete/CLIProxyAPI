package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var roomTestNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func epoch(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// claudeAuth builds a Claude credential whose last response reported the given
// 5h and 7d utilization (0 to 1) and resets.
func claudeAuth(id string, used5h float64, reset5h time.Time, used7d float64, reset7d time.Time, observedAt time.Time) *Auth {
	headers := map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": strconv.FormatFloat(used5h, 'f', -1, 64),
		"anthropic-ratelimit-unified-5h-reset":       epoch(reset5h),
		"anthropic-ratelimit-unified-7d-utilization": strconv.FormatFloat(used7d, 'f', -1, 64),
		"anthropic-ratelimit-unified-7d-reset":       epoch(reset7d),
		"anthropic-ratelimit-unified-status":         "allowed",
	}
	return observedAuth(id, "claude", headers, observedAt)
}

// codexAuth builds a Codex credential whose last response reported the given
// primary and secondary used-percent (0 to 100) and reset-at.
func codexAuth(id string, primary float64, primaryReset time.Time, secondary float64, secondaryReset time.Time, observedAt time.Time) *Auth {
	headers := map[string]string{
		"x-codex-primary-used-percent":   strconv.FormatFloat(primary, 'f', -1, 64),
		"x-codex-primary-reset-at":       epoch(primaryReset),
		"x-codex-secondary-used-percent": strconv.FormatFloat(secondary, 'f', -1, 64),
		"x-codex-secondary-reset-at":     epoch(secondaryReset),
	}
	return observedAuth(id, "codex", headers, observedAt)
}

// observedAuth records headers the way MarkResult does for a live response.
func observedAuth(id, provider string, headers map[string]string, observedAt time.Time) *Auth {
	auth := &Auth{ID: id, Provider: provider, Label: id}
	response := make(map[string][]string, len(headers))
	for key, value := range headers {
		response[key] = []string{value}
	}
	auth.Quota.ObserveResponseHeadersForProvider(provider, response, observedAt)
	return auth
}

func pickRoom(t *testing.T, selector *RoomSelector, auths ...*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "mixed", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func newTestRoomSelector(strategy string) *RoomSelector {
	selector := NewRoomSelector(strategy)
	selector.now = func() time.Time { return roomTestNow }
	return selector
}

func TestRoomsFromQuotaReadsClaudeFractionsAndCodexPercents(t *testing.T) {
	reset5h := roomTestNow.Add(2 * time.Hour)
	reset7d := roomTestNow.Add(50 * time.Hour)
	claude := claudeAuth("c", 0.25, reset5h, 0.6, reset7d, roomTestNow)
	rooms := RoomsFromQuota("claude", claude.Quota)
	if len(rooms) != 2 || rooms[0].Window != "5h" || rooms[0].Used != 0.25 || !rooms[0].ResetsAt.Equal(reset5h) ||
		rooms[1].Window != "7d" || rooms[1].Used != 0.6 || !rooms[1].ResetsAt.Equal(reset7d) {
		t.Fatalf("claude rooms = %+v", rooms)
	}
	if left := RoomLeft(rooms, roomTestNow); left < 0.399 || left > 0.401 {
		t.Fatalf("claude room left = %v, want 0.4 (the 7d window is the worst)", left)
	}
	if got := WeeklyReset(rooms, roomTestNow); !got.Equal(reset7d) {
		t.Fatalf("claude weekly reset = %v, want %v", got, reset7d)
	}

	codex := codexAuth("x", 80, reset5h, 30, reset7d, roomTestNow)
	rooms = RoomsFromQuota("codex", codex.Quota)
	if len(rooms) != 2 || rooms[0].Window != "primary" || rooms[0].Used != 0.8 || rooms[1].Window != "secondary" || rooms[1].Used != 0.3 {
		t.Fatalf("codex rooms = %+v", rooms)
	}
	if left := RoomLeft(rooms, roomTestNow); left < 0.199 || left > 0.201 {
		t.Fatalf("codex room left = %v, want 0.2 (the primary window is the worst)", left)
	}
	if got := WeeklyReset(rooms, roomTestNow); !got.Equal(reset7d) {
		t.Fatalf("codex weekly reset = %v, want the secondary reset %v", got, reset7d)
	}
}

func TestRoomLeftTreatsAPassedResetAsEmptyWindow(t *testing.T) {
	auth := claudeAuth("c", 0.95, roomTestNow.Add(-time.Minute), 0.5, roomTestNow.Add(24*time.Hour), roomTestNow.Add(-5*time.Hour))
	if left := RoomLeft(RoomsFromQuota("claude", auth.Quota), roomTestNow); left != 0.5 {
		t.Fatalf("room left = %v, want 0.5: the 5h window reset a minute ago", left)
	}
}

func TestMostRoomPicksTheBestWorstWindow(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	later := roomTestNow.Add(3 * time.Hour)
	week := roomTestNow.Add(96 * time.Hour)
	// a has the emptiest 5h window but its weekly window is nearly spent.
	a := claudeAuth("a", 0.01, later, 0.90, week, roomTestNow.Add(-time.Minute))
	b := claudeAuth("b", 0.40, later, 0.30, week, roomTestNow.Add(-time.Minute))
	c := claudeAuth("c", 0.20, later, 0.55, week, roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, a, b, c); got != "b" {
		t.Fatalf("most-room picked %q, want b (worst windows: a 0.10, b 0.60, c 0.45)", got)
	}
}

func TestMostRoomSkipsAFullWeeklyWindow(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	full := claudeAuth("claude_2", 0.0, roomTestNow.Add(time.Hour), 1.0, roomTestNow.Add(20*time.Hour), roomTestNow.Add(-time.Minute))
	other := claudeAuth("claude_3", 0.7, roomTestNow.Add(time.Hour), 0.8, roomTestNow.Add(100*time.Hour), roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, full, other); got != "claude_3" {
		t.Fatalf("most-room picked %q, want claude_3 over a credential with its weekly window spent", got)
	}
}

func TestMostRoomCountsAnUnheardCredentialAsFull(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	heard := claudeAuth("heard", 0.0, roomTestNow.Add(time.Hour), 0.01, roomTestNow.Add(100*time.Hour), roomTestNow.Add(-time.Minute))
	unheard := &Auth{ID: "unheard", Provider: "claude"}
	if got := pickRoom(t, selector, heard, unheard); got != "unheard" {
		t.Fatalf("most-room picked %q, want the credential with no observation (full room)", got)
	}
}

func TestMostRoomBreaksTiesByLeastRecentlyUsed(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	reset := roomTestNow.Add(time.Hour)
	week := roomTestNow.Add(100 * time.Hour)
	recent := claudeAuth("a-recent", 0.2, reset, 0.2, week, roomTestNow.Add(-time.Minute))
	older := claudeAuth("b-older", 0.2, reset, 0.2, week, roomTestNow.Add(-time.Hour))
	if got := pickRoom(t, selector, recent, older); got != "b-older" {
		t.Fatalf("most-room tie picked %q, want the least recently used b-older", got)
	}
	// Picks before any response arrives count as use, so concurrent new sessions spread out.
	if got := pickRoom(t, selector, recent, older); got != "a-recent" {
		t.Fatalf("second tie pick = %q, want a-recent now that b-older was just picked", got)
	}
}

func TestMostRoomAppliesToCodexCredentials(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	week := roomTestNow.Add(100 * time.Hour)
	busy := codexAuth("codex-busy", 70, roomTestNow.Add(time.Hour), 10, week, roomTestNow.Add(-time.Minute))
	idle := codexAuth("codex-idle", 5, roomTestNow.Add(time.Hour), 40, week, roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, busy, idle); got != "codex-idle" {
		t.Fatalf("most-room picked %q, want codex-idle (worst windows 0.30 vs 0.60)", got)
	}
}

func TestSoonestResetPicksTheNearestWeeklyReset(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategySoonestReset)
	hour := roomTestNow.Add(time.Hour)
	// soon has less room than late, but its weekly window resets first.
	soon := claudeAuth("soon", 0.5, hour, 0.7, roomTestNow.Add(30*time.Hour), roomTestNow.Add(-time.Minute))
	late := claudeAuth("late", 0.0, hour, 0.1, roomTestNow.Add(120*time.Hour), roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, late, soon); got != "soon" {
		t.Fatalf("soonest-reset picked %q, want soon", got)
	}
}

func TestSoonestResetSkipsACredentialWithNoRoomLeft(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategySoonestReset)
	hour := roomTestNow.Add(time.Hour)
	spent := claudeAuth("spent", 0.0, hour, 1.0, roomTestNow.Add(10*time.Hour), roomTestNow.Add(-time.Minute))
	open := claudeAuth("open", 0.3, hour, 0.4, roomTestNow.Add(60*time.Hour), roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, spent, open); got != "open" {
		t.Fatalf("soonest-reset picked %q, want open: spent resets first but has no room", got)
	}
}

func TestSoonestResetPrefersAKnownResetAndFallsBackToMostRoom(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategySoonestReset)
	known := claudeAuth("known", 0.6, roomTestNow.Add(time.Hour), 0.6, roomTestNow.Add(150*time.Hour), roomTestNow.Add(-time.Minute))
	unheard := &Auth{ID: "unheard", Provider: "claude"}
	if got := pickRoom(t, selector, unheard, known); got != "known" {
		t.Fatalf("soonest-reset picked %q, want the credential with a known weekly reset", got)
	}

	// No weekly reset known anywhere: most room decides.
	fiveHourOnly := observedAuth("five-hour-only", "claude", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.8",
		"anthropic-ratelimit-unified-5h-reset":       epoch(roomTestNow.Add(time.Hour)),
	}, roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, fiveHourOnly, unheard); got != "unheard" {
		t.Fatalf("soonest-reset without known resets picked %q, want unheard (most room)", got)
	}
}

func TestRoomSelectorSkipsCredentialsInCooldown(t *testing.T) {
	selector := newTestRoomSelector(RoomStrategyMostRoom)
	cooling := &Auth{ID: "cooling", Provider: "claude", Unavailable: true, NextRetryAfter: roomTestNow.Add(time.Hour), Quota: QuotaState{Exceeded: true, NextRecoverAt: roomTestNow.Add(time.Hour)}}
	ready := claudeAuth("ready", 0.9, roomTestNow.Add(time.Hour), 0.9, roomTestNow.Add(100*time.Hour), roomTestNow.Add(-time.Minute))
	if got := pickRoom(t, selector, cooling, ready); got != "ready" {
		t.Fatalf("most-room picked %q, want ready: cooling is in cooldown", got)
	}
}

func TestSetRoomStrategyKeepsTheSessionAffinitySelector(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: NewRoomSelector(RoomStrategyMostRoom), TTL: time.Hour})
	manager.SetSelector(affinity)
	if !manager.SetRoomStrategy(RoomStrategySoonestReset) {
		t.Fatal("SetRoomStrategy() = false, want true")
	}
	if manager.Selector() != affinity {
		t.Fatal("SetRoomStrategy replaced the session-affinity selector")
	}
	if got := affinity.fallback.(*RoomSelector).Strategy(); got != RoomStrategySoonestReset {
		t.Fatalf("strategy = %q, want %q", got, RoomStrategySoonestReset)
	}
	manager.SetSelector(&RoundRobinSelector{})
	if manager.SetRoomStrategy(RoomStrategyMostRoom) {
		t.Fatal("SetRoomStrategy() = true for a round-robin selector")
	}
}
