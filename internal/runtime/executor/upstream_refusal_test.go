package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Anthropic's answer to a client other than Claude Code on a subscription login,
// as seen live on 2026-10-19: 429, no rate-limit headers, no Retry-After.
const claudeRefusalBody = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"},"request_id":"req_test"}`

type claudeRefusalRun struct {
	hits    int32
	err     error
	manager *cliproxyauth.Manager
	authIDs []string
}

// runClaudeRefusal sends one request through a manager with two Claude
// credentials whose upstream answers 429 with the given headers, allowing
// retryRounds additional credential rounds.
func runClaudeRefusal(t *testing.T, cfg *config.Config, headers http.Header, retryRounds int) *claudeRefusalRun {
	t.Helper()
	run := &claudeRefusalRun{}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for key, values := range headers {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(claudeRefusalBody))
	}))
	t.Cleanup(server.Close)

	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(retryRounds, 30*time.Second, 0)
	manager.RegisterExecutor(NewClaudeExecutor(cfg))
	reg := registry.GetGlobalRegistry()
	base := uuid.NewString()
	for i := range 2 {
		auth := &cliproxyauth.Auth{
			ID:         base + "-refusal-" + strconv.Itoa(i),
			Provider:   "claude",
			Attributes: map[string]string{"api_key": "key-" + strconv.Itoa(i), "base_url": server.URL},
		}
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		id := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
		run.authIDs = append(run.authIDs, auth.ID)
	}
	_, run.err = manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(`{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	run.hits = hits.Load()
	run.manager = manager
	return run
}

func (r *claudeRefusalRun) cooledDown() []string {
	var cooled []string
	for _, id := range r.authIDs {
		auth, _ := r.manager.GetByID(id)
		if auth.Unavailable || auth.Quota.Exceeded || !auth.NextRetryAfter.IsZero() {
			cooled = append(cooled, id)
			continue
		}
		for _, state := range auth.ModelStates {
			if state != nil && (state.Unavailable || state.Quota.Exceeded || !state.NextRetryAfter.IsZero()) {
				cooled = append(cooled, id)
				break
			}
		}
	}
	return cooled
}

func TestClaudeRefusalPassesThroughWithoutCooldownOrRetry(t *testing.T) {
	run := runClaudeRefusal(t, &config.Config{DisableClaudeCloakMode: true}, nil, 3)
	var status interface{ StatusCode() int }
	if !errors.As(run.err, &status) || status.StatusCode() != http.StatusTooManyRequests || run.err.Error() != claudeRefusalBody {
		t.Fatalf("error = %v, want Anthropic's 429 body unchanged", run.err)
	}
	if run.hits != 1 {
		t.Fatalf("upstream requests = %d, want 1: a refusal must not be retried on another credential", run.hits)
	}
	if cooled := run.cooledDown(); len(cooled) != 0 {
		t.Fatalf("credentials cooled down after a refusal: %v", cooled)
	}
}

func TestClaudeQuota429StillCoolsDownAndFailsOver(t *testing.T) {
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-Status":    []string{"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status": []string{"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":     []string{strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)},
	}
	run := runClaudeRefusal(t, &config.Config{DisableClaudeCloakMode: true}, headers, 3)
	if run.hits != 2 {
		t.Fatalf("upstream requests = %d, want 2: a quota 429 moves to the other credential", run.hits)
	}
	if cooled := run.cooledDown(); len(cooled) != 2 {
		t.Fatalf("cooled credentials = %v, want both", cooled)
	}
}

func TestClaudeBare429KeepsUpstreamHandlingWhenCloakingIsOn(t *testing.T) {
	run := runClaudeRefusal(t, &config.Config{}, nil, 0)
	if run.hits != 2 {
		t.Fatalf("upstream requests = %d, want 2: outside passthrough a bare 429 keeps upstream's failover", run.hits)
	}
}

func TestCodexRefusalIsRequestScopedOnlyWithoutUsageSignals(t *testing.T) {
	honest := &config.Config{}
	honest.Codex.DisableCodexCloaking = true
	usage := http.Header{"X-Codex-Primary-Used-Percent": []string{"100"}, "X-Codex-Primary-Reset-At": []string{"1792427400"}}
	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		headers http.Header
		body    string
		refusal bool
	}{
		{"bare 429", honest, nil, `{"error":{"message":"Error"}}`, true},
		{"429 with usage headers", honest, usage, `{"error":{"message":"Error"}}`, false},
		{"429 with retry-after", honest, http.Header{"Retry-After": []string{"30"}}, `{"error":{"message":"Error"}}`, false},
		{"usage limit body", honest, nil, `{"error":{"type":"usage_limit_reached"}}`, false},
		{"model capacity body", honest, nil, `{"error":{"message":"Selected model is at capacity. Please try a different model."}}`, false},
		{"cloaking on", &config.Config{}, nil, `{"error":{"message":"Error"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := codexStatusErrOrRefusal(tc.cfg, http.StatusTooManyRequests, tc.headers, []byte(tc.body), false)
			var scoped cliproxyexecutor.RequestScopedError
			got := errors.As(err, &scoped) && scoped.IsRequestScoped()
			if got != tc.refusal {
				t.Fatalf("request-scoped = %v, want %v (error %T)", got, tc.refusal, err)
			}
		})
	}
}
