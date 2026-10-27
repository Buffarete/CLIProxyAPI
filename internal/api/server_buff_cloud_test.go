package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestProxyPausedRefusesProxiedRequestsAndKeepsManagementUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	s := &Server{cfg: cfg}
	engine := gin.New()
	engine.Use(s.proxyPausedMiddleware())
	ok := func(c *gin.Context) { c.String(http.StatusOK, "served") }
	paths := []string{
		"/v1/messages", "/v1/responses", "/v1/models", "/v1/chat/completions", "/backend-api/codex/responses",
		"/v8/management/buff/accounts", "/v0/management/config", "/healthz", "/codex/callback",
	}
	for _, p := range paths {
		engine.Any(p, ok)
	}
	refused := map[string]bool{"/v1/messages": true, "/v1/responses": true, "/v1/models": true, "/v1/chat/completions": true, "/backend-api/codex/responses": true}

	for _, paused := range []bool{false, true} {
		cfg.Routing.Paused = paused
		for _, p := range paths {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`)))
			wantRefused := paused && refused[p]
			if wantRefused {
				if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), ProxyPausedMessage) {
					t.Errorf("paused %s: %d %s, want 403 with the message", p, recorder.Code, recorder.Body.String())
				}
			} else if recorder.Code != http.StatusOK {
				t.Errorf("paused=%v %s: %d, want served", paused, p, recorder.Code)
			}
		}
	}
}

// The routine and thread headers never reach upstream, even on a route that
// passes the client's headers through.
func TestBuffTagHeadersAreNotForwardedUpstream(t *testing.T) {
	server := newTestServer(t)
	executor := &codexSearchCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)
	credential := &auth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "codex-token", "account_id": "account-123"},
	}
	if _, err := server.handlers.AuthManager.Register(context.Background(), credential); err != nil {
		t.Fatalf("register Codex auth: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"query":"q"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", "session-123")
	req.Header.Set(buffcloud.RoutineHeader, "nightly")
	req.Header.Set(buffcloud.ThreadHeader, "thread-7")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || executor.request == nil {
		t.Fatalf("status = %d, upstream request %v; body=%s", rr.Code, executor.request != nil, rr.Body.String())
	}
	if got := executor.request.Header.Get("Session_id"); got != "session-123" {
		t.Fatalf("Session_id = %q; the route should pass client headers through", got)
	}
	for name := range executor.request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-buff-") {
			t.Errorf("upstream request carries %s", name)
		}
	}
	if meta := logging.GetClientRequestMetadata(executor.request.Context()); meta.Routine != "nightly" || meta.Thread != "thread-7" {
		t.Errorf("upstream request context metadata = %+v", meta)
	}
}
