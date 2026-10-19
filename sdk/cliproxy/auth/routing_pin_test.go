package auth

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// scriptedExecutor answers per credential: an error from failures, else success.
type scriptedExecutor struct {
	provider string
	failures map[string]error

	mu    sync.Mutex
	calls []string
}

func (e *scriptedExecutor) Identifier() string { return e.provider }

func (e *scriptedExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID)
	err := e.failures[auth.ID]
	e.mu.Unlock()
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *scriptedExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not used"}
}

func (*scriptedExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (*scriptedExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (*scriptedExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *scriptedExecutor) takeCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	calls := e.calls
	e.calls = nil
	return calls
}

const (
	pinClaudeModel = "pin-test-claude-model"
	pinCodexModel  = "pin-test-codex-model"
)

type pinFixture struct {
	manager *Manager
	claude  *scriptedExecutor
	codex   *scriptedExecutor
}

// newPinFixture registers claude-a, claude-b, codex-a and codex-b and applies
// routing.pinned and routing.pinned-fallback.
func newPinFixture(t *testing.T, selector Selector, pinned string, fallback *bool) *pinFixture {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)
	cfg := &internalconfig.Config{}
	cfg.Routing.Pinned = pinned
	cfg.Routing.PinnedFallback = fallback
	manager.SetConfig(cfg)
	f := &pinFixture{
		manager: manager,
		claude:  &scriptedExecutor{provider: "claude", failures: map[string]error{}},
		codex:   &scriptedExecutor{provider: "codex", failures: map[string]error{}},
	}
	manager.RegisterExecutor(f.claude)
	manager.RegisterExecutor(f.codex)
	reg := registry.GetGlobalRegistry()
	for _, a := range []struct{ id, provider, model string }{
		{"pin-claude-a", "claude", pinClaudeModel},
		{"pin-claude-b", "claude", pinClaudeModel},
		{"pin-codex-a", "codex", pinCodexModel},
		{"pin-codex-b", "codex", pinCodexModel},
	} {
		reg.RegisterClient(a.id, a.provider, []*registry.ModelInfo{{ID: a.model}})
		id := a.id
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: a.id, Provider: a.provider}); errRegister != nil {
			t.Fatalf("register %s: %v", a.id, errRegister)
		}
	}
	return f
}

func (f *pinFixture) execute(provider, model string) error {
	_, err := f.manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	return err
}

func boolPtr(v bool) *bool { return &v }

func TestRoutingPinServesOnlyItsOwnProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector Selector
		fallback *bool
	}{
		{"round-robin", &RoundRobinSelector{}, nil},
		{"most-room", NewRoomSelector(RoomStrategyMostRoom), nil},
		{"most-room without fallback", NewRoomSelector(RoomStrategyMostRoom), boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinFixture(t, tc.selector, "pin-claude-b", tc.fallback)
			for range 3 {
				if err := f.execute("claude", pinClaudeModel); err != nil {
					t.Fatalf("claude request: %v", err)
				}
			}
			if calls := f.claude.takeCalls(); !slices.Equal(calls, []string{"pin-claude-b", "pin-claude-b", "pin-claude-b"}) {
				t.Fatalf("claude calls = %v, want every request on the pinned pin-claude-b", calls)
			}
			for range 2 {
				if err := f.execute("codex", pinCodexModel); err != nil {
					t.Fatalf("codex request with a Claude pin: %v", err)
				}
			}
			calls := f.codex.takeCalls()
			if len(calls) != 2 || calls[0] == calls[1] {
				t.Fatalf("codex calls = %v, want both Codex credentials used: the Claude pin must not affect Codex", calls)
			}
		})
	}
}

func TestRoutingPinFallsBackWhenThePinnedCredentialFails(t *testing.T) {
	f := newPinFixture(t, NewRoomSelector(RoomStrategyMostRoom), "pin-claude-b", nil)
	f.claude.failures["pin-claude-b"] = &Error{HTTPStatus: http.StatusInternalServerError, Message: "upstream broke"}
	if err := f.execute("claude", pinClaudeModel); err != nil {
		t.Fatalf("request with fallback on: %v", err)
	}
	if calls := f.claude.takeCalls(); !slices.Equal(calls, []string{"pin-claude-b", "pin-claude-a"}) {
		t.Fatalf("claude calls = %v, want the pin first, then pin-claude-a", calls)
	}
	// The pin is now cooling down, so the next request goes straight to another credential.
	if err := f.execute("claude", pinClaudeModel); err != nil {
		t.Fatalf("request while the pin cools down: %v", err)
	}
	if calls := f.claude.takeCalls(); !slices.Equal(calls, []string{"pin-claude-a"}) {
		t.Fatalf("claude calls = %v, want only pin-claude-a while the pin cools down", calls)
	}
}

