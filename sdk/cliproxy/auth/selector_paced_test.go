package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The headers below have the shape of real Claude responses recorded by the
// proxy (quota-signals.state): fractions for -utilization, epoch seconds for
// -reset. Times are relative to the wall clock, because the session-affinity
// selector checks availability against time.Now.

type pacedFixture struct {
	t        *testing.T
	now      time.Time
	room     *RoomSelector
	affinity *SessionAffinitySelector
}

func newPacedFixture(t *testing.T) *pacedFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	room := NewRoomSelector(RoomStrategyPaced)
	room.now = func() time.Time { return now }
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: room, TTL: time.Hour})
	t.Cleanup(affinity.cache.Stop)
	return &pacedFixture{t: t, now: now, room: room, affinity: affinity}
}

// account builds a Claude account with the given 5h and 7d use and the hours
// until each window resets.
func (f *pacedFixture) account(id string, used5h, hours5h, used7d, hours7d float64) *Auth {
	at := func(hours float64) time.Time { return f.now.Add(time.Duration(hours * float64(time.Hour))) }
	return claudeAuth(id, used5h, at(hours5h), used7d, at(hours7d), f.now.Add(-time.Minute))
}

// session sends one request of a Claude Code session and returns the account it went to.
func (f *pacedFixture) session(id, model string, auths ...*Auth) string {
	f.t.Helper()
	got, err := f.affinity.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{
		Headers: http.Header{"X-Claude-Code-Session-Id": []string{id}},
	}, auths)
	if err != nil {
		f.t.Fatalf("Pick(%s) error = %v", id, err)
	}
	return got.ID
}

func (f *pacedFixture) spread(prefix string, sessions int, auths ...*Auth) map[string]int {
	f.t.Helper()
	counts := map[string]int{}
	for i := 0; i < sessions; i++ {
		counts[f.session(fmt.Sprintf("%s-%d", prefix, i), "claude-opus-5-5", auths...)]++
	}
	return counts
}

func setUsed5h(auth *Auth, used float64, now time.Time) {
	auth.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = fmt.Sprintf("%g", used)
	auth.Quota.ObservedAt = now
}

func TestPacedSpreadsParallelSessionsOverEqualAccounts(t *testing.T) {
	f := newPacedFixture(t)
	a := f.account("a", 0.1, 3, 0.4, 72)
	b := f.account("b", 0.1, 3, 0.4, 72)
	c := f.account("c", 0.1, 3, 0.4, 72)
	counts := f.spread("par", 12, a, b, c)
	for _, id := range []string{"a", "b", "c"} {
		if counts[id] != 4 {
			t.Fatalf("12 sessions over 3 equal accounts = %v, want 4 each", counts)
		}
	}
	if active := f.room.ActiveSessions(); active["a"] != 4 || active["b"] != 4 || active["c"] != 4 {
		t.Fatalf("ActiveSessions() = %v, want 4 each", active)
	}
}

func TestPacedGivesMoreSessionsToTheWeekThatEndsSooner(t *testing.T) {
	f := newPacedFixture(t)
	// Same room, but soon's week ends in 24 h and late's in 120 h: soon should
	// spend about five times as fast to reach 0% by its reset.
	soon := f.account("soon", 0.1, 3, 0.5, 24)
	late := f.account("late", 0.1, 3, 0.5, 120)
	counts := f.spread("pace", 12, soon, late)
	if counts["soon"] < 9 || counts["late"] < 1 {
		t.Fatalf("sessions = %v, want most on soon and still some on late", counts)
	}
}

func TestPacedKeepsALowWeekAlive(t *testing.T) {
	f := newPacedFixture(t)
	// low has 10% of its week left with 5 days to go; it should get the fewest
	// sessions, not be spent first as soonest-reset would (its week ends first).
	low := f.account("low", 0.0, 3, 0.9, 110)
	full := f.account("full", 0.0, 3, 0.3, 120)
	counts := f.spread("alive", 8, low, full)
	if counts["low"] > 1 {
		t.Fatalf("sessions = %v, want at most 1 on the low account", counts)
	}
	soonest := NewRoomSelector(RoomStrategySoonestReset)
	if got, _ := soonest.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{low, full}); got.ID != "low" {
		t.Fatalf("soonest-reset picked %s; the test assumes it would spend low first", got.ID)
	}
}

