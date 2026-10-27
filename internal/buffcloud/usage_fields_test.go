package buffcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestRowFromRecordCarriesTheNewFields(t *testing.T) {
	at := time.UnixMilli(1_792_400_000_000)
	row := RowFromRecord(usage.Record{
		Provider: "codex", AuthID: "c.json", Model: "gpt-6.1-sol", Alias: "gpt-6.1", RequestedAt: at,
		Latency: 3 * time.Second, TTFT: 420 * time.Millisecond, Stream: true,
		Detail: usage.Detail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 30, CacheReadTokens: 60},
	})
	if row.TTFTMs != 420 || !row.Stream || row.RequestedModel != "gpt-6.1" || row.Error != "" {
		t.Errorf("row = %+v", row)
	}
	if row.Tokens == nil || row.Tokens.Reasoning != 30 || row.Tokens.Total() != 150 {
		t.Errorf("tokens = %+v; reasoning is inside output, not added to the total", row.Tokens)
	}

	// The alias equal to the model is not repeated; a success keeps no error.
	same := RowFromRecord(usage.Record{Provider: "claude", AuthID: "a.json", Model: "opus", Alias: "opus", RequestedAt: at})
	data, _ := json.Marshal(same)
	for _, field := range []string{"requestedModel", "error", "ttftMs", "stream", "endpoint", "reasoning", "client", "routine", "thread"} {
		if strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("row without the field still has %q: %s", field, data)
		}
	}

	// A failure under 400 keeps no error text.
	cancelled := RowFromRecord(usage.Record{Provider: "claude", AuthID: "a.json", RequestedAt: at, Failed: true, Fail: usage.Failure{StatusCode: 399, Body: "nope"}})
	if cancelled.Error != "" {
		t.Errorf("status 399 error = %q", cancelled.Error)
	}
	failed := RowFromRecord(usage.Record{Provider: "claude", AuthID: "a.json", RequestedAt: at, Failed: true, Fail: usage.Failure{StatusCode: 529, Body: `{"error":{"message":"Overloaded"}}`}})
	if failed.Status != 529 || failed.Error != `{"error":{"message":"Overloaded"}}` {
		t.Errorf("529 row = %+v", failed)
	}
}

func TestErrorTextRedactsAndCuts(t *testing.T) {
	for _, secret := range []string{
		"sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"sk-proj-abcdefghijklmnop",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"AIzaSyA1234567890abcdefghijklmnopq",
		"ghp_abcdefghijklmnopqrstuvwxyz0123",
	} {
		got := ErrorText(`{"error":"invalid key ` + secret + `"}`)
		if strings.Contains(got, secret) || !strings.Contains(got, "[redacted]") {
			t.Errorf("ErrorText kept %q: %s", secret, got)
		}
	}
	for in, want := range map[string]string{
		"Authorization: Bearer abc.def-ghi_jkl":     "Authorization: [redacted]",
		`{"api_key":"0123456789abcdef","x":1}`:      `{"api_key":"[redacted]","x":1}`,
		"access_token=zzzzzzzzzzzz&next=1":          "access_token=[redacted]&next=1",
		"rate limit reached, try again in 12s":      "rate limit reached, try again in 12s",
		"  line one\n\n  line two  ":                "line one line two",
		`{"type":"error","message":"secret sauce"}`: `{"type":"error","message":"secret sauce"}`,
	} {
		if got := ErrorText(in); got != want {
			t.Errorf("ErrorText(%q) = %q, want %q", in, got, want)
		}
	}
	long := ErrorText(strings.Repeat("é", 1000))
	if n := len([]rune(long)); n != 300 || !strings.HasSuffix(long, "…") {
		t.Errorf("long error is %d characters", n)
	}
}

func TestHandleUsageAddsTheEndpoint(t *testing.T) {
	base := t.TempDir()
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := openUsageLog(base, UsageKeepDays, time.UTC, func() time.Time { return at })
	ctx := logging.WithEndpoint(context.Background(), "POST /v1/messages")
	l.HandleUsage(ctx, usage.Record{RequestID: "1", Provider: "claude", AuthID: "a.json", RequestedAt: at})
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	l.HandleUsage(context.WithValue(context.Background(), "gin", gc), usage.Record{RequestID: "2", Provider: "codex", AuthID: "c.json", RequestedAt: at})
	rows, _ := l.Range(0, 0)
	if len(rows) != 2 || rows[0].Endpoint != "/v1/messages" || rows[1].Endpoint != "/v1/realtime" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestUnroutedMiddlewareLogsRequestsThatReachedNoAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := t.TempDir()
	l := openUsageLog(base, UsageKeepDays, time.Local, time.Now)
	l.unroutedWait, l.unroutedPoll = 50*time.Millisecond, 5*time.Millisecond

	router := gin.New()
	router.Use(func(c *gin.Context) {
		if id := c.GetHeader("X-Test-Trace"); id != "" {
			c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), id))
		}
	})
	router.Use(l.UnroutedMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		switch c.GetHeader("X-Test-Case") {
		case "routed":
			// The attempt's usage record names an account.
			l.HandleUsage(c.Request.Context(), usage.Record{RequestID: "r", TraceID: c.GetHeader("X-Test-Trace"), Provider: "claude", AuthID: "a.json", RequestedAt: time.Now(), Failed: true, Fail: usage.Failure{StatusCode: 529}})
			c.JSON(529, gin.H{"error": "overloaded"})
		case "badkey":
			c.JSON(http.StatusUnauthorized, gin.H{"error": "bad key"})
		case "ok":
			c.JSON(http.StatusOK, gin.H{})
		default:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "auth_not_found", "message": "no auth available"}})
		}
	})
	for _, c := range []struct{ trace, kase string }{
		{"t-unrouted", "none"},
		{"t-routed", "routed"},
		{"t-badkey", "badkey"},
		{"t-ok", "ok"},
		{"", "none"}, // not an AI path: the logger gave it no trace id
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("X-Test-Trace", c.trace)
		req.Header.Set("X-Test-Case", c.kase)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	deadline := time.Now().Add(2 * time.Second)
	var rows []Row
	for time.Now().Before(deadline) {
		rows, _ = l.Range(0, 0)
		if len(rows) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	rows, _ = l.Range(0, 0)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v; want the routed row and one unrouted row", rows)
	}
	var unrouted Row
	for _, r := range rows {
		if r.AccountID == "" {
			unrouted = r
		}
	}
	if unrouted.TraceID != "t-unrouted" || unrouted.Status != 503 || unrouted.Endpoint != "/v1/messages" ||
		!strings.Contains(unrouted.Error, "no auth available") || unrouted.Provider != "" || unrouted.Model != nil {
		t.Errorf("unrouted row = %+v", unrouted)
	}
	// Left out by default, kept with unrouted=1, and never an account's last row.
	if got, _, _ := l.Query(UsageQuery{}); len(got) != 1 || got[0].AccountID != "a.json" {
		t.Errorf("default query = %+v", got)
	}
	if got, _, _ := l.Query(UsageQuery{Filter: UsageFilter{Unrouted: true}}); len(got) != 2 {
		t.Errorf("unrouted=1 query = %d rows", len(got))
	}
	if _, ok := l.Last(""); ok {
		t.Error("the unrouted row became an account's last row")
	}
	if _, err := os.Stat(filepath.Join(base, UsageFileName)); !os.IsNotExist(err) {
		t.Error("legacy file written")
	}
}
