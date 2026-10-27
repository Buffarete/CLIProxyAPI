package auth

// Buff Cloud fork: a usage reserve per credential. Will keeps part of an
// account's limit free for himself: with a 20% reserve the router treats the
// account as full once any of its windows reaches 80% used, and it is
// available again when that window resets. The reserve lives in the auth file
// (metadata key buff_reserve), beside the account's other settings.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ReserveMetadataKey is the auth file key that holds the reserve.
const ReserveMetadataKey = "buff_reserve"

// Reserve is the share of each usage window kept free, 0 to 1.
type Reserve struct {
	// All applies to every window that has no value of its own.
	All float64 `json:"all,omitempty"`
	// Windows sets one window by its name ("5h", "7d", "primary", "secondary",
	// or a per-model window such as "7d_opus"). 0 turns the reserve off for it.
	Windows map[string]float64 `json:"windows,omitempty"`
}

// IsZero reports whether the reserve holds nothing back.
func (r Reserve) IsZero() bool {
	if r.All > 0 {
		return false
	}
	for _, v := range r.Windows {
		if v > 0 {
			return false
		}
	}
	return true
}

// For is the reserve of one window.
func (r Reserve) For(window string) float64 {
	if v, ok := r.Windows[window]; ok {
		return v
	}
	return r.All
}

// ParseReserve reads and checks a reserve from JSON. Each value must be at
// least 0 and below 1: a 100% reserve would be the account's Enabled switch.
func ParseReserve(raw []byte) (Reserve, error) {
	var r Reserve
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return Reserve{}, fmt.Errorf("reserve must be {\"all\": 0.2, \"windows\": {\"7d\": 0.3}}: %v", err)
	}
	if err := checkReserveValue("all", r.All); err != nil {
		return Reserve{}, err
	}
	for window, v := range r.Windows {
		if strings.TrimSpace(window) == "" {
			return Reserve{}, fmt.Errorf("reserve window name is empty")
		}
		if err := checkReserveValue(window, v); err != nil {
			return Reserve{}, err
		}
	}
	if len(r.Windows) == 0 {
		r.Windows = nil
	}
	return r, nil
}

func checkReserveValue(name string, v float64) error {
	if math.IsNaN(v) || v < 0 || v >= 1 {
		return fmt.Errorf("reserve %s must be from 0 to below 1, got %v", name, v)
	}
	return nil
}

// ReserveOf reads a credential's reserve. A missing or malformed value is no
// reserve, so a hand-edited file never stops an account by accident.
func ReserveOf(auth *Auth) Reserve {
	if auth == nil || auth.Metadata == nil {
		return Reserve{}
	}
	raw, ok := auth.Metadata[ReserveMetadataKey]
	if !ok || raw == nil {
		return Reserve{}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Reserve{}
	}
	r, err := ParseReserve(encoded)
	if err != nil {
		return Reserve{}
	}
	return r
}

// ReserveHold is a window that has reached the part Will keeps free.
type ReserveHold struct {
	Window  string
	Used    float64
	Reserve float64
	// Until is when the window resets and the hold ends.
	Until time.Time
}

// claudeWindowUtilization matches every Claude window, including per-model
// ones such as anthropic-ratelimit-unified-7d_opus-utilization.
var claudeWindowUtilization = regexp.MustCompile(`^Anthropic-Ratelimit-Unified-(.+)-Utilization$`)

// reserveRooms are the windows a reserve applies to: RoomsFromQuota's, plus
// Claude's per-model windows, which only bind requests for that model family.
func reserveRooms(provider string, quota QuotaState) []Room {
	rooms := RoomsFromQuota(provider, quota)
	if !strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return rooms
	}
	var extra []Room
	for key := range quota.Signals {
		match := claudeWindowUtilization.FindStringSubmatch(http.CanonicalHeaderKey(key))
		if match == nil {
			continue
		}
		window := strings.ToLower(match[1])
		if window == "5h" || window == "7d" || !strings.Contains(window, "_") {
			continue
		}
		prefix := "anthropic-ratelimit-unified-" + window
		used, ok := parseSignalFloat(quota.Signals, prefix+"-utilization")
		if !ok {
			continue
		}
		extra = append(extra, Room{Window: window, Used: used, ResetsAt: parseSignalEpoch(quota.Signals, prefix+"-reset")})
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Window < extra[j].Window })
	return append(rooms, extra...)
}

// reserveWindowModel is the model family a per-model window binds ("opus" for
// "7d_opus"), or "" for a window that binds every request.
func reserveWindowModel(window string) string {
	if _, family, ok := strings.Cut(window, "_"); ok {
		return family
	}
	return ""
}

