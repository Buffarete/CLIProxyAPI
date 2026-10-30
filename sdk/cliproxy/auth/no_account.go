package auth

// Buff Cloud fork: when no account can take a request, say what happened to
// every account, not only the ones held by their reserve. A network failure on
// box (a DNS lookup that fails) is reported as such: no account is at fault,
// and the client should try again.

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// noAccountError answers a request that no account could serve.
type noAccountError struct {
	message string
	status  int
	retryIn time.Duration
}

func (e *noAccountError) Error() string   { return e.message }
func (e *noAccountError) StatusCode() int { return e.status }

// Headers gives Retry-After for clients that wait.
func (e *noAccountError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", fmt.Sprintf("%d", int(math.Ceil(math.Max(e.retryIn.Seconds(), 0)))))
	return headers
}

// explainNoAccount builds the answer for a request that ran out of accounts:
// the accounts tried in this request, then every other account for the model
// with why it was not used. A request whose last failure was a network error
// is a 502, so the client retries; otherwise the reserve's 403 stands.
func (m *Manager) explainNoAccount(routeModel string, tried map[string]struct{}, lastErr error, reserve *reserveError) error {
	now := time.Now()
	network := lastErr != nil && isTransientTransportError(lastErr)
	model := routeModel
	if model == "" {
		model = "this model"
	}
	reg := registry.GetGlobalRegistry()
	key := canonicalModelKey(routeModel)
	var lines []string
	for _, auth := range m.List() {
		if auth == nil || (key != "" && !reg.ClientSupportsModel(auth.ID, key)) {
			continue
		}
		if _, ok := tried[auth.ID]; ok {
			lines = append(lines, nameOf(auth)+": tried, failed")
			continue
		}
		lines = append(lines, nameOf(auth)+": "+skipReason(auth, routeModel, now))
	}
	sort.Strings(lines)
	var b strings.Builder
	status, retryIn := http.StatusForbidden, time.Duration(0)
	if reserve != nil {
		retryIn = reserve.resetsIn
	}
	if network {
		status, retryIn = http.StatusBadGateway, 5*time.Second
		fmt.Fprintf(&b, "Network error on the proxy host, no account is at fault: %s. Try again.", shortTransportError(lastErr))
	} else {
		fmt.Fprintf(&b, "No account for %s can take this request.", model)
		if lastErr != nil {
			fmt.Fprintf(&b, " Last error: %s.", firstLineOf(lastErr.Error()))
		}
	}
	if len(lines) > 0 {
		b.WriteString(" Accounts: " + strings.Join(lines, "; ") + ".")
	}
	return &noAccountError{message: b.String(), status: status, retryIn: retryIn}
}

// skipReason says why an account that was not tried could not take the request.
func skipReason(auth *Auth, model string, now time.Time) string {
	if blocked, reason, next := isAuthBlockedForModel(auth, model, now); blocked {
		switch reason {
		case blockReasonDisabled:
			return "off"
		case blockReasonReserve:
			hold, _ := reserveHold(auth, model, now)
			return fmt.Sprintf("at its reserve, keeps %d%% of %s free (%d%% used, resets %s)", int(math.Round(hold.Reserve*100)), hold.Window, int(math.Round(hold.Used*100)), clockUTC(hold.Until))
		case blockReasonCooldown:
			if !next.IsZero() {
				return "cooling down until " + clockUTC(next) + coolingCause(auth)
			}
			return "cooling down" + coolingCause(auth)
		default:
			if hasUnauthorizedAuthFailure(auth) {
				return "signed out"
			}
			return "unavailable" + coolingCause(auth)
		}
	}
	pace := PaceOf(auth, model, now)
	switch {
	case pace.Week < pacedWeekEmpty:
		return fmt.Sprintf("7d full (resets %s)", clockUTC(pace.WeekResetsAt))
	case pace.FiveHour < pacedMoveAt:
		return fmt.Sprintf("5h limit (%d%% used)", int(math.Round((1-pace.FiveHour)*100)))
	}
	return "free, not reached"
}

func coolingCause(auth *Auth) string {
	if auth.LastError != nil && strings.TrimSpace(auth.LastError.Message) != "" {
		return " after " + firstLineOf(auth.LastError.Message)
	}
	return ""
}

func nameOf(auth *Auth) string {
	if name := strings.TrimSpace(auth.Label); name != "" {
		return name
	}
	return auth.ID
}

func clockUTC(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.UTC().Format("Jan 2 15:04 UTC")
}

// shortTransportError keeps the part of a dial error that says what failed:
// "lookup api.anthropic.com on 127.0.0.53:53: server misbehaving".
func shortTransportError(err error) string {
	text := err.Error()
	if i := strings.Index(text, "lookup "); i >= 0 {
		return "DNS " + text[i:]
	}
	if i := strings.LastIndex(text, "dial tcp"); i >= 0 {
		return text[i:]
	}
	return firstLineOf(text)
}

func firstLineOf(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// asReserveError reports whether a pick failed on the reserve.
func asReserveError(err error) (*reserveError, bool) {
	var reserve *reserveError
	if errors.As(err, &reserve) && reserve != nil {
		return reserve, true
	}
	return nil, false
}
