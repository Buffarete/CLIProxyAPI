package buffcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestParseUserAgent(t *testing.T) {
	for _, c := range []struct{ ua, client, version string }{
		{"claude-cli/2.1.3 (external, cli)", "Claude Code", "2.1.3"},
		{"claude-cli/2.0.14 (external, sdk-ts)", "Claude Code", "2.0.14"},
		{"codex_cli_rs/0.50.0 (Ubuntu 24.4.0; x86_64) xterm-256color", "Codex", "0.50.0"},
		{"codex_exec/0.48.1 (Mac OS 15.1.0; arm64)", "Codex", "0.48.1"},
		{"codex-tui/0.51.0", "Codex", "0.51.0"},
		{"opencode/0.15.3 ai-sdk/provider-utils/3.0.0 runtime/bun/1.3.0", "opencode", "0.15.3"},
		{"pi/1.4.0", "pi", "1.4.0"},
		{"curl/8.5.0", "curl", "8.5.0"},
		{"Mozilla/5.0 (X11; Linux x86_64)", "Mozilla", "5.0"},
		{"python-httpx", "python-httpx", ""},
		{"", "", ""},
		{"  OpenAI/Python 1.99.0", "OpenAI", "Python"},
	} {
		client, version := ParseUserAgent(c.ua)
		if client != c.client || version != c.version {
			t.Errorf("ParseUserAgent(%q) = %q, %q; want %q, %q", c.ua, client, version, c.client, c.version)
		}
	}
}

func TestTagsMiddlewareTakesTheHeadersOffAndKeepsThem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seen http.Header
	var meta logging.ClientRequestMetadata
	r := gin.New()
	r.Use(TagsMiddleware())
	r.POST("/v1/messages", func(c *gin.Context) {
		seen = c.Request.Header.Clone()
		meta = logging.GetClientRequestMetadata(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.3 (external, cli)")
	req.Header.Set(RoutineHeader, "  Morning brief  ")
	req.Header.Set(ThreadHeader, strings.Repeat("ü", 200))
	r.ServeHTTP(httptest.NewRecorder(), req)

	if seen.Get(RoutineHeader) != "" || seen.Get(ThreadHeader) != "" || len(seen.Values(RoutineHeader)) > 0 {
		t.Errorf("handler still sees the tags: %v", seen)
	}
	if meta.Routine != "Morning brief" || meta.Thread != strings.Repeat("ü", 120) || meta.UserAgent != "claude-cli/2.1.3 (external, cli)" {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestUsageRowsCarryClientRoutineAndThreadButNoIP(t *testing.T) {
	base := t.TempDir()
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := openUsageLog(base, UsageKeepDays, time.UTC, func() time.Time { return at })
	ctx := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{
		ClientIP: "203.0.113.7", ResolvedClientIP: "198.51.100.2", XForwardedFor: "192.0.2.9",
		UserAgent: "codex_cli_rs/0.50.0 (Ubuntu)", Routine: "nightly", Thread: "T3 thread 4",
	})
	l.HandleUsage(ctx, usage.Record{RequestID: "1", Provider: "codex", AuthID: "c.json", RequestedAt: at})
	rows, _ := l.Range(0, 0)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	row := rows[0]
	if row.Client != "Codex" || row.ClientVersion != "0.50.0" || row.Routine != "nightly" || row.Thread != "T3 thread 4" {
		t.Errorf("row = %+v", row)
	}
	data, _ := (&row).marshalForTest()
	for _, ip := range []string{"203.0.113.7", "198.51.100.2", "192.0.2.9"} {
		if strings.Contains(data, ip) {
			t.Errorf("row holds the client IP %s: %s", ip, data)
		}
	}
	// Filters and series read the new fields.
	got, _, _ := l.Query(UsageQuery{Filter: mustFilter(t, "client=Codex&routine=nightly&thread=T3 thread 4")})
	if len(got) != 1 {
		t.Errorf("filtered rows = %d", len(got))
	}
	s, err := l.Series(SeriesQuery{Since: at.Add(-time.Hour).UnixMilli(), Until: at.Add(time.Hour).UnixMilli(), Bucket: "hour", GroupBy: "routine", Loc: time.UTC})
	if err != nil || len(s.Buckets) != 1 || s.Buckets[0].Groups[0].Key != "nightly" {
		t.Errorf("series by routine = %+v, %v", s, err)
	}
}

func (r *Row) marshalForTest() (string, error) {
	data, err := json.Marshal(r)
	return string(data), err
}
