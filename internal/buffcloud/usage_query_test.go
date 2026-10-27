package buffcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func strp(s string) *string { return &s }

// writeRows appends rows to path as JSON lines.
func writeRows(t *testing.T, path string, rows ...Row) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, r := range rows {
		line, _ := json.Marshal(r)
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func ats(rows []Row) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.At
	}
	return out
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zoneinfo for %s: %v", name, err)
	}
	return loc
}

func TestUsageLogWritesEachRowToTheLocalDayItStarted(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	base := t.TempDir()
	// 2026-10-20 23:59:30 in New York is 03:59:30 UTC on the 21st.
	late := time.Date(2026, 10, 20, 23, 59, 30, 0, ny)
	now := func() time.Time { return late.Add(time.Hour) }
	l := openUsageLog(base, UsageKeepDays, ny, now)
	for i, at := range []time.Time{late, late.Add(time.Minute)} {
		l.HandleUsage(context.Background(), usage.Record{
			RequestID: fmt.Sprint("r", i), Provider: "claude", AuthID: "a.json", Model: "m", RequestedAt: at, Latency: 90 * time.Second,
		})
	}
	dir := filepath.Join(base, UsageDirName)
	if got := strings.Join(listDays(dir), ","); got != "2026-10-20,2026-10-21" {
		t.Fatalf("day files = %s", got)
	}
	// A range inside the 21st reads only that day: the 20th's file is unreadable.
	if err := os.Chmod(filepath.Join(dir, "2026-10-20.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "2026-10-20.jsonl"), 0o600) })
	rows, err := l.Range(time.Date(2026, 10, 21, 0, 0, 0, 0, ny).UnixMilli(), 0)
	if err != nil || len(rows) != 1 || rows[0].At != late.Add(time.Minute).UnixMilli() {
		t.Fatalf("rows on the 21st = %v, %v", ats(rows), err)
	}
	if os.Getuid() != 0 {
		if _, err := l.Range(late.UnixMilli(), 0); err == nil {
			t.Error("a range over the 20th should have read its file")
		}
	}
}

func TestUsageLogPrunesOldDaysAndKeepsTheLegacyFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, UsageDirName)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, day := range []string{"2026-10-16", "2026-10-17", "2026-10-26"} {
		at, _ := time.ParseInLocation(dayLayout, day, time.UTC)
		writeRows(t, filepath.Join(dir, day+".jsonl"), Row{At: at.Add(time.Hour).UnixMilli(), AccountID: "a.json", Provider: "anthropic", Status: 200})
	}
	legacy := filepath.Join(base, UsageFileName)
	writeRows(t, legacy,
		Row{At: time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC).UnixMilli(), AccountID: "old.json", Provider: "anthropic", Status: 200},
		Row{At: time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC).UnixMilli(), AccountID: "a.json", Provider: "anthropic", Status: 200},
	)
	before, _ := os.ReadFile(legacy)
	beforeInfo, _ := os.Stat(legacy)

	clock := now
	l := openUsageLog(base, 10, time.UTC, func() time.Time { return clock })
	if got := strings.Join(listDays(dir), ","); got != "2026-10-17,2026-10-26" {
		t.Fatalf("after open, day files = %s; want the 16th gone (10 days kept)", got)
	}
	rows, err := l.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy row from the 10th is past the window; the one from the 18th is not.
	if len(rows) != 3 || rows[1].At != time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("rows = %v", ats(rows))
	}
	// A write on a later day prunes again.
	clock = now.AddDate(0, 0, 1)
	l.HandleUsage(context.Background(), usage.Record{RequestID: "x", Provider: "claude", AuthID: "a.json", RequestedAt: clock})
	if got := strings.Join(listDays(dir), ","); got != "2026-10-26,2026-10-28" {
		t.Fatalf("after a write on the 28th, day files = %s", got)
	}
	after, _ := os.ReadFile(legacy)
	afterInfo, _ := os.Stat(legacy)
	if string(after) != string(before) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Error("the legacy file changed")
	}
}

