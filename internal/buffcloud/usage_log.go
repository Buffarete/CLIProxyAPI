package buffcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// UsageDirName is the usage log's directory in the config file's directory:
	// one JSON-lines file per local day, named by the day the request started.
	UsageDirName = "usage"
	// UsageFileName is the single usage log older builds wrote. It is read, never
	// written or deleted, until its rows age past the keep window.
	UsageFileName = "usage.jsonl"
	// UsageKeepDays is how long a day's file stays; BUFF_USAGE_KEEP_DAYS overrides it.
	UsageKeepDays = 180
)

const (
	tailChunk = 64 * 1024
	// lastRowHorizon bounds the startup scan for each account's last request.
	lastRowHorizon = 8 * 24 * time.Hour
	seenRequestIDs = 256
	// recentTraces is how many inbound requests' last rows are kept, enough to
	// cover every request that finishes while a failed one waits for its row.
	recentTraces = 2048
)

// UsageLog appends one row per upstream request to usage/<day>.jsonl and
// remembers each account's last row. It is a usage.Plugin.
type UsageLog struct {
	dir      string
	legacy   string
	keepDays int
	loc      *time.Location
	now      func() time.Time

	mu   sync.Mutex
	last map[string]Row
	// seen holds recent request ids: an attempt publishes one record, and a side
	// model (Codex's image tool) may publish a second under the same id.
	seen     map[string]struct{}
	seenRing []string
	// byTrace holds the last row of each recent inbound request, for its messages row.
	byTrace   map[string]Row
	traceRing []string
	pruned    string

	// unroutedWait and unroutedPoll pace the wait for a failed request's usage
	// row before it is logged as unrouted.
	unroutedWait, unroutedPoll time.Duration

	legacyMu    sync.Mutex
	legacyStamp legacyStamp
	legacyEndMs int64
}

// legacyStamp identifies a version of the legacy file, for its cached end.
type legacyStamp struct {
	size int64
	mod  time.Time
}

// OpenUsageLog returns the log kept in baseDir (the config file's directory):
// day files in baseDir/usage, and the legacy baseDir/usage.jsonl. It is seeded
// with each account's last row from the last 8 days of both.
func OpenUsageLog(baseDir string) *UsageLog {
	keep := UsageKeepDays
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BUFF_USAGE_KEEP_DAYS"))); err == nil && n > 0 {
		keep = n
	}
	return openUsageLog(baseDir, keep, time.Local, time.Now)
}

