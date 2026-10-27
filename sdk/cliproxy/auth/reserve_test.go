package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// withReserve sets the reserve the way PATCH /buff/reserve writes it.
func withReserve(auth *Auth, all float64, windows map[string]float64) *Auth {
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	value := map[string]any{"all": all}
	if windows != nil {
		w := map[string]any{}
		for k, v := range windows {
			w[k] = v
		}
		value["windows"] = w
	}
	auth.Metadata[ReserveMetadataKey] = value
	return auth
}

func pickRoomErr(selector *RoomSelector, model string, auths ...*Auth) (*Auth, error) {
	return selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
}

func TestReserveRouting(t *testing.T) {
	now := roomTestNow
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(50 * time.Hour)

	cases := []struct {
		name    string
		used7d  float64
		reserve float64
		held    bool
	}{
		{"below the threshold", 0.79, 0.2, false},
		{"at the threshold", 0.8, 0.2, true},
		{"above the threshold", 0.93, 0.2, true},
		{"no reserve set", 0.99, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// a has more room in its worst window than b, so most-room picks a unless a is held.
			a := claudeAuth("a", 0.1, reset5h, tc.used7d, reset7d, now)
			if tc.reserve > 0 {
				withReserve(a, tc.reserve, nil)
			}
			b := claudeAuth("b", 0.995, reset5h, 0.1, reset7d, now)
			got := pickRoom(t, newTestRoomSelector(RoomStrategyMostRoom), a, b)
			want := "a"
			if tc.held {
				want = "b"
			}
			if got != want {
				t.Fatalf("picked %s, want %s", got, want)
			}
			if held := ReserveHeld(a, "", now); held != tc.held {
				t.Fatalf("ReserveHeld = %v, want %v", held, tc.held)
			}
		})
	}
}

func TestReserveEndsWhenTheWindowResets(t *testing.T) {
	reset7d := roomTestNow.Add(-time.Minute)
	a := withReserve(claudeAuth("a", 0.1, roomTestNow.Add(time.Hour), 0.95, reset7d, roomTestNow.Add(-time.Hour)), 0.2, nil)
	if ReserveHeld(a, "", roomTestNow) {
		t.Fatal("a window whose reset has passed still holds the account")
	}
	if blocked, reason, until := isAuthBlockedForModel(a, "", reset7d.Add(-time.Second)); !blocked || reason != blockReasonReserve || !until.Equal(reset7d) {
		t.Fatalf("before the reset: blocked=%v reason=%v until=%v", blocked, reason, until)
	}
}

func TestReserveEveryAccountHeldNamesTheReserves(t *testing.T) {
	now := roomTestNow
	a := withReserve(claudeAuth("a", 0.85, now.Add(2*time.Hour), 0.3, now.Add(50*time.Hour), now), 0.2, nil)
	a.Label = "Claude 4"
	b := withReserve(claudeAuth("b", 0.1, now.Add(2*time.Hour), 0.75, now.Add(30*time.Hour), now), 0.25, nil)
	b.Label = "Claude 5"
	_, err := pickRoomErr(newTestRoomSelector(RoomStrategyMostRoom), "claude-opus-5", a, b)
	var reserveErr *reserveError
	if !errors.As(err, &reserveErr) {
		t.Fatalf("error = %v, want a reserve error", err)
	}
	if reserveErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", reserveErr.StatusCode())
	}
	message := err.Error()
	for _, want := range []string{"claude-opus-5", "Claude 4 keeps 20% of 5h free (85% used", "Claude 5 keeps 25% of 7d free (75% used", "Buff Cloud"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	if got := reserveErr.Headers().Get("Retry-After"); got != "7200" {
		t.Fatalf("Retry-After = %q, want the first reset, 7200", got)
	}
}

func TestReserveHeldAndCoolingAccounts(t *testing.T) {
	now := roomTestNow
	a := withReserve(claudeAuth("a", 0.1, now.Add(2*time.Hour), 0.9, now.Add(50*time.Hour), now), 0.2, nil)
	b := claudeAuth("b", 0.1, now.Add(2*time.Hour), 0.1, now.Add(50*time.Hour), now)
	b.Quota.Exceeded, b.Quota.NextRecoverAt = true, now.Add(time.Hour)
	_, err := getAvailableAuths([]*Auth{a, b}, "claude", "", now)
	if err == nil || !strings.Contains(err.Error(), "1 other account(s) are cooling down") {
		t.Fatalf("error = %v", err)
	}
	// With no reserve in the way the real cooldown error stays as it was.
	_, err = getAvailableAuths([]*Auth{b}, "claude", "", now)
	var reserveErr *reserveError
	if err == nil || errors.As(err, &reserveErr) {
		t.Fatalf("cooldown alone: error = %v", err)
	}
}

