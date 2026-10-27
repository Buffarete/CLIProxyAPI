package buffcloud

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

// MessagesDirName is the messages log's directory in the config file's
// directory: one JSON-lines file per day, kept for MessagesKeepDays.
const MessagesDirName = "messages"

const (
	// MessagesKeepDays is how long a day's file stays; BUFF_MESSAGES_KEEP_DAYS overrides it.
	MessagesKeepDays = 90
	// replyLimit caps how much of a response is kept for parsing.
	replyLimit = 8 << 20
	// usageWait is how long a finished request waits for its usage row, which
	// names the account; usage records arrive through a queue.
	usageWait  = 15 * time.Second
	queryLimit = 200
	// orderSlack is how long a request can run: rows go in finish order, not start order.
	orderSlack = time.Hour
)

// MessageRow is one line of a messages file: what the client added in one
// request, and the model's reply. Account, model, status and tokens come from
// the request's usage row (TraceID joins them) when it has one.
type MessageRow struct {
	At         int64   `json:"at"`
	TraceID    string  `json:"traceId"`
	Path       string  `json:"path"`
	AccountID  string  `json:"accountId,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Model      *string `json:"model"`
	Status     int     `json:"status"`
	Ms         int64   `json:"ms"`
	Tokens     *Tokens `json:"tokens"`
	SessionKey *string `json:"sessionKey"`
	Input      []Part  `json:"input"`
	Output     []Part  `json:"output"`
	Truncated  bool    `json:"truncated,omitempty"`
	// Routine and Thread are the request's X-Buff-Routine and X-Buff-Thread tags.
	Routine string `json:"routine,omitempty"`
	Thread  string `json:"thread,omitempty"`
}

// MessagesLog writes and reads the messages files.
type MessagesLog struct {
	dir      string
	keepDays int
	usage    func(traceID string) (Row, bool)

	mu     sync.Mutex
	pruned string
}

// OpenMessagesLog returns the log in dir. usage finds a request's usage row.
func OpenMessagesLog(dir string, usage func(traceID string) (Row, bool)) *MessagesLog {
	keep := MessagesKeepDays
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BUFF_MESSAGES_KEEP_DAYS"))); err == nil && n > 0 {
		keep = n
	}
	return &MessagesLog{dir: dir, keepDays: keep, usage: usage}
}

var defaultMessagesLog atomic.Pointer[MessagesLog]

// SetDefaultMessagesLog makes l the log management endpoints read.
func SetDefaultMessagesLog(l *MessagesLog) { defaultMessagesLog.Store(l) }

// DefaultMessagesLog is the running server's messages log, or nil.
func DefaultMessagesLog() *MessagesLog { return defaultMessagesLog.Load() }

const dayLayout = "2006-01-02"

func (l *MessagesLog) fileFor(at time.Time) string {
	return filepath.Join(l.dir, at.Format(dayLayout)+".jsonl")
}

// Append writes the row to its day's file and, once a day, removes old files.
func (l *MessagesLog) Append(row MessageRow) error {
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	at := time.UnixMilli(row.At)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	if day := at.Format(dayLayout); day != l.pruned {
		l.pruned = day
		l.prune(at)
	}
	return appendLine(l.fileFor(at), line)
}

func (l *MessagesLog) prune(now time.Time) {
	cutoff := now.AddDate(0, 0, -l.keepDays).Format(dayLayout)
	for _, day := range l.days() {
		if day < cutoff {
			_ = os.Remove(filepath.Join(l.dir, day+".jsonl"))
		}
	}
}

// days lists the days with a file, oldest first.
func (l *MessagesLog) days() []string { return listDays(l.dir) }

// MessageQuery filters Query. Zero values match everything.
type MessageQuery struct {
	// Before and Since bound At (epoch ms): Before is exclusive, for paging.
	Before, Since int64
	TraceID       string
	Session       string
	Account       string
	// Text matches any part's text or tool name, case-insensitively.
	Text  string
	Limit int
}

func (q MessageQuery) match(row MessageRow) bool {
	if q.Before > 0 && row.At >= q.Before || q.Since > 0 && row.At < q.Since {
		return false
	}
	if q.TraceID != "" && row.TraceID != q.TraceID {
		return false
	}
	if q.Session != "" && (row.SessionKey == nil || *row.SessionKey != q.Session) {
		return false
	}
	if q.Account != "" && row.AccountID != q.Account {
		return false
	}
	if q.Text == "" {
		return true
	}
	needle := strings.ToLower(q.Text)
	for _, p := range append(append([]Part{}, row.Input...), row.Output...) {
		if strings.Contains(strings.ToLower(p.Text), needle) || strings.Contains(strings.ToLower(p.Name), needle) {
			return true
		}
	}
	return row.Model != nil && strings.Contains(strings.ToLower(*row.Model), needle)
}

// Query returns matching rows, newest first, and whether more match beyond the limit.
func (l *MessagesLog) Query(q MessageQuery) ([]MessageRow, bool, error) {
	if q.Limit <= 0 || q.Limit > queryLimit {
		q.Limit = queryLimit
	}
	days := l.days()
	var rows []MessageRow
	for i := len(days) - 1; i >= 0; i-- {
		day, _ := time.ParseInLocation(dayLayout, days[i], time.Local)
		if q.Before > 0 && day.UnixMilli() >= q.Before {
			continue
		}
		if q.Since > 0 && day.AddDate(0, 0, 1).UnixMilli() <= q.Since {
			break
		}
		stop, past := false, false
		err := eachLineBackwards(filepath.Join(l.dir, days[i]+".jsonl"), func(line []byte) bool {
			var row MessageRow
			if json.Unmarshal(line, &row) != nil {
				return true
			}
			// Rows are written as requests finish, so a row far enough before since
			// means every older row is too.
			if q.Since > 0 && row.At < q.Since-orderSlack.Milliseconds() {
				past = true
				return false
			}
			if !q.match(row) {
				return true
			}
			if len(rows) == q.Limit {
				stop = true
				return false
			}
			rows = append(rows, row)
			return true
		})
		if err != nil {
			return nil, false, err
		}
		if stop {
			return rows, true, nil
		}
		if past {
			break
		}
	}
	return rows, false, nil
}

// messagePaths are the routes whose bodies hold a conversation.
var messagePaths = map[string]bool{
	"/v1/messages":         true,
	"/v1/chat/completions": true,
	"/v1/responses":        true,
}

type teeWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *teeWriter) keep(p []byte) {
	if room := replyLimit - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
}

func (w *teeWriter) Write(p []byte) (int, error) {
	w.keep(p)
	return w.ResponseWriter.Write(p)
}

func (w *teeWriter) WriteString(s string) (int, error) {
	w.keep([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// Flush, FlushError and Unwrap keep a client's disconnect visible to
// streaming handlers, as the logging package's trace writer does.
func (w *teeWriter) Flush() { _ = w.FlushError() }

// FlushError forwards the transport's flush error.
func (w *teeWriter) FlushError() error { return httpwire.FlushResponse(w.ResponseWriter) }

// Unwrap is the wrapped writer.
func (w *teeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type readCloser struct {
	io.Reader
	io.Closer
}

// Middleware records each conversation request's new input and its reply.
func (l *MessagesLog) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimSuffix(c.Request.URL.Path, "/")
		if c.Request.Method != http.MethodPost || !messagePaths[path] || c.Request.Body == nil {
			c.Next()
			return
		}
		original := c.Request.Body
		body, err := io.ReadAll(original)
		if err != nil {
			// A body over the size limit, or a broken one: the handler meets the same error.
			c.Request.Body = readCloser{io.MultiReader(bytes.NewReader(body), errReader{err}), original}
			c.Next()
			return
		}
		c.Request.Body = readCloser{bytes.NewReader(body), original}
		started := time.Now()
		tee := &teeWriter{ResponseWriter: c.Writer}
		c.Writer = tee
		c.Next()
		c.Writer = tee.ResponseWriter

		row := MessageRow{
			At:      started.UnixMilli(),
			TraceID: logging.GetRequestID(c.Request.Context()),
			Path:    path,
			Status:  tee.Status(),
			Ms:      time.Since(started).Milliseconds(),
		}
		var model struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &model) == nil && model.Model != "" {
			row.Model = &model.Model
		}
		if key := sessionHeader(c.Request.Header); key != "" {
			row.SessionKey = &key
		}
		meta := logging.GetClientRequestMetadata(c.Request.Context())
		row.Routine, row.Thread = meta.Routine, meta.Thread
		// A refused client (bad key, proxy turned off) sent nothing worth keeping.
		if row.Status == http.StatusUnauthorized || row.Status == http.StatusForbidden {
			return
		}
		reply := tee.buf.Bytes()
		go l.finish(row, body, reply)
	}
}

// sessionHeader is the client's own session id: Claude Code's header, or Codex's.
func sessionHeader(h http.Header) string {
	for _, name := range []string{"X-Claude-Code-Session-Id", "Session_id", "Session-Id", "Conversation_id"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

func (l *MessagesLog) finish(row MessageRow, body, reply []byte) {
	var inCut, outCut bool
	row.Input, inCut = InputOf(body)
	row.Output, outCut = OutputOf(reply)
	row.Truncated = inCut || outCut || len(reply) >= replyLimit
	if len(row.Input) == 0 && len(row.Output) == 0 {
		return
	}
	if row.TraceID != "" && l.usage != nil {
		deadline := time.Now().Add(usageWait)
		for {
			if u, ok := l.usage(row.TraceID); ok {
				row.AccountID, row.Provider, row.Tokens = u.AccountID, u.Provider, u.Tokens
				if u.Model != nil {
					row.Model = u.Model
				}
				if u.SessionKey != nil {
					row.SessionKey = u.SessionKey
				}
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	if err := l.Append(row); err != nil {
		log.WithError(err).Warn("buff-cloud: appending messages row")
	}
}
