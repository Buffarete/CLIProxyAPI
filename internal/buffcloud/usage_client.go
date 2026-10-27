package buffcloud

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

const (
	// RoutineHeader names the Buff Cloud routine a request belongs to.
	RoutineHeader = "X-Buff-Routine"
	// ThreadHeader names the T3 thread a request belongs to.
	ThreadHeader = "X-Buff-Thread"
	// tagLimit caps a routine or thread tag, in characters.
	tagLimit = 120
	// clientLimit caps the client name and version taken from a User-Agent.
	clientLimit = 60
)

// TagsMiddleware takes the X-Buff-Routine and X-Buff-Thread headers off each
// request, so no executor or passthrough can send them upstream, and keeps
// them with the User-Agent in the request's ClientRequestMetadata, where usage
// rows, messages rows and Codex's live paths find them.
func TagsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Request.Header
		routine, thread := tag(h.Get(RoutineHeader)), tag(h.Get(ThreadHeader))
		h.Del(RoutineHeader)
		h.Del(ThreadHeader)
		ctx := c.Request.Context()
		meta := logging.GetClientRequestMetadata(ctx)
		meta.Routine, meta.Thread = routine, thread
		if meta.UserAgent == "" {
			meta.UserAgent = strings.TrimSpace(c.Request.UserAgent())
		}
		c.Request = c.Request.WithContext(logging.WithClientRequestMetadata(ctx, meta))
		c.Next()
	}
}

// tag trims a header value and cuts it to 120 characters.
func tag(v string) string {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > tagLimit {
		v = strings.TrimSpace(string([]rune(v)[:tagLimit]))
	}
	return v
}

// addClientContext fills the row's client, version, routine and thread from
// the request's metadata. The client IP is never kept.
func addClientContext(ctx context.Context, row *Row) {
	if ctx == nil {
		return
	}
	meta := logging.GetClientRequestMetadata(ctx)
	row.Client, row.ClientVersion = ParseUserAgent(meta.UserAgent)
	row.Routine, row.Thread = meta.Routine, meta.Thread
}

// ParseUserAgent names the client of a User-Agent and its version: Claude Code
// (claude-cli/x.y), Codex (codex_cli_rs, codex_exec, codex-*), opencode and
// pi by name, else the first product token as it is.
func ParseUserAgent(ua string) (client, version string) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", ""
	}
	product, _, _ := strings.Cut(ua, " ")
	name, version, _ := strings.Cut(product, "/")
	name = strings.TrimSpace(name)
	version = strings.Trim(strings.TrimSpace(version), ";,()")
	lower := strings.ToLower(name)
	switch {
	case lower == "claude-cli" || lower == "claude-code":
		client = "Claude Code"
	case lower == "codex_cli_rs" || lower == "codex_exec" || strings.HasPrefix(lower, "codex-") || strings.HasPrefix(lower, "codex_"):
		client = "Codex"
	case lower == "opencode" || strings.HasPrefix(lower, "opencode-"):
		client = "opencode"
	case lower == "pi":
		client = "pi"
	default:
		client = name
	}
	return cut(client, clientLimit), cut(version, clientLimit)
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}