// reserveWindowLength bounds how long a reading without a reset time counts.
func reserveWindowLength(provider string, quota QuotaState, window string) time.Duration {
	if strings.EqualFold(provider, "codex") {
		if minutes, ok := parseSignalFloat(quota.Signals, "x-codex-"+window+"-window-minutes"); ok && minutes > 0 {
			return time.Duration(minutes * float64(time.Minute))
		}
	}
	if window == "5h" || window == "primary" {
		return 5 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// ReserveHolds lists the credential's windows at or past their reserve for a
// request for model ("" checks only the windows that bind every request).
//
// The readings are the rate-limit headers of the account's last response. A
// reading can be old, and clients that use the account outside the proxy can
// spend it meanwhile, so the proxy's next request to the account is the one
// that learns the new number; after that response the account is held. A
// window whose reset has passed counts as empty, as it does for routing. A
// reading with no reset time counts for one window length after it was taken.
func ReserveHolds(auth *Auth, model string, now time.Time) []ReserveHold {
	reserve := ReserveOf(auth)
	if reserve.IsZero() {
		return nil
	}
	model = strings.ToLower(model)
	var holds []ReserveHold
	for _, room := range reserveRooms(auth.Provider, auth.Quota) {
		keep := reserve.For(room.Window)
		if keep <= 0 || room.Used+1e-9 < 1-keep {
			continue
		}
		if family := reserveWindowModel(room.Window); family != "" && (model == "" || !strings.Contains(model, family)) {
			continue
		}
		until := room.ResetsAt
		if until.IsZero() {
			if auth.Quota.ObservedAt.IsZero() {
				continue
			}
			until = auth.Quota.ObservedAt.Add(reserveWindowLength(auth.Provider, auth.Quota, room.Window))
		}
		if !until.After(now) {
			continue
		}
		holds = append(holds, ReserveHold{Window: room.Window, Used: room.Used, Reserve: keep, Until: until})
	}
	return holds
}

// reserveHold is the hold that ends last, since the account is free only once
// every held window has reset.
func reserveHold(auth *Auth, model string, now time.Time) (ReserveHold, bool) {
	holds := ReserveHolds(auth, model, now)
	if len(holds) == 0 {
		return ReserveHold{}, false
	}
	last := holds[0]
	for _, h := range holds[1:] {
		if h.Until.After(last.Until) {
			last = h
		}
	}
	return last, true
}

// reserveError answers a request when every credential that could serve it is
// at its reserve or cooling down. It is a 403, not a 429: a 429 makes Claude
// Code retry quietly for minutes, while this should stop at once and say why.
type reserveError struct {
	model    string
	held     []string
	cooling  int
	resetsIn time.Duration
}

func (e *reserveError) Error() string {
	model := e.model
	if model == "" {
		model = "this model"
	}
	message := fmt.Sprintf("Every account for %s is at the reserve set in Buff Cloud: %s.", model, strings.Join(e.held, "; "))
	if e.cooling > 0 {
		message += fmt.Sprintf(" %d other account(s) are cooling down.", e.cooling)
	}
	return message + " Lower the reserve on Buff Cloud's Usage page to use it now."
}

func (e *reserveError) StatusCode() int { return http.StatusForbidden }

// Headers gives Retry-After for clients that wait.
func (e *reserveError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", fmt.Sprintf("%d", int(math.Ceil(math.Max(e.resetsIn.Seconds(), 0)))))
	return headers
}

// describeReserveHold reads "Claude 4 keeps 20% of 7d free (81% used, resets 2026-10-28 09:00 UTC)".
func describeReserveHold(auth *Auth, hold ReserveHold) string {
	name := strings.TrimSpace(auth.Label)
	if name == "" {
		name = auth.ID
	}
	return fmt.Sprintf("%s keeps %d%% of %s free (%d%% used, resets %s)", name, int(math.Round(hold.Reserve*100)), hold.Window,
		int(math.Round(hold.Used*100)), hold.Until.UTC().Format("2006-01-02 15:04 UTC"))
}

// newReserveError is the error for a candidate list where nothing is
// available and at least one credential is held by its reserve, or nil.
// Credentials blocked for other reasons (disabled, signed out) are left out
// of the message; cooling-down ones are counted.
func newReserveError(auths []*Auth, modelFor func(*Auth) string, routeModel string, now time.Time) error {
	var held []string
	cooling := 0
	var soonest time.Time
	for _, auth := range auths {
		model := modelFor(auth)
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		switch {
		case !blocked:
			return nil
		case reason == blockReasonReserve:
			hold, _ := reserveHold(auth, model, now)
			held = append(held, describeReserveHold(auth, hold))
		case reason == blockReasonCooldown:
			cooling++
		default:
			continue
		}
		if next.After(now) && (soonest.IsZero() || next.Before(soonest)) {
			soonest = next
		}
	}
	if len(held) == 0 {
		return nil
	}
	sort.Strings(held)
	return &reserveError{model: routeModel, held: held, cooling: cooling, resetsIn: soonest.Sub(now)}
}

// anyReserveSet reports whether any credential has a reserve. The scheduler's
// fast path keeps its own availability lists, so routing takes the legacy pick
// paths, which check the reserve on every pick, while one is set.
func (m *Manager) anyReserveSet() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if !ReserveOf(auth).IsZero() {
			return true
		}
	}
	return false
}

// ReserveHeld reports whether the credential is at its reserve for model. The
// Responses WebSocket handler uses it to drop a connection's pinned account.
func ReserveHeld(auth *Auth, model string, now time.Time) bool {
	_, held := reserveHold(auth, model, now)
	return held
}
