package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// The error box's resolver gave on 2026-10-29 at 23:47 (and an i/o timeout at 23:30), as the executor reports it.
const recordedDNSFailure = `Post "https://api.anthropic.com/v1/messages?beta=true": claude tls: dial upstream: dial tcp: lookup api.anthropic.com on 127.0.0.53:53: server misbehaving`
const recordedDNSTimeout = `Post "https://api.anthropic.com/v1/messages?beta=true": claude tls: dial upstream: dial tcp: lookup api.anthropic.com on 127.0.0.53:53: read udp 127.0.0.1:39466->127.0.0.53:53: i/o timeout`

func TestANetworkFailureDoesNotCoolTheAccount(t *testing.T) {
	for _, message := range []string{recordedDNSFailure, recordedDNSTimeout, "context canceled"} {
		manager := NewManager(nil, nil, nil)
		auth := &Auth{ID: "claude-7-net", Provider: "claude"}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		manager.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "claude", Model: "claude-opus-5-5", Success: false, Error: &Error{Message: message}})
		got, _ := manager.GetByID(auth.ID)
		if blocked, reason, until := isAuthBlockedForModel(got, "claude-opus-5-5", time.Now()); blocked {
			t.Errorf("%q blocked the account: reason=%v until=%v", message, reason, until)
		}
		reg.UnregisterClient(auth.ID)
	}
}

// The 23:47 case: Claude 7, 3, 1, 2 and 4 each failed on the DNS error, and only
// Claude 5 and 6 were left, behind their reserves.
func TestNoAccountAfterANetworkFailureSaysSoAndListsEveryAccount(t *testing.T) {
	now := time.Now()
	manager := NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	add := func(id, label string, used5h, used7d float64, reserve float64) {
		auth := claudeAuth(id, used5h, now.Add(2*time.Hour), used7d, now.Add(60*time.Hour), now.Add(-time.Minute))
		auth.Label = label
		if reserve > 0 {
			withReserve(auth, reserve, nil)
		}
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	add("net-claude_7", "Claude 7", 0.44, 0.09, 0)
	add("net-claude_3", "Claude 3", 0.95, 0.4, 0)
	add("net-claude", "Claude 1", 0, 1, 0)
	add("net-claude_5", "Claude 5", 0.1, 0.74, 0.3)
	add("net-claude_6", "Claude 6", 0, 0.9, 0.1)
	tried := map[string]struct{}{"net-claude_7": {}, "net-claude_3": {}, "net-claude": {}}
	reserve := &reserveError{model: "claude-opus-5-5", held: []string{"Claude 5", "Claude 6"}, resetsIn: time.Hour}

	err := manager.explainNoAccount("claude-opus-5-5", tried, errors.New(recordedDNSFailure), reserve)
	var answer *noAccountError
	if !errors.As(err, &answer) || answer.StatusCode() != http.StatusBadGateway {
		t.Fatalf("answer = %v, want a 502", err)
	}
	for _, want := range []string{
		"Network error on the proxy host, no account is at fault: DNS lookup api.anthropic.com on 127.0.0.53:53: server misbehaving",
		"Claude 7: tried, failed", "Claude 3: tried, failed", "Claude 1: tried, failed",
		"Claude 5: at its reserve, keeps 30% of 7d free (74% used",
		"Claude 6: at its reserve, keeps 10% of 7d free (90% used",
	} {
		if !strings.Contains(answer.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, answer.Error())
		}
	}

	// Without a network failure the reserve's 403 stands, and accounts not tried say why.
	err = manager.explainNoAccount("claude-opus-5-5", map[string]struct{}{"net-claude_7": {}}, &Error{HTTPStatus: 429, Message: "rate_limit_error"}, reserve)
	if !errors.As(err, &answer) || answer.StatusCode() != http.StatusForbidden {
		t.Fatalf("answer = %v, want a 403", err)
	}
	for _, want := range []string{"No account for claude-opus-5-5 can take this request.", "Claude 1: 7d full", "Claude 3: 5h limit (95% used)"} {
		if !strings.Contains(answer.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, answer.Error())
		}
	}
}