func TestPacedSendsNoNewSessionToALowFiveHourAccount(t *testing.T) {
	f := newPacedFixture(t)
	// hot has the best weekly pace by far but only 15% of its 5h window left.
	hot := f.account("hot", 0.85, 2, 0.2, 10)
	cool := f.account("cool", 0.3, 2, 0.5, 150)
	counts := f.spread("new", 5, hot, cool)
	if counts["hot"] != 0 {
		t.Fatalf("sessions = %v, want none on hot (15%% of 5h left)", counts)
	}
}

func TestPacedMovesABoundSessionBeforeTheFiveHourLimit(t *testing.T) {
	f := newPacedFixture(t)
	a := f.account("a", 0.5, 2, 0.3, 30)
	b := f.account("b", 0.5, 2, 0.3, 150)
	if got := f.session("s1", "claude-opus-5-5", a, b); got != "a" {
		t.Fatalf("first request went to %s, want a (its week ends sooner)", got)
	}
	setUsed5h(a, 0.85, f.now)
	if got := f.session("s1", "claude-opus-5-5", a, b); got != "a" {
		t.Fatalf("at 15%% of 5h left the session moved to %s; it should keep its cache on a", got)
	}
	setUsed5h(a, 0.92, f.now)
	if got := f.session("s1", "claude-opus-5-5", a, b); got != "b" {
		t.Fatalf("at 8%% of 5h left the session stayed on %s, want a move to b", got)
	}
	setUsed5h(a, 0.0, f.now)
	if got := f.session("s1", "claude-opus-5-5", a, b); got != "b" {
		t.Fatalf("after the move the session went back to %s; it should stay on b", got)
	}
}

func TestPacedMovesSessionsAtOnceAfterA429(t *testing.T) {
	f := newPacedFixture(t)
	a := f.account("a", 0.6, 2, 0.3, 30)
	b := f.account("b", 0.2, 2, 0.4, 100)
	c := f.account("c", 0.7, 2, 0.4, 100)
	for i := 0; i < 3; i++ {
		if got := f.session(fmt.Sprintf("s%d", i), "claude-opus-5-5", a, b, c); got != "a" && i == 0 {
			t.Fatalf("first session went to %s, want a", got)
		}
	}
	// a answers 429 on its 5h window: MarkResult puts it in cooldown until the reset.
	a.Unavailable = true
	a.NextRetryAfter = f.now.Add(2 * time.Hour)
	a.Quota.Exceeded = true
	a.Quota.NextRecoverAt = f.now.Add(2 * time.Hour)
	a.LastError = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit_error"}
	if got := f.session("s0", "claude-opus-5-5", a, b, c); got == "a" {
		t.Fatal("after a 429 the session stayed on a")
	} else if got != "b" {
		t.Fatalf("after a 429 the session moved to %s, want b (more 5h room than c)", got)
	}
}

func TestPacedWithEveryAccountLowTakesTheMostRoom(t *testing.T) {
	f := newPacedFixture(t)
	a := f.account("a", 0.95, 1, 0.5, 50)
	b := f.account("b", 0.88, 4, 0.5, 50)
	c := f.account("c", 0.97, 2, 0.5, 50)
	if got := f.session("low-1", "claude-opus-5-5", a, b, c); got != "b" {
		t.Fatalf("every account low: picked %s, want b (most 5h room)", got)
	}
	// Nothing is dropped when no account has room, so a bound session stays.
	if got := f.session("low-1", "claude-opus-5-5", a, b, c); got != "b" {
		t.Fatalf("every account low: bound session moved to %s", got)
	}
	if kept, dropped := dropNearLimit([]*Auth{a, b, c}, "claude-opus-5-5", f.now); len(kept) != 3 || dropped != nil {
		t.Fatalf("dropNearLimit with every account low = %d kept, %v dropped", len(kept), dropped)
	}
}

