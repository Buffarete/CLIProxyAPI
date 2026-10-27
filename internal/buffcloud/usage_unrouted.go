package buffcloud

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

// unroutedBodyLimit is how much of a refused request's reply is kept for its error text.
const unroutedBodyLimit = 4 << 10

// addRequestContext fills the row's fields that come from the client's
// request rather than from the upstream record.
func addRequestContext(ctx context.Context, row *Row) {
	if ctx == nil {
		return
	}
	row.Endpoint = endpointOf(ctx)
}

// endpointOf is the client's path: the handler's "METHOD /path" endpoint
// without the method, else the gin request's path.
func endpointOf(ctx context.Context) string {
	if endpoint := strings.TrimSpace(logging.GetEndpoint(ctx)); endpoint != "" {
		if _, path, ok := strings.Cut(endpoint, " "); ok {
			return strings.TrimSpace(path)
		}
		return endpoint
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil && c.Request.URL != nil {
		return c.Request.URL.Path
	}
	return ""
}

type headWriter struct {
	gin.ResponseWriter
	head []byte
}

func (w *headWriter) keep(p []byte) {
	if room := unroutedBodyLimit - len(w.head); room > 0 {
		w.head = append(w.head, p[:min(len(p), room)]...)
	}
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.keep(p)
	return w.ResponseWriter.Write(p)
}

func (w *headWriter) WriteString(s string) (int, error) {
	w.keep([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// UnroutedMiddleware writes a row with accountId "" for each model request
// that failed without reaching an account: status 400 or above (401 and 404
// aside, which are a bad client key or an unknown path) and no usage row under
// its trace id once the usage queue has had time to deliver one. Only requests
// the logger gave a trace id, the AI API paths, are considered.
func (l *UsageLog) UnroutedMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		started := time.Now()
		head := &headWriter{ResponseWriter: c.Writer}
		c.Writer = head
		c.Next()
		c.Writer = head.ResponseWriter

		status := head.Status()
		if status < 400 || status == http.StatusUnauthorized || status == http.StatusNotFound {
			return
		}
		traceID := logging.GetRequestID(c.Request.Context())
		if traceID == "" {
			return
		}
		row := Row{
			At:       started.UnixMilli(),
			Status:   status,
			Ms:       time.Since(started).Milliseconds(),
			TraceID:  traceID,
			Endpoint: c.Request.URL.Path,
			Error:    ErrorText(string(head.head)),
		}
		addClientContext(c.Request.Context(), &row)
		go l.writeUnrouted(row)
	}
}

func (l *UsageLog) writeUnrouted(row Row) {
	deadline := time.Now().Add(l.unroutedWait)
	for {
		if _, ok := l.ByTrace(row.TraceID); ok {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(l.unroutedPoll)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(row); err != nil {
		log.WithError(err).Warn("buff-cloud: appending unrouted usage row")
	}
}

// addClientContext fills the row's client fields from the request.
func addClientContext(_ context.Context, _ *Row) {}