func TestUsageLogReadsTheLegacyFileAndDayFilesTogether(t *testing.T) {
	base := t.TempDir()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return day.Add(36 * time.Hour) }
	// Legacy rows end mid-morning on the 20th; day files take over from there.
	var legacy []Row
	for i := 0; i < 3000; i++ {
		legacy = append(legacy, Row{At: day.Add(-48*time.Hour + time.Duration(i)*time.Minute).UnixMilli(), AccountID: "legacy.json", Provider: "anthropic", Status: 200, Ms: 500})
	}
	writeRows(t, filepath.Join(base, UsageFileName), legacy...)
	writeRows(t, filepath.Join(base, UsageDirName, "2026-10-20.jsonl"),
		Row{At: day.Add(20 * time.Hour).UnixMilli(), AccountID: "new.json", Provider: "openai", Status: 200},
	)
	writeRows(t, filepath.Join(base, UsageDirName, "2026-10-21.jsonl"),
		Row{At: day.Add(25 * time.Hour).UnixMilli(), AccountID: "new.json", Provider: "openai", Status: 429},
	)
	l := openUsageLog(base, UsageKeepDays, time.UTC, now)

	// Seeding finds both files' accounts.
	if last, ok := l.Last("legacy.json"); !ok || last.At != legacy[2999].At {
		t.Errorf("legacy account's last row = %v, %v", last.At, ok)
	}
	if last, ok := l.Last("new.json"); !ok || last.Status != 429 {
		t.Errorf("new account's last row = %+v, %v", last, ok)
	}
	rows, err := l.Range(day.Add(-time.Hour).UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// 60 + 1000 legacy minutes from 23:00 on the 19th, then the two day rows.
	if len(rows) != 2+60+(3000-2*24*60) || rows[len(rows)-1].AccountID != "new.json" || rows[0].At != day.Add(-time.Hour).UnixMilli() {
		t.Fatalf("rows = %d, first %d", len(rows), rows[0].At)
	}
	// A range after the legacy file ends does not read it.
	if err := os.Chmod(filepath.Join(base, UsageFileName), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(base, UsageFileName), 0o600) })
	rows, err = l.Range(day.Add(19*time.Hour).UnixMilli(), 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows after the legacy file = %d, %v", len(rows), err)
	}
}

// queryLog has rows on three days, one a legacy file, with ties on At.
func queryLog(t *testing.T) (*UsageLog, []Row) {
	t.Helper()
	base := t.TempDir()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	mk := func(h time.Duration, acct, model string, status int) Row {
		r := Row{At: day.Add(h).UnixMilli(), AccountID: acct, Provider: "anthropic", Status: status, Ms: 100}
		if model != "" {
			r.Model = strp(model)
		}
		return r
	}
	legacy := []Row{
		mk(-30*time.Hour, "a.json", "opus", 200),
		mk(-29*time.Hour, "b.json", "sonnet", 500),
		mk(-29*time.Hour, "a.json", "opus", 429),
	}
	d20 := []Row{
		mk(1*time.Hour, "a.json", "opus", 200),
		mk(2*time.Hour, "b.json", "opus", 200),
		mk(2*time.Hour, "a.json", "sonnet", 529),
		mk(2*time.Hour, "", "opus", 503),
		mk(3*time.Hour, "b.json", "", 200),
	}
	d21 := []Row{
		mk(25*time.Hour, "a.json", "opus", 200),
		mk(26*time.Hour, "b.json", "sonnet", 429),
	}
	d21[0].Client, d21[0].Routine, d21[0].Thread, d21[0].SessionKey = "Codex", "nightly", "t-1", strp("s-1")
	writeRows(t, filepath.Join(base, UsageFileName), legacy...)
	writeRows(t, filepath.Join(base, UsageDirName, "2026-10-20.jsonl"), d20...)
	writeRows(t, filepath.Join(base, UsageDirName, "2026-10-21.jsonl"), d21...)
	l := openUsageLog(base, UsageKeepDays, time.UTC, func() time.Time { return day.Add(48 * time.Hour) })
	return l, append(append(legacy, d20...), d21...)
}