func TestPacedHonoursPerModelWindows(t *testing.T) {
	f := newPacedFixture(t)
	a := f.account("a", 0.1, 3, 0.3, 30)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_opus-Utilization"] = "0.95"
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_opus-Reset"] = epoch(f.now.Add(30 * time.Hour))
	b := f.account("b", 0.1, 3, 0.3, 150)
	if got := f.session("opus", "claude-opus-5-5", a, b); got != "b" {
		t.Fatalf("opus went to %s, want b: a's 7d_opus window is at 95%%", got)
	}
	if got := f.session("sonnet", "claude-sonnet-5-5", a, b); got != "a" {
		t.Fatalf("sonnet went to %s, want a: the opus window does not bind sonnet", got)
	}
}

func TestPaceOfCountsTheReserveAndAnUnheardAccount(t *testing.T) {
	now := time.Now()
	a := claudeAuth("a", 0.1, now.Add(time.Hour), 0.6, now.Add(40*time.Hour), now)
	withReserve(a, 0.3, nil)
	pace := PaceOf(a, "claude-opus-5-5", now)
	if pace.Week < 0.099 || pace.Week > 0.101 || pace.FiveHour < 0.599 || pace.FiveHour > 0.601 {
		t.Fatalf("pace with a 30%% reserve = %+v, want week 0.1 and 5h 0.6", pace)
	}
	if rate := pace.TargetRate; rate < 0.0024 || rate > 0.0026 {
		t.Fatalf("target rate = %v, want 0.1/40h", rate)
	}
	unheard := PaceOf(&Auth{ID: "new", Provider: "claude"}, "", now)
	if unheard.FiveHour != 1 || unheard.Week != 1 || unheard.TargetRate != 1.0/168 {
		t.Fatalf("unheard pace = %+v, want full with a 7-day week", unheard)
	}
}

func TestSetRoomStrategySwitchesToPaced(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: NewRoomSelector(RoomStrategySoonestReset), TTL: time.Hour})
	t.Cleanup(affinity.cache.Stop)
	manager.SetSelector(affinity)
	if manager.ActiveSessions() == nil {
		t.Fatal("ActiveSessions() = nil for a room selector")
	}
	if !manager.SetRoomStrategy(RoomStrategyPaced) || affinity.pacedSelector() == nil {
		t.Fatal("SetRoomStrategy(paced) did not switch in place")
	}
}

func TestPacedSpendsTheLastOfAWeekButLeavesASpentOne(t *testing.T) {
	f := newPacedFixture(t)
	// ending has 15% of its week left and 6 h to its reset: no floor on the
	// weekly window, so it keeps getting work and ends its week near 0%.
	ending := f.account("ending", 0.1, 3, 0.85, 6)
	other := f.account("other", 0.1, 3, 0.5, 150)
	if got := f.session("end-1", "claude-opus-5-5", ending, other); got != "ending" {
		t.Fatalf("new session went to %s, want ending (15%% of its week left, 6 h to go)", got)
	}
	// With 1% of its week left the account counts as spent: its session moves.
	ending.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0.99"
	if got := f.session("end-1", "claude-opus-5-5", ending, other); got != "other" {
		t.Fatalf("with its week spent the session stayed on %s, want a move to other", got)
	}
}

func TestPacedRechecksAStaleSpentWeekWithOneSession(t *testing.T) {
	f := newPacedFixture(t)
	spent := f.account("spent", 0.0, 3, 1.0, 60)
	spent.Quota.ObservedAt = f.now.Add(-4 * time.Hour)
	roomy := f.account("roomy", 0.1, 3, 0.4, 100)
	if kept, _ := dropNearLimit([]*Auth{spent, roomy}, "claude-opus-5-5", f.now); len(kept) != 2 {
		t.Fatalf("dropNearLimit dropped an account due a recheck: kept %d", len(kept))
	}
	if got := f.session("probe-1", "claude-opus-5-5", spent, roomy); got != "spent" {
		t.Fatalf("first new session went to %s, want spent: its reading is 4 h old", got)
	}
	for i := 2; i < 5; i++ {
		if got := f.session(fmt.Sprintf("probe-%d", i), "claude-opus-5-5", spent, roomy); got != "roomy" {
			t.Fatalf("session %d went to %s; one recheck per 3 h is enough", i, got)
		}
	}
	fresh := f.account("fresh-spent", 0.0, 3, 1.0, 60)
	if got := f.session("probe-fresh", "claude-opus-5-5", fresh, roomy); got != "roomy" {
		t.Fatalf("a fresh spent reading got a session (%s)", got)
	}
}
