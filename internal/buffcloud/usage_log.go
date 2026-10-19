package buffcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// UsageFileName is the usage log's name in the config file's directory.
const UsageFileName = "usage.jsonl"

const (
	tailChunk = 64 * 1024
	// lastRowHorizon bounds the startup scan for each account's last request.
	lastRowHorizon = 8 * 24 * time.Hour
	seenRequestIDs = 256
)

// UsageLog appends one row per upstream request to usage.jsonl and remembers
// each account's last row. It is a usage.Plugin.
type UsageLog struct {
	path string

	mu   sync.Mutex
	last map[string]Row
	// seen holds recent request ids: an attempt publishes one record, and a side
	// model (Codex's image tool) may publish a second under the same id.
	seen     map[string]struct{}
	seenRing []string
}

// OpenUsageLog returns the log at path, seeded with each account's last row
// from the file's recent tail.
func OpenUsageLog(path string) *UsageLog {
	l := &UsageLog{path: path, last: map[string]Row{}, seen: map[string]struct{}{}}
	rows, err := l.Since(time.Now().Add(-lastRowHorizon).UnixMilli())
	if err != nil {
		log.WithError(err).Warn("buff-cloud: reading usage log tail")
	}
	for _, row := range rows {
		l.remember(row)
	}
	return l
}

var defaultUsageLog atomic.Pointer[UsageLog]

// SetDefaultUsageLog makes l the log management endpoints read.
func SetDefaultUsageLog(l *UsageLog) { defaultUsageLog.Store(l) }

// DefaultUsageLog is the running server's usage log, or nil.
func DefaultUsageLog() *UsageLog { return defaultUsageLog.Load() }

// HandleUsage appends the record's row as the request finishes.
func (l *UsageLog) HandleUsage(_ context.Context, record usage.Record) {
	row, ok := RowFromRecord(record)
	if !ok {
		return
	}
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if id := record.RequestID; id != "" {
		if _, dup := l.seen[id]; dup {
			return
		}
		l.markSeen(id)
	}
	if err := appendLine(l.path, line); err != nil {
		log.WithError(err).Warn("buff-cloud: appending usage row")
	}
	l.rememberLocked(row)
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, errWrite := f.Write(append(line, '\n'))
	return errors.Join(errWrite, f.Close())
}

func (l *UsageLog) markSeen(id string) {
	if len(l.seenRing) >= seenRequestIDs {
		delete(l.seen, l.seenRing[0])
		l.seenRing = l.seenRing[1:]
	}
	l.seen[id] = struct{}{}
	l.seenRing = append(l.seenRing, id)
}

func (l *UsageLog) remember(row Row) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rememberLocked(row)
}

func (l *UsageLog) rememberLocked(row Row) {
	if prev, ok := l.last[row.AccountID]; !ok || row.At >= prev.At {
		l.last[row.AccountID] = row
	}
}

// Last is the account's most recent request, if any in the last 8 days.
func (l *UsageLog) Last(accountID string) (Row, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.last[accountID]
	return row, ok
}

// Since returns the rows of requests that started at or after since (epoch ms),
// oldest first. It reads the file backwards a chunk at a time and stops at a row
// that finished before since: rows are appended when a request finishes, so
// every earlier row also finished, and started, before it.
func (l *UsageLog) Since(since int64) ([]Row, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	pos := info.Size()
	var buf []byte
	for pos > 0 {
		start := max(0, pos-tailChunk)
		part := make([]byte, pos-start)
		if _, err := f.ReadAt(part, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buf = append(part, buf...)
		pos = start
		if pos == 0 {
			break
		}
		// The first line may be cut off; judge by the first whole one.
		first := bytes.IndexByte(buf, '\n')
		if first < 0 {
			continue
		}
		next := bytes.IndexByte(buf[first+1:], '\n')
		if next < 0 {
			continue
		}
		if row, ok := parseRow(buf[first+1 : first+1+next]); ok && row.At+row.Ms < since {
			break
		}
	}
	if pos > 0 {
		buf = buf[bytes.IndexByte(buf, '\n')+1:]
	}
	var rows []Row
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if row, ok := parseRow(line); ok && row.At >= since {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func parseRow(line []byte) (Row, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Row{}, false
	}
	var row Row
	if json.Unmarshal(line, &row) != nil || row.AccountID == "" {
		return Row{}, false
	}
	return row, true
}
