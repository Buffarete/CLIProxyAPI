// Package buffcloud holds what this fork adds for Buff Cloud's AI page: a
// per-request usage log in the schema Buff Cloud reads (proxy/types.ts
// UsageRecord), the summary it shows, and the Codex usage probe.
package buffcloud

import (
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Tokens splits a request's tokens so that the four parts add up to its total,
// for Codex as for Claude.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	// Reasoning is the part of Output spent on reasoning, when the response says.
	Reasoning int64 `json:"reasoning,omitempty"`
}

// Total is the sum Buff Cloud shows as a request's tokens. Reasoning is inside Output.
func (t Tokens) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite
}

// add sums the four parts; the summary's split leaves reasoning out.
func (t Tokens) add(o Tokens) Tokens {
	return Tokens{Input: t.Input + o.Input, Output: t.Output + o.Output, CacheRead: t.CacheRead + o.CacheRead, CacheWrite: t.CacheWrite + o.CacheWrite}
}

// Row is one line of a usage file: one upstream request. Fields after TraceID
// are optional, so readers of the original schema are unaffected.
type Row struct {
	// At is when the request started, epoch ms.
	At int64 `json:"at"`
	// AccountID is the credential's auth file name.
	AccountID string `json:"accountId"`
	// Provider is "anthropic" or "openai", Buff Cloud's names.
	Provider   string  `json:"provider"`
	Model      *string `json:"model"`
	Status     int     `json:"status"`
	Ms         int64   `json:"ms"`
	Tokens     *Tokens `json:"tokens"`
	SessionKey *string `json:"sessionKey"`
	// TraceID is the inbound HTTP request's id, which messages.jsonl rows carry too.
	TraceID string `json:"traceId,omitempty"`
	// Error is the start of the upstream error body, for status 400 and above,
	// or the transport error for status 0 (no HTTP answer: DNS, dial, TLS).
	Error string `json:"error,omitempty"`
	// TTFTMs is the time to the first token.
	TTFTMs int64 `json:"ttftMs,omitempty"`
	Stream bool  `json:"stream,omitempty"`
	// RequestedModel is the model the client asked for, when it is not Model.
	RequestedModel string `json:"requestedModel,omitempty"`
	// Endpoint is the client's path, such as /v1/messages.
	Endpoint string `json:"endpoint,omitempty"`
	// Client and ClientVersion come from the User-Agent.
	Client        string `json:"client,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
	// Routine and Thread come from the X-Buff-Routine and X-Buff-Thread headers.
	Routine string `json:"routine,omitempty"`
	Thread  string `json:"thread,omitempty"`
}

// ProviderName maps an executor's provider to Buff Cloud's name for it.
func ProviderName(provider string) string {
	switch p := strings.ToLower(strings.TrimSpace(provider)); p {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	default:
		return p
	}
}

// SessionKey turns the proxy's session identity ("claude:<id>",
// "claude:<id>:agent:<agent>", "codex:<id>") into the client's own session id,
// which is what Claude Code transcripts and Codex rollout files are keyed by.
// A subagent's requests carry its parent session's id, as Claude Code's
// X-Claude-Code-Session-Id header does.
func SessionKey(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.Index(id, ":agent:"); i >= 0 {
		id = id[:i]
	}
	for _, prefix := range []string{"claude:", "codex:"} {
		if strings.HasPrefix(id, prefix) {
			return id[len(prefix):]
		}
	}
	return id
}

// RowFromRecord converts an upstream usage record into a usage row. A record
// without a credential is a row with accountId "". A Codex WebSocket prewarm
// (generate: false) is a row: the backend processes and counts its input.
func RowFromRecord(record usage.Record) Row {
	provider := ProviderName(record.Provider)
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	row := Row{
		At:        at.UnixMilli(),
		AccountID: strings.TrimSpace(record.AuthID),
		Provider:  provider,
		Status:    200,
		Ms:        record.Latency.Milliseconds(),
		TraceID:   strings.TrimSpace(record.TraceID),
		TTFTMs:    record.TTFT.Milliseconds(),
		Stream:    record.Stream,
	}
	if record.Failed {
		row.Status = record.Fail.StatusCode
		if row.Status >= 400 || row.Status == 0 {
			row.Error = ErrorText(record.Fail.Body)
		}
	}
	model := firstNonEmpty(record.ResponseModel, record.Model, record.Alias)
	if model != "" {
		row.Model = &model
	}
	if requested := strings.TrimSpace(record.Alias); requested != "" && requested != model {
		row.RequestedModel = requested
	}
	if key := SessionKey(record.SessionID); key != "" {
		row.SessionKey = &key
	}
	row.Tokens = tokensOf(provider, record.Detail)
	return row
}

// errorTextLimit caps a row's error text, in characters.
const errorTextLimit = 300

// errorBodyLimit is how much of an error body is looked at, in bytes, so a
// huge body costs no more than this to clean.
const errorBodyLimit = 8 << 10

// secretPatterns match strings that look like credentials: provider keys,
// bearer tokens, JWTs and key=value pairs naming a key or token.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\b(?:AIza[0-9A-Za-z_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abpr]-[A-Za-z0-9-]{10,}|ya29\.[A-Za-z0-9_.-]{8,})`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|token|secret|password|authorization)["']?\s*[:=]\s*["']?)[^\s"',}&;]{8,}`),
}

// ErrorText is an error body as a row keeps it: whitespace collapsed, anything
// that looks like a key or token replaced, cut to 300 characters.
func ErrorText(body string) string {
	if len(body) > errorBodyLimit {
		body = strings.ToValidUTF8(body[:errorBodyLimit], "")
	}
	text := strings.Join(strings.Fields(body), " ")
	for i, re := range secretPatterns {
		if i == len(secretPatterns)-1 {
			text = re.ReplaceAllString(text, "${1}[redacted]")
		} else {
			text = re.ReplaceAllString(text, "[redacted]")
		}
	}
	if runes := []rune(text); len(runes) > errorTextLimit {
		text = string(runes[:errorTextLimit-1]) + "…"
	}
	return text
}

// tokensOf is nil when the response carried no usage. Claude reports cache reads
// and writes beside input_tokens; Codex counts them inside input_tokens, so they
// are taken out of input to keep the four parts disjoint.
func tokensOf(provider string, d usage.Detail) *Tokens {
	if d.InputTokens == 0 && d.OutputTokens == 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 {
		return nil
	}
	t := Tokens{Input: d.InputTokens, Output: d.OutputTokens, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheCreationTokens, Reasoning: d.ReasoningTokens}
	if provider == "openai" {
		t.Input = max(0, d.InputTokens-d.CacheReadTokens-d.CacheCreationTokens)
	}
	return &t
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