func TestReservePerWindowOverride(t *testing.T) {
	now := roomTestNow
	// 20% everywhere, but the weekly window may be used up to 95%.
	a := withReserve(claudeAuth("a", 0.5, now.Add(time.Hour), 0.9, now.Add(50*time.Hour), now), 0.2, map[string]float64{"7d": 0.05})
	if ReserveHeld(a, "", now) {
		t.Fatal("7d at 90% is held although its own reserve is 5%")
	}
	a = withReserve(claudeAuth("a", 0.5, now.Add(time.Hour), 0.96, now.Add(50*time.Hour), now), 0.2, map[string]float64{"7d": 0.05})
	if !ReserveHeld(a, "", now) {
		t.Fatal("7d at 96% is not held with a 5% reserve")
	}
	// Only the 5h window is reserved.
	a = withReserve(claudeAuth("a", 0.7, now.Add(time.Hour), 0.99, now.Add(50*time.Hour), now), 0, map[string]float64{"5h": 0.3})
	holds := ReserveHolds(a, "", now)
	if len(holds) != 1 || holds[0].Window != "5h" {
		t.Fatalf("holds = %+v, want 5h only", holds)
	}
}

func TestReserveCodexPercents(t *testing.T) {
	now := roomTestNow
	a := withReserve(codexAuth("a", 10, now.Add(time.Hour), 80, now.Add(50*time.Hour), now), 0.2, nil)
	holds := ReserveHolds(a, "gpt-6.1-sol", now)
	if len(holds) != 1 || holds[0].Window != "secondary" || holds[0].Used != 0.8 {
		t.Fatalf("holds = %+v", holds)
	}
}

func TestReservePerModelWindow(t *testing.T) {
	now := roomTestNow
	a := claudeAuth("a", 0.1, now.Add(time.Hour), 0.2, now.Add(50*time.Hour), now)
	a.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization":      {"0.1"},
		"Anthropic-Ratelimit-Unified-5h-Reset":            {epoch(now.Add(time.Hour))},
		"Anthropic-Ratelimit-Unified-7d-Utilization":      {"0.2"},
		"Anthropic-Ratelimit-Unified-7d-Reset":            {epoch(now.Add(50 * time.Hour))},
		"Anthropic-Ratelimit-Unified-7d_opus-Utilization": {"0.9"},
		"Anthropic-Ratelimit-Unified-7d_opus-Reset":       {epoch(now.Add(40 * time.Hour))},
	}, now)
	withReserve(a, 0.2, nil)
	if !ReserveHeld(a, "claude-opus-5", now) {
		t.Fatal("an Opus request is not held by the 7d_opus window")
	}
	if ReserveHeld(a, "claude-sonnet-5", now) || ReserveHeld(a, "", now) {
		t.Fatal("the 7d_opus window holds requests for other models")
	}
}

func TestReserveReadingWithoutResetLastsOneWindow(t *testing.T) {
	observed := roomTestNow.Add(-6 * time.Hour)
	a := observedAuth("a", "codex", map[string]string{
		"x-codex-primary-used-percent":   "95",
		"x-codex-primary-window-minutes": "300",
	}, observed)
	withReserve(a, 0.2, nil)
	if ReserveHeld(a, "", roomTestNow) {
		t.Fatal("a 5 h reading taken 6 h ago still holds the account")
	}
	if !ReserveHeld(a, "", observed.Add(4*time.Hour)) {
		t.Fatal("a 5 h reading taken 4 h ago does not hold the account")
	}
}

func TestReserveRebindsASession(t *testing.T) {
	now := time.Now()
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	a := withReserve(claudeAuth("a", 0.1, now.Add(time.Hour), 0.5, now.Add(50*time.Hour), now), 0.2, nil)
	b := claudeAuth("b", 0.1, now.Add(time.Hour), 0.5, now.Add(50*time.Hour), now)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-reserve"}}}
	first, err := selector.Pick(context.Background(), "claude", "claude-opus-5", opts, []*Auth{a, b})
	if err != nil || first.ID != "a" {
		t.Fatalf("first pick = %v, %v", first, err)
	}
	// The session's account reaches its reserve: the next turn moves.
	a.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.81"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {epoch(now.Add(50 * time.Hour))},
	}, now)
	next, err := selector.Pick(context.Background(), "claude", "claude-opus-5", opts, []*Auth{a, b})
	if err != nil || next.ID != "b" {
		t.Fatalf("pick after the reserve = %v, %v", next, err)
	}
}

func TestParseReserve(t *testing.T) {
	good, err := ParseReserve([]byte(`{"all":0.2,"windows":{"7d":0.3,"5h":0}}`))
	if err != nil || good.For("7d") != 0.3 || good.For("5h") != 0 || good.For("primary") != 0.2 {
		t.Fatalf("ParseReserve = %+v, %v", good, err)
	}
	for _, bad := range []string{`{"all":1}`, `{"all":-0.1}`, `{"windows":{"7d":1.5}}`, `{"windows":{"":0.2}}`, `{"percent":20}`, `[]`} {
		if _, err := ParseReserve([]byte(bad)); err == nil {
			t.Fatalf("ParseReserve(%s) accepted", bad)
		}
	}
	if !ReserveOf(&Auth{Metadata: map[string]any{ReserveMetadataKey: map[string]any{"all": 2}}}).IsZero() {
		t.Fatal("a malformed reserve in the auth file holds the account")
	}
}
