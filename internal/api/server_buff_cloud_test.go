package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
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
				if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), ProxyPausedMessage) {
					t.Errorf("paused %s: %d %s, want 503 with the message", p, recorder.Code, recorder.Body.String())
				}
			} else if recorder.Code != http.StatusOK {
				t.Errorf("paused=%v %s: %d, want served", paused, p, recorder.Code)
			}
		}
	}
}
