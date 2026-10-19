package buffcloud

import (
	"reflect"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestSummariseCountsHourAndDayPerAccountAndInTotal(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	ms := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	opus, sol := ptr("claude-opus-5-5"), ptr("gpt-6.1-sol")
	rows := []Row{
		// Yesterday: in neither count, but the latest of nothing yet.
		{At: dayStart.Add(-time.Minute).UnixMilli(), AccountID: "a", Status: 200, Model: opus, Tokens: &Tokens{Input: 1000}},
		// Today, before the last hour.
		{At: ms(3 * time.Hour), AccountID: "a", Status: 200, Model: opus, Tokens: &Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}},
		// Exactly one hour ago counts as the last hour.
		{At: ms(time.Hour), AccountID: "b", Status: 200, Model: sol, Tokens: &Tokens{Input: 1, Output: 2, CacheRead: 3}},
		// A failure without usage is a request with no tokens.
		{At: ms(10 * time.Minute), AccountID: "a", Status: 429},
		// An account no longer configured counts only in the totals.
		{At: ms(5 * time.Minute), AccountID: "gone", Status: 200, Tokens: &Tokens{Output: 5}},
	}
	accounts := []Account{
		{ID: "a", Provider: "anthropic", Label: "Claude 1", Enabled: true},
		{ID: "b", Provider: "openai", Label: "Codex", Enabled: true},
	}

	s := Summarise(accounts, rows, now, dayStart)

	want := func(requests int, split Tokens) Count {
		return Count{Requests: requests, Tokens: split.Total(), Split: split}
	}
	checks := []struct {
		name      string
		got, want Count
	}{
		{"a last hour", s.Accounts[0].LastHour, want(1, Tokens{})},
		{"a today", s.Accounts[0].Today, want(2, Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40})},
		{"b last hour", s.Accounts[1].LastHour, want(1, Tokens{Input: 1, Output: 2, CacheRead: 3})},
		{"b today", s.Accounts[1].Today, want(1, Tokens{Input: 1, Output: 2, CacheRead: 3})},
		{"total last hour", s.LastHour, want(3, Tokens{Input: 1, Output: 7, CacheRead: 3})},
		{"total today", s.Today, want(4, Tokens{Input: 11, Output: 27, CacheRead: 33, CacheWrite: 40})},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if s.Today.Tokens != 111 {
		t.Errorf("today's tokens = %d, want 111 (input + output + cache read + cache write)", s.Today.Tokens)
	}
	if got := *s.TodayByModel["claude-opus-5-5"]; got != want(1, Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}) {
		t.Errorf("opus today = %+v", got)
	}
	if got := s.TodayByModel[UnknownModel].Requests; got != 2 {
		t.Errorf("rows without a model today = %d, want 2", got)
	}
	if want := (LastRequest{AccountID: "gone", At: ms(5 * time.Minute), Status: 200}); s.Last == nil || *s.Last != want {
		t.Errorf("last = %+v, want %+v", s.Last, want)
	}
}

func TestSummariseRoomsAndCooldownAsTheRouterSeesThem(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	future, past := now.Add(2*time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli()
	accounts := []Account{{
		ID: "codex", Provider: "openai",
		Rooms: []Room{
			{Window: "primary", Used: 0.25, ResetsAt: &future},
			// Its reset has passed: full again, reset no longer shown.
			{Window: "secondary", Used: 1, ResetsAt: &past},
		},
		CooldownUntil: &past,
		LastStatus:    ptr(429),
	}, {
		ID: "claude", Provider: "anthropic",
		Rooms:         []Room{{Window: "7d", Used: 1.2}},
		CooldownUntil: &future,
	}}

	s := Summarise(accounts, nil, now, now)

	codexRooms := []RoomView{{Window: "5h", Left: 0.75, ResetsAt: &future}, {Window: "7d", Left: 1}}
	if !reflect.DeepEqual(s.Accounts[0].Rooms, codexRooms) {
		t.Errorf("codex rooms = %+v, want %+v", s.Accounts[0].Rooms, codexRooms)
	}
	if s.Accounts[0].CooldownUntil != nil {
		t.Errorf("a cooldown that has ended is still shown: %d", *s.Accounts[0].CooldownUntil)
	}
	if s.Accounts[0].LastStatus == nil || *s.Accounts[0].LastStatus != 429 {
		t.Errorf("last status = %v, want 429", s.Accounts[0].LastStatus)
	}
	if got := s.Accounts[1].Rooms; len(got) != 1 || got[0].Left != 0 {
		t.Errorf("overspent window = %+v, want room 0", got)
	}
	if s.Accounts[1].CooldownUntil == nil || *s.Accounts[1].CooldownUntil != future {
		t.Errorf("running cooldown = %v, want %d", s.Accounts[1].CooldownUntil, future)
	}
	if s.Last != nil || s.Today.Requests != 0 {
		t.Errorf("no rows gave last=%v today=%+v", s.Last, s.Today)
	}
}
