// Package buffcloud holds what this fork adds for Buff Cloud's AI page: a
// per-request usage log in the schema Buff Cloud reads (proxy/types.ts
// UsageRecord), the summary it shows, and the Codex usage probe.
package buffcloud

import (
	"strings"

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
	// Error is the start of the upstream error body, for status 400 and above.
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

// RowFromRecord converts an upstream usage record into a usage.jsonl row; it
// reports false for a record without a credential. A Codex WebSocket prewarm
// (generate: false) is a row: the backend processes and counts its input.
func RowFromRecord(record usage.Record) (Row, bool) {
	if strings.TrimSpace(record.AuthID) == "" {
		return Row{}, false
	}
	provider := ProviderName(record.Provider)
	row := Row{
		At:        record.RequestedAt.UnixMilli(),
		AccountID: record.AuthID,
		Provider:  provider,
		Status:    200,
		Ms:        record.Latency.Milliseconds(),
		TraceID:   strings.TrimSpace(record.TraceID),
	}
	if record.Failed {
		row.Status = record.Fail.StatusCode
	}
	if model := firstNonEmpty(record.ResponseModel, record.Model, record.Alias); model != "" {
		row.Model = &model
	}
	if key := SessionKey(record.SessionID); key != "" {
		row.SessionKey = &key
	}
	row.Tokens = tokensOf(provider, record.Detail)
	return row, true
}

// tokensOf is nil when the response carried no usage. Claude reports cache reads
// and writes beside input_tokens; Codex counts them inside input_tokens, so they
// are taken out of input to keep the four parts disjoint.
func tokensOf(provider string, d usage.Detail) *Tokens {
	if d.InputTokens == 0 && d.OutputTokens == 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 {
		return nil
	}
	t := Tokens{Input: d.InputTokens, Output: d.OutputTokens, CacheRead: d.CacheReadTokens, CacheWrite: d.CacheCreationTokens}
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