func TestRoutingPinWithoutFallbackReturnsThePinnedError(t *testing.T) {
	f := newPinFixture(t, NewRoomSelector(RoomStrategyMostRoom), "pin-claude-b", boolPtr(false))
	f.claude.failures["pin-claude-b"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "pinned account is rate limited"}
	err := f.execute("claude", pinClaudeModel)
	if err == nil || statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want the pinned credential's 429", err)
	}
	if calls := f.claude.takeCalls(); !slices.Equal(calls, []string{"pin-claude-b"}) {
		t.Fatalf("claude calls = %v, want only the pinned credential", calls)
	}
	// While the pin cools down the request fails without touching another credential.
	if err = f.execute("claude", pinClaudeModel); err == nil {
		t.Fatal("request while the pin cools down succeeded, want an error")
	}
	if calls := f.claude.takeCalls(); len(calls) != 0 {
		t.Fatalf("claude calls = %v, want none while the pin cools down", calls)
	}
}

func TestRoutingPinIgnoresAnUnknownCredential(t *testing.T) {
	f := newPinFixture(t, NewRoomSelector(RoomStrategyMostRoom), "no-such-credential", boolPtr(false))
	if err := f.execute("claude", pinClaudeModel); err != nil {
		t.Fatalf("claude request with an unknown pin: %v", err)
	}
	if err := f.execute("codex", pinCodexModel); err != nil {
		t.Fatalf("codex request with an unknown pin: %v", err)
	}
}

// refusalError is what the Claude and Codex executors return for a 429 that
// carries no rate-limit headers and no Retry-After (upstreamRefusalError).
type refusalError struct{}

func (refusalError) Error() string {
	return `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`
}
func (refusalError) StatusCode() int          { return http.StatusTooManyRequests }
func (refusalError) IsRequestScoped() bool    { return true }
func (refusalError) IsCredentialScoped() bool { return false }

func TestRefusalPassesThroughWithoutCooldownOrRetry(t *testing.T) {
	f := newPinFixture(t, NewRoomSelector(RoomStrategyMostRoom), "", nil)
	f.manager.SetRetryConfig(3, 30*time.Second, 0)
	f.claude.failures["pin-claude-a"] = refusalError{}
	f.claude.failures["pin-claude-b"] = refusalError{}

	err := f.execute("claude", pinClaudeModel)
	if err == nil || statusCodeFromError(err) != http.StatusTooManyRequests || err.Error() != (refusalError{}).Error() {
		t.Fatalf("error = %v, want the upstream 429 body unchanged", err)
	}
	calls := f.claude.takeCalls()
	if len(calls) != 1 {
		t.Fatalf("claude calls = %v, want one attempt and no retry on another credential", calls)
	}
	refused, _ := f.manager.GetByID(calls[0])
	if refused.Failed != 1 {
		t.Fatalf("refused credential failed count = %d, want 1", refused.Failed)
	}
	if refused.Unavailable || refused.Quota.Exceeded || !refused.NextRetryAfter.IsZero() {
		t.Fatalf("refused credential cooled down: unavailable=%v quota=%+v next_retry=%v", refused.Unavailable, refused.Quota, refused.NextRetryAfter)
	}
	for model, state := range refused.ModelStates {
		if state != nil && (state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero()) {
			t.Fatalf("model %s cooled down after a refusal: %+v", model, state)
		}
	}
	// A quota 429 still moves to another credential, as upstream does.
	quota := newPinFixture(t, NewRoomSelector(RoomStrategyMostRoom), "pin-claude-a", nil)
	quota.claude.failures["pin-claude-a"] = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
	if err = quota.execute("claude", pinClaudeModel); err != nil {
		t.Fatalf("request after a quota 429: %v", err)
	}
	if calls := quota.claude.takeCalls(); !slices.Equal(calls, []string{"pin-claude-a", "pin-claude-b"}) {
		t.Fatalf("claude calls = %v, want the quota 429 on pin-claude-a retried on pin-claude-b", calls)
	}
}
