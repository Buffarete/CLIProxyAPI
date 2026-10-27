package buffcloud

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrUsageCursor is a /buff/usage cursor this proxy did not give.
var ErrUsageCursor = errors.New("cursor is not one this proxy gave")

// UsageLimitMax caps a /buff/usage page.
const UsageLimitMax = 5000

// usageFilterFields are the exact-match filters /buff/usage and
// /buff/usage/series take, by query parameter.
var usageFilterFields = map[string]func(Row) string{
	"account":  func(r Row) string { return r.AccountID },
	"provider": func(r Row) string { return r.Provider },
	"model":    func(r Row) string { return derefString(r.Model) },
	"client":   func(r Row) string { return r.Client },
	"routine":  func(r Row) string { return r.Routine },
	"thread":   func(r Row) string { return r.Thread },
	"session":  func(r Row) string { return derefString(r.SessionKey) },
}

// usageQueryParams are the parameters added after `since`; with none of them,
// /buff/usage answers as it always has.
var usageQueryParams = []string{"until", "status", "limit", "cursor", "unrouted"}

// HasUsageQueryParams reports whether the query uses any parameter newer than since.
func HasUsageQueryParams(values url.Values) bool {
	for _, name := range usageQueryParams {
		if values.Has(name) {
			return true
		}
	}
	for name := range usageFilterFields {
		if values.Has(name) {
			return true
		}
	}
	return false
}

// match is one parameter's values: a row passes when it equals one of include
// (or include is empty) and none of exclude.
type match struct {
	include, exclude []string
}

func (m match) pass(value string) bool {
	for _, v := range m.exclude {
		if v == value {
			return false
		}
	}
	if len(m.include) == 0 {
		return true
	}
	for _, v := range m.include {
		if v == value {
			return true
		}
	}
	return false
}

func parseMatch(values []string) match {
	var m match
	for _, raw := range values {
		for _, v := range strings.Split(raw, ",") {
			v = strings.TrimSpace(v)
			if neg, ok := strings.CutPrefix(v, "!"); ok {
				if neg = strings.TrimSpace(neg); neg != "" {
					m.exclude = append(m.exclude, neg)
				}
			} else if v != "" {
				m.include = append(m.include, v)
			}
		}
	}
	return m
}

// UsageFilter selects rows. The zero value passes every routed row.
type UsageFilter struct {
	fields map[string]match
	status match
	// Unrouted keeps rows with no account, which are left out by default.
	Unrouted bool
}

// ParseUsageFilter reads the filters of a /buff/usage or /buff/usage/series query.
func ParseUsageFilter(values url.Values) (UsageFilter, error) {
	f := UsageFilter{fields: map[string]match{}}
	for name := range usageFilterFields {
		if vs, ok := values[name]; ok {
			if m := parseMatch(vs); len(m.include)+len(m.exclude) > 0 {
				f.fields[name] = m
			}
		}
	}
	if vs, ok := values["status"]; ok {
		f.status = parseMatch(vs)
		for _, v := range append(append([]string{}, f.status.include...), f.status.exclude...) {
			if v != "ok" && v != "error" {
				if _, err := strconv.Atoi(v); err != nil {
					return UsageFilter{}, fmt.Errorf("status must be ok, error, 429 or a number, not %q", v)
				}
			}
		}
	}
	switch strings.TrimSpace(values.Get("unrouted")) {
	case "", "0", "false":
	case "1", "true":
		f.Unrouted = true
	default:
		return UsageFilter{}, fmt.Errorf("unrouted must be 1 or 0")
	}
	return f, nil
}

// Pass reports whether the row is selected.
func (f UsageFilter) Pass(row Row) bool {
	if row.AccountID == "" && !f.Unrouted {
		return false
	}
	for name, m := range f.fields {
		if !m.pass(usageFilterFields[name](row)) {
			return false
		}
	}
	if len(f.status.include)+len(f.status.exclude) > 0 && !statusPass(f.status, row.Status) {
		return false
	}
	return true
}

// statusPass matches a status against ok (< 400), error (>= 400 and not 429)
// and exact codes.
func statusPass(m match, status int) bool {
	is := func(v string) bool {
		switch v {
		case "ok":
			return status < 400
		case "error":
			return isError(status)
		default:
			n, _ := strconv.Atoi(v)
			return n == status
		}
	}
	for _, v := range m.exclude {
		if is(v) {
			return false
		}
	}
	if len(m.include) == 0 {
		return true
	}
	for _, v := range m.include {
		if is(v) {
			return true
		}
	}
	return false
}

func isError(status int) bool { return status >= 400 && status != 429 }

// UsageQuery is a /buff/usage request.
type UsageQuery struct {
	// Since and Until bound when requests started: Since inclusive, Until
	// exclusive (0 for no bound).
	Since, Until int64
	Filter       UsageFilter
	// Limit, when above 0, pages rows newest first.
	Limit  int
	Cursor string
}

// usageCursor resumes a newest-first page: rows that started before At, and
// rows that started at At after the first Skip of them.
type usageCursor struct {
	At   int64
	Skip int
}

func (c usageCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d.%d", c.At, c.Skip)))
}

