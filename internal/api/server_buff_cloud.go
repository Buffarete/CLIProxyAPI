package api

// Buff Cloud fork: the usage log, the proxy on/off switch and the /buff
// management routes. See docs/buff-cloud/api.md.

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// ProxyPausedMessage is what a client gets while routing.paused is set.
const ProxyPausedMessage = "Buff Cloud has turned this proxy off. Turn it back on from the AI page."

// setupBuffCloudUsageLog writes usage.jsonl next to the config file.
func setupBuffCloudUsageLog(configFilePath string) {
	if strings.TrimSpace(configFilePath) == "" {
		return
	}
	l := buffcloud.OpenUsageLog(filepath.Join(filepath.Dir(configFilePath), buffcloud.UsageFileName))
	buffcloud.SetDefaultUsageLog(l)
	usage.RegisterNamedPlugin("buff-cloud-usage-log", l)
}

func (s *Server) registerBuffCloudRoutes(v8 *gin.RouterGroup) {
	v8.GET("/buff/accounts", s.mgmt.BuffAccounts)
	v8.GET("/buff/usage", s.mgmt.BuffUsage)
	v8.GET("/buff/summary", s.mgmt.BuffSummary)
	v8.PATCH("/buff/settings", s.mgmt.PatchBuffSettings)
}

// proxyPausedMiddleware answers every proxied request with 503 while
// routing.paused is set. Management, /healthz and OAuth callbacks stay up.
func (s *Server) proxyPausedMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.getConfig()
		if cfg == nil || !cfg.Routing.Paused || !isProxiedPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"type":  "error",
			"error": gin.H{"type": "proxy_off", "message": ProxyPausedMessage},
		})
	}
}

func isProxiedPath(path string) bool {
	for _, prefix := range []string{"/v0/management", "/v8/management", "/v0/resource/", "/management.html", "/healthz"} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return !strings.HasSuffix(path, "/callback")
}
