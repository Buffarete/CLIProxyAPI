package buffcloud

// Buff Cloud fork: read a Claude account's usage through Claude Code itself.
// The proxy never sends a request of its own with a Claude plan token; Claude
// Code's /usage command (`claude -p /usage`) reads the account's limits, costs
// no tokens and takes well under a second. Its text is turned into the
// anthropic-ratelimit-unified-* headers a real response carries, so the router
// and Buff Cloud read it like any other observation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ClaudeUsageTimeout bounds one `claude -p /usage` run.
const ClaudeUsageTimeout = 45 * time.Second

// claudeBinary finds Claude Code: on PATH, else where its installer puts it.
func claudeBinary() string {
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "bin", "claude")
	}
	return "claude"
}

// ReadClaudeUsage runs Claude Code's /usage for the account logged in at dir
// and returns its limits as rate-limit headers.
func ReadClaudeUsage(ctx context.Context, dir string, now time.Time) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, ClaudeUsageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBinary(), "-p", "/usage", "--output-format", "json")
	cmd.Dir = os.TempDir()
	cmd.Env = claudeUsageEnv(os.Environ(), dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return nil, fmt.Errorf("claude /usage: %v %s", err, detail)
	}
	var out struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("claude /usage: unreadable answer: %v", err)
	}
	if out.IsError {
		return nil, fmt.Errorf("claude /usage: %s", firstLine(out.Result))
	}
	return ParseClaudeUsage(out.Result, now)
}

// claudeUsageEnv is the environment for one run: the account's config dir, and
// none of the variables that would send Claude Code through the proxy.
func claudeUsageEnv(environ []string, dir string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR":
			continue
		}
		env = append(env, kv)
	}
	// The default ~/.claude dir keeps its account in ~/.claude.json, which
	// Claude Code only reads without CLAUDE_CONFIG_DIR.
	if home, err := os.UserHomeDir(); err != nil || filepath.Clean(dir) != filepath.Join(home, ".claude") {
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	return env
}

// usageLine matches "Current session: 21% used · resets Oct 30, 1:19am (Europe/London)"
// and the week lines; the reset part is missing while a window has not started.
var usageLine = regexp.MustCompile(`^Current (session|week \(([^)]*)\)):\s*([\d.]+)% used(?:\s*·\s*resets (.+?)(?:\s*\(([^)]+)\))?)?\s*$`)

// ParseClaudeUsage turns /usage text into rate-limit headers: "session" is the
// 5h window, "week (all models)" the 7d one, and "week (Fable)" the per-model
// 7d_fable window.
func ParseClaudeUsage(text string, now time.Time) (http.Header, error) {
	headers := http.Header{}
	for _, line := range strings.Split(text, "\n") {
		m := usageLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		window := "5h"
		if m[1] != "session" {
			window = "7d"
			if scope := strings.ToLower(strings.TrimSpace(m[2])); scope != "" && scope != "all models" {
				window = "7d_" + strings.Fields(scope)[0]
			}
		}
		percent, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsNaN(percent) {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + window
		used := math.Min(math.Max(percent/100, 0), 1)
		headers.Set(prefix+"-Utilization", strconv.FormatFloat(used, 'f', -1, 64))
		status := "allowed"
		if used >= 1 {
			status = "rejected"
		}
		headers.Set(prefix+"-Status", status)
		if m[4] != "" {
			if reset, ok := parseUsageReset(m[4], m[5], now); ok {
				headers.Set(prefix+"-Reset", strconv.FormatInt(reset.Unix(), 10))
			}
		}
	}
	if headers.Get("Anthropic-Ratelimit-Unified-5h-Utilization") == "" && headers.Get("Anthropic-Ratelimit-Unified-7d-Utilization") == "" {
		return nil, fmt.Errorf("claude /usage: no limits in %q", firstLine(text))
	}
	return headers, nil
}

// parseUsageReset reads "Oct 30, 1:19am", "Nov 3, 9pm" or "9:59pm" in zone. The
// year is the one that puts the reset nearest to now, since resets are at most
// a week ahead.
func parseUsageReset(text, zone string, now time.Time) (time.Time, bool) {
	loc := time.Local
	if zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	text = strings.ToLower(strings.TrimSpace(text))
	local := now.In(loc)
	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm", "3:04pm", "3pm"} {
		// Month names match in any case; "pm" in a layout wants it lower case.
		t, err := time.ParseInLocation(layout, text, loc)
		if err != nil {
			continue
		}
		if !strings.Contains(layout, "Jan") {
			candidate := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if candidate.Before(now.Add(-time.Hour)) {
				candidate = candidate.AddDate(0, 0, 1)
			}
			return candidate, true
		}
		best := time.Time{}
		for _, year := range []int{local.Year() - 1, local.Year(), local.Year() + 1} {
			candidate := time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if best.IsZero() || math.Abs(candidate.Sub(now).Hours()) < math.Abs(best.Sub(now).Hours()) {
				best = candidate
			}
		}
		return best, true
	}
	return time.Time{}, false
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > 160 {
		line = line[:160]
	}
	return line
}

// CodexUsageHeaders turns wham/usage's rate_limit windows into the x-codex-*
// headers a Codex response carries; nil when the answer has none.
func CodexUsageHeaders(usage CodexUsage) http.Header {
	raw, ok := usage.Fields["rate_limit"]
	if !ok {
		return nil
	}
	var limit map[string]json.RawMessage
	if err := json.Unmarshal(raw, &limit); err != nil {
		return nil
	}
	headers := http.Header{}
	for key, window := range map[string]string{"primary_window": "primary", "secondary_window": "secondary"} {
		var w struct {
			UsedPercent        *float64 `json:"used_percent"`
			LimitWindowSeconds float64  `json:"limit_window_seconds"`
			ResetAt            int64    `json:"reset_at"`
		}
		if err := json.Unmarshal(limit[key], &w); err != nil || w.UsedPercent == nil {
			continue
		}
		prefix := "X-Codex-" + window
		headers.Set(prefix+"-Used-Percent", strconv.FormatFloat(*w.UsedPercent, 'f', -1, 64))
		if w.ResetAt > 0 {
			headers.Set(prefix+"-Reset-At", strconv.FormatInt(w.ResetAt, 10))
		}
		if w.LimitWindowSeconds > 0 {
			headers.Set(prefix+"-Window-Minutes", strconv.FormatFloat(w.LimitWindowSeconds/60, 'f', -1, 64))
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}
