package buffcloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// disconnectedWriter is a transport whose client has gone: every flush fails.
type disconnectedWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *disconnectedWriter) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

func TestWrappersSurfaceAFlushErrorFromAClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := openUsageLog(t.TempDir(), UsageKeepDays, time.UTC, time.Now)
	l.unroutedWait, l.unroutedPoll = time.Millisecond, time.Millisecond
	m := OpenMessagesLog(t.TempDir(), nil)
	transport := &disconnectedWriter{ResponseRecorder: httptest.NewRecorder()}
	c, engine := gin.CreateTestContext(transport)
	var flushErr, ctrlErr error
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	// Both Buff Cloud wrappers sit between the SSE handler and the transport.
	engine.Use(l.UnroutedMiddleware(), m.Middleware())
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: {}\n\n")
		flushErr = httpwire.FlushResponse(c.Writer)
		ctrlErr = http.NewResponseController(c.Writer).Flush()
		c.Writer.Flush()
	})
	engine.HandleContext(c)

	if !errors.Is(flushErr, io.ErrClosedPipe) {
		t.Errorf("FlushResponse through the wrappers = %v, want the transport's error", flushErr)
	}
	if !errors.Is(ctrlErr, io.ErrClosedPipe) {
		t.Errorf("ResponseController.Flush through the wrappers = %v, want the transport's error", ctrlErr)
	}
	if transport.flushes != 3 {
		t.Errorf("transport flushes = %d, want 3 (each flush reaches it)", transport.flushes)
	}
}

func TestPluginRowWithNoAccountStopsTheUnroutedDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := openUsageLog(t.TempDir(), UsageKeepDays, time.UTC, time.Now)
	l.unroutedWait, l.unroutedPoll = 50*time.Millisecond, 5*time.Millisecond
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(withRequestID(c.Request.Context(), "t-plugin"))
	})
	router.Use(l.UnroutedMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		// A plugin executor's record carries no AuthID.
		l.HandleUsage(c.Request.Context(), usage.Record{RequestID: "p", TraceID: "t-plugin", Provider: "my-plugin", RequestedAt: time.Now(), Failed: true, Fail: usage.Failure{StatusCode: 502}})
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream"})
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if row, ok := l.ByTrace("t-plugin"); !ok || row.Provider != "my-plugin" {
		t.Fatalf("ByTrace = %+v, %v", row, ok)
	}
	time.Sleep(150 * time.Millisecond)
	rows, _ := l.Range(0, 0)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v; want only the plugin's row", rows)
	}
	if _, ok := l.Last(""); ok {
		t.Error("a row with no account became an account's last row")
	}
}

func TestEndpointIsTheRealPathForRoutedAndUnroutedRows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l := openUsageLog(t.TempDir(), UsageKeepDays, time.UTC, func() time.Time { return at })
	l.unroutedWait, l.unroutedPoll = time.Millisecond, time.Millisecond
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(withRequestID(c.Request.Context(), c.GetHeader("X-Trace")))
	})
	router.Use(l.UnroutedMiddleware())
	router.POST("/v1beta/models/*action", func(c *gin.Context) {
		if c.GetHeader("X-Trace") == "routed" {
			// What the handler puts in the execution context.
			ctx := withEndpoint(context.WithValue(c.Request.Context(), "gin", c), "POST "+c.FullPath())
			l.HandleUsage(ctx, usage.Record{RequestID: "r", TraceID: "routed", Provider: "gemini", AuthID: "g.json", RequestedAt: at})
			c.JSON(http.StatusOK, gin.H{})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no auth available"})
	})
	for _, trace := range []string{"routed", "unrouted"} {
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3-pro:generateContent", strings.NewReader(`{}`))
		req.Header.Set("X-Trace", trace)
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	deadline := time.Now().Add(2 * time.Second)
	var rows []Row
	for time.Now().Before(deadline) {
		if rows, _ = l.Range(0, 0); len(rows) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Endpoint != "/v1beta/models/gemini-3-pro:generateContent" {
			t.Errorf("%s row endpoint = %q, want the request path", r.TraceID, r.Endpoint)
		}
	}
}

func TestModelFilterMatchesUnknownLikeSeries(t *testing.T) {
	l, _ := queryLog(t)
	rows, _, err := l.Query(UsageQuery{Filter: mustFilter(t, "model=unknown")})
	if err != nil || len(rows) != 1 || rows[0].Model != nil {
		t.Fatalf("model=unknown = %+v, %v", rows, err)
	}
	rows, _, _ = l.Query(UsageQuery{Filter: mustFilter(t, "model=!unknown")})
	for _, r := range rows {
		if r.Model == nil {
			t.Errorf("model=!unknown kept a row with no model")
		}
	}
}

func TestErrorTextCutsLargeBodiesAndRedactsGoogleAndJSONTokens(t *testing.T) {
	for in, want := range map[string]string{
		`{"error":"bad credential ya29.a0AfB_byC1234567890abcdef"}`: `{"error":"bad credential [redacted]"}`,
		`{"token": "abcdefghijklmnop", "n": 1}`:                     `{"token": "[redacted]", "n": 1}`,
		`{"access_token": "abcdefghijklmnop"}`:                      `{"access_token": "[redacted]"}`,
		`{"max_tokens": 4096, "message": "too many tokens"}`:        `{"max_tokens": 4096, "message": "too many tokens"}`,
	} {
		if got := ErrorText(in); got != want {
			t.Errorf("ErrorText(%q) = %q, want %q", in, got, want)
		}
	}
	// A body past 8 KB is cut before cleaning; the text stays short and clean.
	body := strings.Repeat("x ", 5000) + "sk-ant-api03-SECRETSECRETSECRET"
	got := ErrorText(body)
	if strings.Contains(got, "SECRET") || len([]rune(got)) > 300 {
		t.Errorf("large body text = %d characters, holds secret: %v", len([]rune(got)), strings.Contains(got, "SECRET"))
	}
	// A cut in the middle of a character leaves valid UTF-8.
	got = ErrorText(strings.Repeat("a", errorBodyLimit-1) + "é")
	if !strings.HasPrefix(got, "aaa") || strings.ContainsRune(got, '�') {
		t.Errorf("cut text = %q...", got[:10])
	}
}

func withRequestID(ctx context.Context, id string) context.Context {
	return logging.WithRequestID(ctx, id)
}

func withEndpoint(ctx context.Context, endpoint string) context.Context {
	return logging.WithEndpoint(ctx, endpoint)
}