func decodeUsageCursor(s string) (usageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err == nil {
		at, skip, ok := strings.Cut(string(raw), ".")
		var c usageCursor
		c.At, err = strconv.ParseInt(at, 10, 64)
		if err == nil && ok {
			c.Skip, err = strconv.Atoi(skip)
			if err == nil && c.Skip >= 0 {
				return c, nil
			}
		}
	}
	return usageCursor{}, ErrUsageCursor
}

// Query returns the rows that pass the filter: oldest first without a limit;
// newest first with one, plus the cursor of the next page when more remain.
func (l *UsageLog) Query(q UsageQuery) ([]Row, string, error) {
	if q.Limit <= 0 {
		var rows []Row
		err := l.scan(q.Since, q.Until, false, func(batch []Row, _ int64) bool {
			for _, row := range batch {
				if q.Filter.Pass(row) {
					rows = append(rows, row)
				}
			}
			return true
		})
		sortRows(rows, false)
		return rows, "", err
	}
	limit := min(q.Limit, UsageLimitMax)
	var cursor usageCursor
	until := q.Until
	if q.Cursor != "" {
		c, err := decodeUsageCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		cursor = c
		if until <= 0 || c.At+1 < until {
			until = c.At + 1
		}
	}
	var rows []Row
	err := l.scan(q.Since, until, true, func(batch []Row, complete int64) bool {
		for _, row := range batch {
			if q.Filter.Pass(row) {
				rows = append(rows, row)
			}
		}
		// Enough whole rows past the skipped ones to fill the page and tell
		// whether another follows.
		n := 0
		for _, row := range rows {
			if row.At >= complete {
				n++
			}
		}
		return n <= cursor.Skip+limit
	})
	if err != nil {
		return nil, "", err
	}
	sortRows(rows, true)
	if q.Cursor != "" {
		skipped := 0
		for skipped < len(rows) && skipped < cursor.Skip && rows[skipped].At == cursor.At {
			skipped++
		}
		rows = rows[skipped:]
	}
	if len(rows) <= limit {
		return rows, "", nil
	}
	rows = rows[:limit]
	last := rows[limit-1].At
	next := usageCursor{At: last}
	for _, row := range rows {
		if row.At == last {
			next.Skip++
		}
	}
	if q.Cursor != "" && last == cursor.At {
		next.Skip += cursor.Skip
	}
	return rows, next.encode(), nil
}

// SeriesTokens are a group's tokens. Reasoning is part of output.
type SeriesTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

func (t *SeriesTokens) add(row Row) {
	if row.Tokens == nil {
		return
	}
	t.Input += row.Tokens.Input
	t.Output += row.Tokens.Output
	t.CacheRead += row.Tokens.CacheRead
	t.CacheWrite += row.Tokens.CacheWrite
	t.Reasoning += row.Tokens.Reasoning
}

// SeriesModel is one model's share of a group.
type SeriesModel struct {
	Requests int          `json:"requests"`
	Tokens   SeriesTokens `json:"tokens"`
}

// SeriesGroup is one group's counts in one bucket. The percentiles are
// nearest-rank over the group's rows; ttftP50 counts only rows with a ttftMs
// and is 0 when none has one.
type SeriesGroup struct {
	Key         string                  `json:"key"`
	Requests    int                     `json:"requests"`
	Errors      int                     `json:"errors"`
	RateLimited int                     `json:"rateLimited"`
	Tokens      SeriesTokens            `json:"tokens"`
	MsP50       int64                   `json:"msP50"`
	MsP95       int64                   `json:"msP95"`
	TTFTP50     int64                   `json:"ttftP50"`
	Models      map[string]*SeriesModel `json:"models"`

	ms, ttft []int64
}

// SeriesBucket is one time bucket; Start is epoch ms.
type SeriesBucket struct {
	Start  int64          `json:"start"`
	Groups []*SeriesGroup `json:"groups"`
}

// Series is the /buff/usage/series reply.
type Series struct {
	Since   int64          `json:"since"`
	Until   int64          `json:"until"`
	Bucket  string         `json:"bucket"`
	GroupBy string         `json:"groupBy"`
	TZ      string         `json:"tz"`
	Buckets []SeriesBucket `json:"buckets"`
}

// SeriesMaxBuckets caps the buckets one series reply may span.
const SeriesMaxBuckets = 2000

// SeriesQuery is a /buff/usage/series request.
type SeriesQuery struct {
	Since, Until int64
	Filter       UsageFilter
	// Bucket is 5m, hour, day or week.
	Bucket string
	// GroupBy is none, account, model, provider, client, routine, thread or status.
	GroupBy string
	Loc     *time.Location
}

var seriesGroupKeys = map[string]func(Row) string{
	"none":     func(Row) string { return "all" },
	"account":  func(r Row) string { return r.AccountID },
	"model":    modelKey,
	"provider": func(r Row) string { return r.Provider },
	"client":   func(r Row) string { return r.Client },
	"routine":  func(r Row) string { return r.Routine },
	"thread":   func(r Row) string { return r.Thread },
	"status":   func(r Row) string { return strconv.Itoa(r.Status) },
}

