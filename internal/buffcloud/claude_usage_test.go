package buffcloud

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Recorded `claude -p /usage` answers (Claude Code 2.1.312) from three accounts on 2026-10-29.
const (
	usageClaude3 = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 21% used · resets Oct 30, 1:19am (Europe/London)\nCurrent week (all models): 25% used · resets Nov 3, 8:59pm (Europe/London)\nCurrent week (Fable): 0% used · resets Nov 3, 9pm (Europe/London)"
	usageClaude6 = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 0% used\nCurrent week (all models): 90% used · resets Nov 1, 5pm (Europe/London)\nCurrent week (Fable): 0% used · resets Nov 1, 5pm (Europe/London)"
	usageSpent   = "You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 0% used · resets Oct 29, 9:59pm (Europe/London)\nCurrent week (all models): 100% used · resets Nov 2, 8:59am (Europe/London)"
)

var usageNow = time.Date(2026, 10, 29, 20, 30, 0, 0, time.UTC)

func TestParseClaudeUsageReadsSessionWeekAndModelWindows(t *testing.T) {
	h, err := ParseClaudeUsage(usageClaude3, usageNow)
	if err != nil {
		t.Fatal(err)
	}
	london, _ := time.LoadLocation("Europe/London")
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization":       "0.21",
		"Anthropic-Ratelimit-Unified-5h-Reset":             epochOf(time.Date(2026, 10, 30, 1, 19, 0, 0, london)),
		"Anthropic-Ratelimit-Unified-7d-Utilization":       "0.25",
		"Anthropic-Ratelimit-Unified-7d-Reset":             epochOf(time.Date(2026, 11, 3, 20, 59, 0, 0, london)),
		"Anthropic-Ratelimit-Unified-7d_fable-Utilization": "0",
		"Anthropic-Ratelimit-Unified-7d-Status":            "allowed",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestParseClaudeUsageWithoutAResetAndAtTheLimit(t *testing.T) {
	h, err := ParseClaudeUsage(usageClaude6, usageNow)
	if err != nil || h.Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "0" || h.Get("Anthropic-Ratelimit-Unified-5h-Reset") != "" {
		t.Fatalf("session with no reset: %v %v", h, err)
	}
	h, _ = ParseClaudeUsage(usageSpent, usageNow)
	if h.Get("Anthropic-Ratelimit-Unified-7d-Utilization") != "1" || h.Get("Anthropic-Ratelimit-Unified-7d-Status") != "rejected" {
		t.Fatalf("spent week: %v", h)
	}
	if _, err := ParseClaudeUsage("Not logged in · Please run /login", usageNow); err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("a logged-out answer: err = %v", err)
	}
}

func TestClaudeUsageEnvLeavesTheProxyOut(t *testing.T) {
	env := claudeUsageEnv([]string{"PATH=/bin", "ANTHROPIC_BASE_URL=https://proxy", "ANTHROPIC_AUTH_TOKEN=x", "CLAUDE_CONFIG_DIR=/home/will/.claude-proxy"}, "/home/will/.claude_7")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "ANTHROPIC_") || strings.Contains(joined, ".claude-proxy") || !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/home/will/.claude_7") {
		t.Fatalf("env = %v", env)
	}
}

func TestCodexUsageHeadersFromWham(t *testing.T) {
	raw := json.RawMessage(`{"allowed":true,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1793324653},"secondary_window":{"used_percent":37,"limit_window_seconds":604800,"reset_at":1793780020}}`)
	h := CodexUsageHeaders(CodexUsage{Fields: map[string]json.RawMessage{"rate_limit": raw}})
	if h.Get("X-Codex-Secondary-Used-Percent") != "37" || h.Get("X-Codex-Primary-Reset-At") != "1793324653" || h.Get("X-Codex-Primary-Window-Minutes") != "300" {
		t.Fatalf("headers = %v", h)
	}
}

func epochOf(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
