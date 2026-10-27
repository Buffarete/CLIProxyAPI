package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
)

// buffUsageServer serves the usage routes over a log holding rows.
func buffUsageServer(t *testing.T, rows ...buffcloud.Row) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	base := t.TempDir()
	for _, r := range rows {
		line, _ := json.Marshal(r)
		dir := filepath.Join(base, buffcloud.UsageDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, time.UnixMilli(r.At).Format("2006-01-02")+".jsonl")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	prev := buffcloud.DefaultUsageLog()
	buffcloud.SetDefaultUsageLog(buffcloud.OpenUsageLog(base))
	t.Cleanup(func() { buffcloud.SetDefaultUsageLog(prev) })
	h := &Handler{}
	router := gin.New()
	router.GET("/buff/usage", h.BuffUsage)
	router.GET("/buff/usage/series", h.BuffUsageSeries)
	return router
}

func getJSON(t *testing.T, router *gin.Engine, url string, wantStatus int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != wantStatus {
		t.Fatalf("GET %s: %d, want %d: %s", url, rec.Code, wantStatus, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestBuffUsageKeepsItsShapeAndAddsParams(t *testing.T) {
	now := time.Now()
	model := "claude-opus-5-5"
	row := func(ago time.Duration, account string, status int) buffcloud.Row {
		return buffcloud.Row{At: now.Add(-ago).UnixMilli(), AccountID: account, Provider: "anthropic", Model: &model, Status: status, Ms: 10}
	}
	router := buffUsageServer(t,
		row(3*time.Hour, "a.json", 200),
		row(2*time.Hour, "b.json", 429),
		row(90*time.Minute, "", 503),
		row(time.Hour, "a.json", 500),
	)
	since := now.Add(-4 * time.Hour).UnixMilli()

	old := getJSON(t, router, "/buff/usage?since="+itoa(since), http.StatusOK)
	if keys(old) != "rows,since" {
		t.Fatalf("old reply keys = %s", keys(old))
	}
	rows := old["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("old reply has %d rows; want 3 (the unrouted row left out)", len(rows))
	}
	first := rows[0].(map[string]any)
	if keys(first) != "accountId,at,model,ms,provider,sessionKey,status,tokens" || first["accountId"] != "a.json" {
		t.Errorf("old row = %v", first)
	}

	paged := getJSON(t, router, "/buff/usage?since="+itoa(since)+"&limit=2", http.StatusOK)
	if keys(paged) != "next,rows,since,until" {
		t.Fatalf("paged reply keys = %s", keys(paged))
	}
	page := paged["rows"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["status"] != 500.0 {
		t.Fatalf("first page = %v", page)
	}
	rest := getJSON(t, router, "/buff/usage?since="+itoa(since)+"&limit=2&cursor="+paged["next"].(string), http.StatusOK)
	if keys(rest) != "rows,since,until" || len(rest["rows"].([]any)) != 1 {
		t.Fatalf("second page = %v", rest)
	}

	filtered := getJSON(t, router, "/buff/usage?since="+itoa(since)+"&account=!a.json&unrouted=1", http.StatusOK)
	if got := filtered["rows"].([]any); len(got) != 2 || got[0].(map[string]any)["accountId"] != "b.json" {
		t.Errorf("account=!a.json&unrouted=1 = %v", got)
	}
	for _, q := range []string{"until=x", "status=nope", "limit=0", "limit=2&cursor=bad", "cursor=abc", "unrouted=2"} {
		getJSON(t, router, "/buff/usage?"+q, http.StatusBadRequest)
	}

	series := getJSON(t, router, "/buff/usage/series?since="+itoa(since)+"&bucket=hour&groupBy=account&tz=Europe/London", http.StatusOK)
	if series["tz"] != "Europe/London" || series["groupBy"] != "account" || len(series["buckets"].([]any)) != 3 {
		t.Errorf("series = %v", series)
	}
	for _, q := range []string{"bucket=minute", "groupBy=colour", "tz=Mars/Base", "since=0&bucket=5m"} {
		getJSON(t, router, "/buff/usage/series?"+q, http.StatusBadRequest)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestBuffAccountsListsFeatures(t *testing.T) {
	s := newBuffTestServer(t)
	body := s.do(http.MethodGet, "/v8/management/buff/accounts", "", http.StatusOK)
	service := body["service"].(map[string]any)
	features, _ := service["features"].([]any)
	if len(features) == 0 || features[0] != "usage-range" {
		t.Errorf("features = %v", service["features"])
	}
}
