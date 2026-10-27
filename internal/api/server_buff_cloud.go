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

// setupBuffCloudUsageLog writes usage/<day>.jsonl next to the config file, and
// returns the middleware that logs requests which failed before reaching an
// account.
func setupBuffCloudUsageLog(configFilePath string) gin.HandlerFunc {
	if strings.TrimSpace(configFilePath) == "" {
		return nil
	}
	l := buffcloud.OpenUsageLog(filepath.Dir(configFilePath))
	buffcloud.SetDefaultUsageLog(l)
	usage.RegisterNamedPlugin("buff-cloud-usage-log", l)
	return l.UnroutedMiddleware()
}

// buffCloudTagsMiddleware takes X-Buff-Routine and X-Buff-Thread off every
// request, keeping them for the usage and messages rows.
func buffCloudTagsMiddleware() gin.HandlerFunc { return buffcloud.TagsMiddleware() }

// setupBuffCloudMessagesLog keeps each request's new messages and reply in
// messages/<day>.jsonl next to the config file, and returns the middleware that
// records them.
func setupBuffCloudMessagesLog(configFilePath string) gin.HandlerFunc {
	if strings.TrimSpace(configFilePath) == "" {
		return nil
	}
	l := buffcloud.OpenMessagesLog(filepath.Join(filepath.Dir(configFilePath), buffcloud.MessagesDirName), func(traceID string) (buffcloud.Row, bool) {
		if u := buffcloud.DefaultUsageLog(); u != nil {
			return u.ByTrace(traceID)
		}
		return buffcloud.Row{}, false
	})
	buffcloud.SetDefaultMessagesLog(l)
	return l.Middleware()
}

func (s *Server) registerBuffCloudRoutes(v8 *gin.RouterGroup) {
	v8.GET("/buff/accounts", s.mgmt.BuffAccounts)
	v8.GET("/buff/usage", s.mgmt.BuffUsage)
	v8.GET("/buff/usage/series", s.mgmt.BuffUsageSeries)
	v8.GET("/buff/summary", s.mgmt.BuffSummary)
	v8.GET("/buff/messages", s.mgmt.BuffMessages)
	v8.PATCH("/buff/settings", s.mgmt.PatchBuffSettings)
	v8.PATCH("/buff/reserve", s.mgmt.PatchBuffReserve)
}

// proxyPausedMiddleware answers every proxied request with 403 while
// routing.paused is set. Management, /healthz and OAuth callbacks stay up.
// Not 503: Claude Code retries a 503 for about 90 seconds and shows nothing,
// while a 403 ends the request at once with the message.
func (s *Server) proxyPausedMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.getConfig()
		if cfg == nil || !cfg.Routing.Paused || !isProxiedPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
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