func openUsageLog(baseDir string, keepDays int, loc *time.Location, now func() time.Time) *UsageLog {
	l := &UsageLog{
		dir:      filepath.Join(baseDir, UsageDirName),
		legacy:   filepath.Join(baseDir, UsageFileName),
		keepDays: keepDays,
		loc:      loc,
		now:      now,
		last:     map[string]Row{},
		seen:     map[string]struct{}{},
		byTrace:  map[string]Row{},

		unroutedWait: usageWait,
		unroutedPoll: 250 * time.Millisecond,
	}
	l.mu.Lock()
	l.pruneLocked(now())
	l.mu.Unlock()
	rows, err := l.Range(now().Add(-lastRowHorizon).UnixMilli(), 0)
	if err != nil {
		log.WithError(err).Warn("buff-cloud: reading recent usage rows")
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

// HandleUsage appends the record's row as the request finishes. ctx is the
// request's, which names the client's endpoint.
func (l *UsageLog) HandleUsage(ctx context.Context, record usage.Record) {
	row := RowFromRecord(record)
	addRequestContext(ctx, &row)
	l.mu.Lock()
	defer l.mu.Unlock()
	if id := record.RequestID; id != "" {
		if _, dup := l.seen[id]; dup {
			return
		}
		l.markSeen(id)
	}
	if err := l.appendLocked(row); err != nil {
		log.WithError(err).Warn("buff-cloud: appending usage row")
	}
	l.rememberLocked(row)
}

// appendLocked writes the row to the file of the local day it started on and,
// once a day, removes files past the keep window.
func (l *UsageLog) appendLocked(row Row) error {
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	l.pruneLocked(l.now())
	return appendLine(l.fileFor(l.dayOf(row.At)), line)
}

func (l *UsageLog) dayOf(ms int64) string {
	return time.UnixMilli(ms).In(l.loc).Format(dayLayout)
}

func (l *UsageLog) fileFor(day string) string {
	return filepath.Join(l.dir, day+".jsonl")
}

// cutoffDay is the oldest day kept.
func (l *UsageLog) cutoffDay(now time.Time) string {
	return now.In(l.loc).AddDate(0, 0, -l.keepDays).Format(dayLayout)
}

func (l *UsageLog) pruneLocked(now time.Time) {
	today := now.In(l.loc).Format(dayLayout)
	if today == l.pruned {
		return
	}
	l.pruned = today
	cutoff := l.cutoffDay(now)
	for _, day := range listDays(l.dir) {
		if day < cutoff {
			if err := os.Remove(l.fileFor(day)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.WithError(err).Warn("buff-cloud: removing old usage file")
			}
		}
	}
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, errWrite := f.Write(append(line, '\n'))
	return errors.Join(errWrite, f.Close())
}

// listDays lists the days with a <day>.jsonl file in dir, oldest first.
func listDays(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range entries {
		if day, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && !e.IsDir() {
			if _, err := time.Parse(dayLayout, day); err == nil {
				days = append(days, day)
			}
		}
	}
	sort.Strings(days)
	return days
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

// rememberLocked keeps the row as its trace's last row, even with no account
// (a plugin executor's record), so the unrouted middleware sees the request
// was logged; only an account's last row needs the account.
func (l *UsageLog) rememberLocked(row Row) {
	if row.TraceID != "" {
		if _, ok := l.byTrace[row.TraceID]; !ok {
			if len(l.traceRing) >= recentTraces {
				delete(l.byTrace, l.traceRing[0])
				l.traceRing = l.traceRing[1:]
			}
			l.traceRing = append(l.traceRing, row.TraceID)
		}
		l.byTrace[row.TraceID] = row
	}
	if row.AccountID == "" {
		return
	}
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

// ByTrace is the last upstream row of a recent inbound request.
func (l *UsageLog) ByTrace(traceID string) (Row, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.byTrace[traceID]
	return row, ok
}

// Since returns the rows of requests that started at or after since (epoch ms),
// oldest first, unrouted rows included.
func (l *UsageLog) Since(since int64) ([]Row, error) {
	return l.Range(since, 0)
}

// Range returns the rows of requests that started in [since, until) (epoch ms;
// until 0 means no bound), oldest first, unrouted rows included. It reads only
// the day files the range covers, and the legacy file only when the range
// reaches back into it.
func (l *UsageLog) Range(since, until int64) ([]Row, error) {
	var rows []Row
	err := l.scan(since, until, false, func(batch []Row, _ int64) bool {
		rows = append(rows, batch...)
		return true
	})
	sortRows(rows, false)
	return rows, err
}

// scan calls fn with batches of rows in [since, until), in file order inside a
// batch; fn returns false to stop. Oldest first, each batch is the legacy file
// or a whole day. Newest first, each batch is a day, with the legacy file's
// rows added once they may reach that day; complete is the time from which
// every row has now been given, so fn may stop once it has enough rows at or
// after it.
func (l *UsageLog) scan(since, until int64, newestFirst bool, fn func(batch []Row, complete int64) bool) error {
	since = max(since, l.keepStart())
	if until > 0 && until <= since {
		return nil
	}
	days := l.daysIn(since, until)
	legacyEnd, hasLegacy, err := l.legacyEnd()
	if err != nil {
		return err
	}
	// legacyDue reports whether the legacy file may hold rows at or after ms.
	legacyDue := func(ms int64) bool { return hasLegacy && legacyEnd >= max(ms, since) }
	readLegacy := func() ([]Row, error) {
		hasLegacy = false
		return l.legacyRange(since, until)
	}
	if !newestFirst {
		if legacyDue(since) {
			batch, err := readLegacy()
			if err != nil {
				return err
			}
			if !fn(batch, since) {
				return nil
			}
		}
		for _, day := range days {
			batch, err := l.readDay(day, since, until)
			if err != nil {
				return err
			}
			if !fn(batch, since) {
				return nil
			}
		}
		return nil
	}
	for i := len(days) - 1; i >= 0; i-- {
		batch, err := l.readDay(days[i], since, until)
		if err != nil {
			return err
		}
		start := l.dayStart(days[i])
		if legacyDue(start) {
			more, err := readLegacy()
			if err != nil {
				return err
			}
			batch = append(batch, more...)
		}
		if !fn(batch, max(start, since)) {
			return nil
		}
	}
	if legacyDue(since) {
		batch, err := readLegacy()
		if err != nil {
			return err
		}
		fn(batch, since)
	}
	return nil
}

// keepStart is the start of the oldest day kept, so the legacy file's old rows
// age out as day files do.
func (l *UsageLog) keepStart() int64 {
	return l.dayStart(l.cutoffDay(l.now()))
}

func (l *UsageLog) dayStart(day string) int64 {
	t, err := time.ParseInLocation(dayLayout, day, l.loc)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// daysIn lists the day files that can hold rows started in [since, until), oldest first.
func (l *UsageLog) daysIn(since, until int64) []string {
	first := l.dayOf(since)
	last := ""
	if until > 0 {
		last = l.dayOf(until - 1)
	}
	var days []string
	for _, day := range listDays(l.dir) {
		if day >= first && (last == "" || day <= last) {
			days = append(days, day)
		}
	}
	return days
}

// readDay returns the day file's rows in [since, until), in file order. A file
// pruned meanwhile has none.
func (l *UsageLog) readDay(day string, since, until int64) ([]Row, error) {
	data, err := os.ReadFile(l.fileFor(day))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Row
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		if row, ok := parseRow(line); ok && row.At >= since && (until <= 0 || row.At < until) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// legacyEnd is an upper bound on when the legacy file's requests started: the
// last row's end, since rows are appended as requests finish. It is cached
// while the file is unchanged.
func (l *UsageLog) legacyEnd() (int64, bool, error) {
	info, err := os.Stat(l.legacy)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	stamp := legacyStamp{size: info.Size(), mod: info.ModTime()}
	l.legacyMu.Lock()
	defer l.legacyMu.Unlock()
	if stamp == l.legacyStamp {
		return l.legacyEndMs, l.legacyEndMs > 0, nil
	}
	end := int64(0)
	errScan := eachLineBackwards(l.legacy, func(line []byte) bool {
		if row, ok := parseRow(line); ok {
			end = max(end, row.At+row.Ms)
		}
		// The last whole rows are enough; the first line seen may be cut off.
		return end == 0
	})
	if errScan != nil {
		return 0, false, errScan
	}
	l.legacyStamp, l.legacyEndMs = stamp, end
	return end, end > 0, nil
}

// legacyRange returns the legacy file's rows in [since, until), in file order.
// It reads the file backwards and stops at a row that finished before since:
// every earlier row also finished, and started, before it.
func (l *UsageLog) legacyRange(since, until int64) ([]Row, error) {
	var rows []Row
	err := eachLineBackwards(l.legacy, func(line []byte) bool {
		row, ok := parseRow(line)
		if !ok {
			return true
		}
		if row.At+row.Ms < since {
			return false
		}
		if row.At >= since && (until <= 0 || row.At < until) {
			rows = append(rows, row)
		}
		return true
	})
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, err
}

// sortRows orders rows by start, keeping file order between equal starts.
func sortRows(rows []Row, newestFirst bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if newestFirst {
			return rows[i].At > rows[j].At
		}
		return rows[i].At < rows[j].At
	})
}

// eachLineBackwards calls fn with each non-empty line of the file, last first,
// until fn returns false.
func eachLineBackwards(path string, fn func([]byte) bool) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pos := info.Size()
	var rest []byte
	for pos > 0 {
		start := max(0, pos-tailChunk)
		part := make([]byte, pos-start)
		if _, err := f.ReadAt(part, start); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pos = start
		buf := append(part, rest...)
		for {
			nl := bytes.LastIndexByte(buf, '\n')
			if nl < 0 {
				break
			}
			line := bytes.TrimSpace(buf[nl+1:])
			buf = buf[:nl]
			if len(line) > 0 && !fn(line) {
				return nil
			}
		}
		rest = buf
	}
	if line := bytes.TrimSpace(rest); len(line) > 0 {
		fn(line)
	}
	return nil
}

func parseRow(line []byte) (Row, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Row{}, false
	}
	var row Row
	if json.Unmarshal(line, &row) != nil || row.At == 0 {
		return Row{}, false
	}
	return row, true
}