func mustFilter(t *testing.T, query string) UsageFilter {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseUsageFilter(values)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUsageQueryFilters(t *testing.T) {
	l, all := queryLog(t)
	count := func(query string) int {
		rows, next, err := l.Query(UsageQuery{Filter: mustFilter(t, query)})
		if err != nil || next != "" {
			t.Fatalf("%s: %v %q", query, err, next)
		}
		return len(rows)
	}
	routed := len(all) - 1
	for query, want := range map[string]int{
		"":                      routed,
		"unrouted=1":            len(all),
		"account=a.json":        5,
		"account=!a.json":       routed - 5,
		"account=a.json,b.json": routed,
		"account=a.json&account=b.json&account=!b.json": 5,
		"model=opus":                 5,
		"model=!opus&model=!sonnet":  1,
		"status=ok":                  5,
		"status=error":               2,
		"status=429":                 2,
		"status=error,429":           4,
		"status=!ok":                 4,
		"status=529":                 1,
		"client=Codex":               1,
		"routine=nightly&thread=t-1": 1,
		"routine=!nightly":           routed - 1,
		"session=s-1":                1,
		"provider=anthropic":         routed,
		"unrouted=1&account=":        len(all),
	} {
		if got := count(query); got != want {
			t.Errorf("%q: %d rows, want %d", query, got, want)
		}
	}
	if _, err := ParseUsageFilter(url.Values{"status": {"bad"}}); err == nil {
		t.Error("status=bad accepted")
	}
	if _, err := ParseUsageFilter(url.Values{"unrouted": {"maybe"}}); err == nil {
		t.Error("unrouted=maybe accepted")
	}
	// since and until bound At, until exclusive.
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	rows, _, _ := l.Query(UsageQuery{Since: day.Add(2 * time.Hour).UnixMilli(), Until: day.Add(25 * time.Hour).UnixMilli()})
	if len(rows) != 3 {
		t.Errorf("rows in [02:00, next 01:00) = %v", ats(rows))
	}
}

func TestUsageQueryPagesNewestFirst(t *testing.T) {
	l, all := queryLog(t)
	var routed []Row
	for _, r := range all {
		if r.AccountID != "" {
			routed = append(routed, r)
		}
	}
	sortRows(routed, true)
	for _, limit := range []int{1, 2, 3, 4, 100} {
		var got []Row
		cursor := ""
		for page := 0; ; page++ {
			rows, next, err := l.Query(UsageQuery{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) > limit || (next != "" && len(rows) != limit) {
				t.Fatalf("limit %d page %d: %d rows, next %q", limit, page, len(rows), next)
			}
			got = append(got, rows...)
			if next == "" {
				break
			}
			cursor = next
			if page > 20 {
				t.Fatal("paging does not end")
			}
		}
		if fmt.Sprint(ats(got)) != fmt.Sprint(ats(routed)) {
			t.Errorf("limit %d: pages = %v\nwant %v", limit, ats(got), ats(routed))
		}
		for i := range got {
			if got[i].AccountID != routed[i].AccountID || got[i].Status != routed[i].Status {
				t.Errorf("limit %d: row %d = %+v, want %+v", limit, i, got[i], routed[i])
			}
		}
	}
	if _, _, err := l.Query(UsageQuery{Limit: 2, Cursor: "garbage!"}); err != ErrUsageCursor {
		t.Errorf("bad cursor: %v", err)
	}
	// A filtered page.
	rows, next, _ := l.Query(UsageQuery{Limit: 2, Filter: mustFilter(t, "account=b.json")})
	if len(rows) != 2 || rows[0].Status != 429 || rows[1].At != time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC).UnixMilli() || next == "" {
		t.Errorf("b.json page = %+v, %q", rows, next)
	}
}

func TestUsageSeriesBucketsInATimeZone(t *testing.T) {
	kolkata := mustLoc(t, "Asia/Kolkata") // UTC+5:30
	base := t.TempDir()
	// Local midnight in Kolkata on the 21st is 18:30 UTC on the 20th.
	midnight := time.Date(2026, 10, 21, 0, 0, 0, 0, kolkata)
	mk := func(d time.Duration, acct, model string, status int, ms, ttft int64, in, reasoning int64) Row {
		return Row{At: midnight.Add(d).UnixMilli(), AccountID: acct, Provider: "anthropic", Model: strp(model), Status: status, Ms: ms, TTFTMs: ttft,
			Tokens: &Tokens{Input: in, Output: 10, CacheRead: 1, CacheWrite: 2, Reasoning: reasoning}}
	}
	rows := []Row{
		mk(-time.Minute, "a.json", "opus", 200, 100, 10, 5, 1),
		mk(0, "a.json", "opus", 200, 200, 20, 5, 2),
		mk(time.Minute, "a.json", "sonnet", 429, 300, 0, 5, 0),
		mk(2*time.Minute, "a.json", "opus", 500, 400, 40, 5, 0),
		mk(3*time.Minute, "b.json", "opus", 200, 1000, 50, 7, 0),
		mk(3*time.Minute, "", "opus", 503, 5, 0, 0, 0),
	}
	// Files are named by the box's day (UTC here), not the query's zone.
	writeRows(t, filepath.Join(base, UsageDirName, "2026-10-20.jsonl"), rows...)
	l := openUsageLog(base, UsageKeepDays, time.UTC, func() time.Time { return midnight.Add(time.Hour) })

	s, err := l.Series(SeriesQuery{
		Since: midnight.Add(-time.Hour).UnixMilli(), Until: midnight.Add(time.Hour).UnixMilli(),
		Bucket: "day", GroupBy: "account", Loc: kolkata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.TZ != "Asia/Kolkata" || len(s.Buckets) != 2 {
		t.Fatalf("series = %+v", s)
	}
	if s.Buckets[0].Start != midnight.AddDate(0, 0, -1).UnixMilli() || s.Buckets[1].Start != midnight.UnixMilli() {
		t.Errorf("bucket starts = %d, %d; want Kolkata midnights", s.Buckets[0].Start, s.Buckets[1].Start)
	}
	today := s.Buckets[1].Groups
	if len(today) != 2 || today[0].Key != "a.json" || today[1].Key != "b.json" {
		t.Fatalf("groups = %+v", today)
	}
	a := today[0]
	if a.Requests != 3 || a.Errors != 1 || a.RateLimited != 1 {
		t.Errorf("a.json counts = %+v", a)
	}
	if a.Tokens != (SeriesTokens{Input: 15, Output: 30, CacheRead: 3, CacheWrite: 6, Reasoning: 2}) {
		t.Errorf("a.json tokens = %+v", a.Tokens)
	}
	// Nearest rank over 200, 300, 400; ttft over 20 and 40.
	if a.MsP50 != 300 || a.MsP95 != 400 || a.TTFTP50 != 20 {
		t.Errorf("a.json percentiles = %d %d %d", a.MsP50, a.MsP95, a.TTFTP50)
	}
	if len(a.Models) != 2 || a.Models["opus"].Requests != 2 || a.Models["sonnet"].Tokens.Input != 5 {
		t.Errorf("a.json models = %+v %+v", a.Models["opus"], a.Models["sonnet"])
	}
	if b := today[1]; b.TTFTP50 != 50 || b.MsP95 != 1000 {
		t.Errorf("b.json = %+v", b)
	}

	// Unrouted rows count only when asked for; groupBy none keys everything "all".
	s, _ = l.Series(SeriesQuery{Since: midnight.UnixMilli(), Until: midnight.Add(time.Hour).UnixMilli(), Bucket: "hour", GroupBy: "none", Loc: kolkata, Filter: UsageFilter{Unrouted: true}})
	if len(s.Buckets) != 1 || s.Buckets[0].Groups[0].Key != "all" || s.Buckets[0].Groups[0].Requests != 5 {
		t.Errorf("unrouted series = %+v", s.Buckets)
	}
	s, _ = l.Series(SeriesQuery{Since: midnight.UnixMilli(), Until: midnight.Add(time.Hour).UnixMilli(), Bucket: "5m", GroupBy: "model", Loc: kolkata, Filter: mustFilter(t, "status=ok")})
	if len(s.Buckets) != 1 || len(s.Buckets[0].Groups) != 1 || s.Buckets[0].Groups[0].Key != "opus" || s.Buckets[0].Groups[0].Requests != 2 {
		t.Errorf("5m by model, ok only = %+v", s.Buckets)
	}

	if err := (SeriesQuery{Since: 0, Until: 2001 * 3600_000, Bucket: "hour", GroupBy: "none"}).Validate(); err == nil {
		t.Error("2001 hourly buckets accepted")
	}
	if err := (SeriesQuery{Since: 0, Until: 1000, Bucket: "minute", GroupBy: "none"}).Validate(); err == nil {
		t.Error("bucket=minute accepted")
	}
	if err := (SeriesQuery{Since: 0, Until: 1000, Bucket: "day", GroupBy: "colour"}).Validate(); err == nil {
		t.Error("groupBy=colour accepted")
	}
}

func TestBucketStart(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	kolkata := mustLoc(t, "Asia/Kolkata")
	for _, c := range []struct {
		at     time.Time
		bucket string
		loc    *time.Location
		want   time.Time
	}{
		{time.Date(2026, 10, 21, 10, 47, 12, 0, kolkata), "5m", kolkata, time.Date(2026, 10, 21, 10, 45, 0, 0, kolkata)},
		{time.Date(2026, 10, 21, 10, 47, 12, 0, kolkata), "hour", kolkata, time.Date(2026, 10, 21, 10, 0, 0, 0, kolkata)},
		{time.Date(2026, 10, 21, 0, 10, 0, 0, kolkata), "day", kolkata, time.Date(2026, 10, 21, 0, 0, 0, 0, kolkata)},
		// Wednesday the 21st: the week started on Monday the 19th.
		{time.Date(2026, 10, 21, 0, 10, 0, 0, kolkata), "week", kolkata, time.Date(2026, 10, 19, 0, 0, 0, 0, kolkata)},
		// Sunday belongs to the week that started six days before.
		{time.Date(2026, 10, 25, 23, 0, 0, 0, london), "week", london, time.Date(2026, 10, 19, 0, 0, 0, 0, london)},
		// London leaves summer time at 02:00 BST on 25 October 2026: the day is 25 hours long.
		{time.Date(2026, 10, 25, 23, 0, 0, 0, london), "day", london, time.Date(2026, 10, 25, 0, 0, 0, 0, london)},
	} {
		if got := BucketStart(c.at.UnixMilli(), c.bucket, c.loc); got != c.want.UnixMilli() {
			t.Errorf("%s %s = %s, want %s", c.at, c.bucket, time.UnixMilli(got).In(c.loc), c.want)
		}
	}
	// The repeated 01:00 hour is two buckets.
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)  // 01:30 BST
	second := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC) // 01:30 GMT
	a, b := BucketStart(first.UnixMilli(), "hour", london), BucketStart(second.UnixMilli(), "hour", london)
	if a == b || b-a != time.Hour.Milliseconds() {
		t.Errorf("repeated hour buckets = %d, %d", a, b)
	}
}

func TestUsageLogReadsWhileItWrites(t *testing.T) {
	base := t.TempDir()
	start := time.Date(2026, 10, 20, 23, 0, 0, 0, time.UTC)
	l := openUsageLog(base, UsageKeepDays, time.UTC, func() time.Time { return start.Add(3 * time.Hour) })
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.HandleUsage(context.Background(), usage.Record{
					RequestID: fmt.Sprintf("w%d-%d", w, i), Provider: "claude", AuthID: "a.json",
					RequestedAt: start.Add(time.Duration(i) * time.Minute), Detail: usage.Detail{InputTokens: 1},
				})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := l.Range(0, 0); err != nil {
					t.Error(err)
				}
				if _, _, err := l.Query(UsageQuery{Limit: 50}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	rows, err := l.Range(0, 0)
	if err != nil || len(rows) != 800 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
}
