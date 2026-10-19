package buffcloud

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageLogAppendsRowsAndReadsThemBackSinceATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), UsageFileName)
	l := OpenUsageLog(path)
	start := time.UnixMilli(1_792_400_000_000)
	// Enough rows to span several 64 KiB chunks when read backwards.
	for i := 0; i < 2000; i++ {
		l.HandleUsage(context.Background(), usage.Record{
			RequestID:   "req-" + string(rune('a'+i%26)) + time.Duration(i).String(),
			Provider:    "claude",
			AuthID:      "linked-claude.json",
			Model:       "claude-opus-5-5",
			RequestedAt: start.Add(time.Duration(i) * time.Minute),
			Latency:     1500 * time.Millisecond,
			SessionID:   "claude:" + strings.Repeat("0", 8) + "-1111-2222-3333-444444444444",
			Detail:      usage.Detail{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4},
		})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("usage.jsonl mode = %v, want 0600", info.Mode().Perm())
	}
	if info.Size() < 3*tailChunk {
		t.Fatalf("test file is %d bytes; want several chunks", info.Size())
	}

	since := start.Add(1990 * time.Minute).UnixMilli()
	rows, err := l.Since(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 || rows[0].At != since || rows[9].At != start.Add(1999*time.Minute).UnixMilli() {
		t.Fatalf("rows since minute 1990 = %d (first %v), want the last 10 oldest first", len(rows), rows)
	}

	all, err := l.Since(0)
	if err != nil || len(all) != 2000 {
		t.Fatalf("all rows = %d, %v; want 2000", len(all), err)
	}

	reopened := OpenUsageLog(path)
	last, ok := reopened.Last("linked-claude.json")
	if !ok || last.At != start.Add(1999*time.Minute).UnixMilli() {
		t.Errorf("reopened log's last row = %+v, %v; want the 2000th", last, ok)
	}
}

func TestUsageLogWritesBuffCloudsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), UsageFileName)
	l := OpenUsageLog(path)
	l.HandleUsage(context.Background(), usage.Record{
		RequestID: "r1", Provider: "codex", AuthID: "linked-codex.json", Model: "gpt-6.1-sol",
		RequestedAt: time.UnixMilli(1_792_400_000_123), Latency: 2345 * time.Millisecond,
		SessionID: "codex:019a0000-0000-7000-8000-000000000001",
		Detail:    usage.Detail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 30, CacheCreationTokens: 40},
	})
	// The image tool's side record reuses the attempt's id and is not a second request.
	l.HandleUsage(context.Background(), usage.Record{RequestID: "r1", Provider: "codex", AuthID: "linked-codex.json", Model: "gpt-image-1.5"})
	// No credential, no row.
	l.HandleUsage(context.Background(), usage.Record{RequestID: "r2", Provider: "codex"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("usage.jsonl has %d lines, want 1:\n%s", len(lines), data)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"at": 1792400000123.0, "accountId": "linked-codex.json", "provider": "openai", "model": "gpt-6.1-sol",
		"status": 200.0, "ms": 2345.0, "sessionKey": "019a0000-0000-7000-8000-000000000001",
		"tokens": map[string]any{"input": 30.0, "output": 20.0, "cacheRead": 30.0, "cacheWrite": 40.0},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("row = %s\nwant  %s", gotJSON, wantJSON)
	}
}

func TestSessionKeyIsTheClientsOwnSessionID(t *testing.T) {
	for in, want := range map[string]string{
		"claude:6f1c0c2e-0000-4000-8000-000000000000":                  "6f1c0c2e-0000-4000-8000-000000000000",
		"claude:6f1c0c2e-0000-4000-8000-000000000000:agent:a981deadbe": "6f1c0c2e-0000-4000-8000-000000000000",
		"codex:019a0000-0000-7000-8000-000000000001":                   "019a0000-0000-7000-8000-000000000001",
		"header:abc": "header:abc",
		"":           "",
	} {
		if got := SessionKey(in); got != want {
			t.Errorf("SessionKey(%q) = %q, want %q", in, got, want)
		}
	}
}