var seriesBucketSize = map[string]time.Duration{
	"5m":   5 * time.Minute,
	"hour": time.Hour,
	"day":  24 * time.Hour,
	"week": 7 * 24 * time.Hour,
}

func modelKey(r Row) string {
	if r.Model == nil || *r.Model == "" {
		return UnknownModel
	}
	return *r.Model
}

// Validate checks the bucket, the grouping and the span.
func (q SeriesQuery) Validate() error {
	size, ok := seriesBucketSize[q.Bucket]
	if !ok {
		return fmt.Errorf("bucket must be 5m, hour, day or week")
	}
	if _, ok := seriesGroupKeys[q.GroupBy]; !ok {
		return fmt.Errorf("groupBy must be none, account, model, provider, client, routine, thread or status")
	}
	if q.Until <= q.Since {
		return fmt.Errorf("until must be after since")
	}
	// A bucket may start up to one size before since; DST adds at most an hour.
	if n := (q.Until-q.Since)/size.Milliseconds() + 2; n > SeriesMaxBuckets {
		return fmt.Errorf("the range spans about %d %s buckets; at most %d are allowed", n, q.Bucket, SeriesMaxBuckets)
	}
	return nil
}

// BucketStart is the start of the bucket holding ms: 5 minutes and hours on
// the local clock, days at local midnight, weeks at Monday's local midnight.
func BucketStart(ms int64, bucket string, loc *time.Location) int64 {
	t := time.UnixMilli(ms).In(loc)
	switch bucket {
	case "5m", "hour":
		// From the wall clock, so a repeated hour at a DST change stays two buckets.
		into := time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
		if bucket == "5m" {
			into = time.Duration(t.Minute()%5)*time.Minute + time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())
		}
		return t.Add(-into).UnixMilli()
	case "week":
		y, m, d := t.Date()
		back := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc).UnixMilli()
	default:
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc).UnixMilli()
	}
}

// Series counts the filtered rows in [Since, Until) into buckets and groups.
// Empty buckets are left out.
func (l *UsageLog) Series(q SeriesQuery) (Series, error) {
	if q.Loc == nil {
		q.Loc = time.Local
	}
	out := Series{Since: q.Since, Until: q.Until, Bucket: q.Bucket, GroupBy: q.GroupBy, TZ: q.Loc.String(), Buckets: []SeriesBucket{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	keyOf := seriesGroupKeys[q.GroupBy]
	buckets := map[int64]map[string]*SeriesGroup{}
	err := l.scan(q.Since, q.Until, false, func(batch []Row, _ int64) bool {
		for _, row := range batch {
			if !q.Filter.Pass(row) {
				continue
			}
			start := BucketStart(row.At, q.Bucket, q.Loc)
			groups := buckets[start]
			if groups == nil {
				groups = map[string]*SeriesGroup{}
				buckets[start] = groups
			}
			key := keyOf(row)
			g := groups[key]
			if g == nil {
				g = &SeriesGroup{Key: key, Models: map[string]*SeriesModel{}}
				groups[key] = g
			}
			g.add(row)
		}
		return true
	})
	if err != nil {
		return out, err
	}
	starts := make([]int64, 0, len(buckets))
	for start := range buckets {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, start := range starts {
		b := SeriesBucket{Start: start, Groups: make([]*SeriesGroup, 0, len(buckets[start]))}
		for _, g := range buckets[start] {
			g.finish()
			b.Groups = append(b.Groups, g)
		}
		sort.Slice(b.Groups, func(i, j int) bool {
			if b.Groups[i].Requests != b.Groups[j].Requests {
				return b.Groups[i].Requests > b.Groups[j].Requests
			}
			return b.Groups[i].Key < b.Groups[j].Key
		})
		out.Buckets = append(out.Buckets, b)
	}
	return out, nil
}

func (g *SeriesGroup) add(row Row) {
	g.Requests++
	if row.Status == 429 {
		g.RateLimited++
	} else if isError(row.Status) {
		g.Errors++
	}
	g.Tokens.add(row)
	g.ms = append(g.ms, row.Ms)
	if row.TTFTMs > 0 {
		g.ttft = append(g.ttft, row.TTFTMs)
	}
	model := modelKey(row)
	m := g.Models[model]
	if m == nil {
		m = &SeriesModel{}
		g.Models[model] = m
	}
	m.Requests++
	m.Tokens.add(row)
}

func (g *SeriesGroup) finish() {
	g.MsP50, g.MsP95 = percentile(g.ms, 0.5), percentile(g.ms, 0.95)
	g.TTFTP50 = percentile(g.ttft, 0.5)
	g.ms, g.ttft = nil, nil
}

// percentile is the nearest-rank percentile, 0 for no values.
func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	rank := int(math.Ceil(p*float64(len(values)))) - 1
	return values[min(max(rank, 0), len(values)-1)]
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// LocalZoneName is the box's IANA time zone: TZ when set, else the target of
// /etc/localtime, else Go's name for the local zone.
func LocalZoneName() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if _, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return strings.TrimPrefix(tz, ":")
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			return name
		}
	}
	return time.Local.String()
}
