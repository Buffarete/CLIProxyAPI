package executor

// Buff Cloud fork: the usage.jsonl row each upstream response produces, from the
// usage the executors already parse out of the response they forward.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type buffRowCapture struct {
	authID string
	rows   chan buffcloud.Row
}

func (c *buffRowCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID != c.authID {
		return
	}
	if row, ok := buffcloud.RowFromRecord(record); ok {
		c.rows <- row
	}
}

func captureBuffRow(t *testing.T, authID string) *buffRowCapture {
	t.Helper()
	c := &buffRowCapture{authID: authID, rows: make(chan buffcloud.Row, 4)}
	usage.RegisterNamedPlugin(t.Name(), c)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), noopClaudeUsagePlugin{}) })
	return c
}

func (c *buffRowCapture) await(t *testing.T) buffcloud.Row {
	t.Helper()
	select {
	case row := <-c.rows:
		return row
	case <-time.After(5 * time.Second):
		t.Fatal("no usage row")
		return buffcloud.Row{}
	}
}

func rowJSON(t *testing.T, row buffcloud.Row) string {
	t.Helper()
	row.At, row.Ms = 0, 0
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sessionCtx(sessionID string) context.Context {
	return logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{SessionID: sessionID})
}

const buffClaudeStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","stop_reason":null,"usage":{"input_tokens":3,"cache_creation_input_tokens":200,"cache_read_input_tokens":30000,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"cache_creation_input_tokens":200,"cache_read_input_tokens":30000,"output_tokens":57}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestBuffUsageRowFromClaudeStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(buffClaudeStream))
	}))
	defer server.Close()
	capture := captureBuffRow(t, "linked-claude_3.json")

	auth := &cliproxyauth.Auth{ID: "linked-claude_3.json", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": server.URL}}
	started := time.Now()
	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(sessionCtx("claude:6f1c0c2e-0000-4000-8000-000000000000:agent:a981"), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"ping"}],"stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}

	row := capture.await(t)
	want := `{"at":0,"accountId":"linked-claude_3.json","provider":"anthropic","model":"claude-opus-5-5","status":200,"ms":0,` +
		`"tokens":{"input":3,"output":57,"cacheRead":30000,"cacheWrite":200},"sessionKey":"6f1c0c2e-0000-4000-8000-000000000000"}`
	if got := rowJSON(t, row); got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
	if row.At < started.UnixMilli() || row.At > time.Now().UnixMilli() {
		t.Errorf("at = %d, want the request's start", row.At)
	}
}

func TestBuffUsageRowFromClaudeJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":4000,"output_tokens":5}}`))
	}))
	defer server.Close()
	capture := captureBuffRow(t, "linked-claude.json")

	auth := &cliproxyauth.Auth{ID: "linked-claude.json", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": server.URL}}
	_, err := NewClaudeExecutor(&config.Config{}).Execute(sessionCtx("claude:11111111-2222-4333-8444-555555555555"), auth, cliproxyexecutor.Request{
		Model:   "claude-haiku-5-5",
		Payload: []byte(`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"ping"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"at":0,"accountId":"linked-claude.json","provider":"anthropic","model":"claude-haiku-5-5","status":200,"ms":0,` +
		`"tokens":{"input":12,"output":5,"cacheRead":4000,"cacheWrite":0},"sessionKey":"11111111-2222-4333-8444-555555555555"}`
	if got := rowJSON(t, capture.await(t)); got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
}

// Codex counts cached tokens inside input_tokens; the row takes them out so that
// input + output + cacheRead + cacheWrite is Codex's total_tokens.
func TestBuffUsageRowFromCodexStreamSplitsCachedTokensOutOfInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-6.1-sol"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"pong"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-6.1-sol","output":[],` +
			`"usage":{"input_tokens":12000,"input_tokens_details":{"cached_tokens":11000},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":12050}}}` + "\n\n"))
	}))
	defer server.Close()
	capture := captureBuffRow(t, "linked-codex.json")

	auth := &cliproxyauth.Auth{ID: "linked-codex.json", Provider: "codex", Attributes: map[string]string{"api_key": "k", "base_url": server.URL}}
	result, err := NewCodexExecutor(&config.Config{}).ExecuteStream(sessionCtx("codex:019a0000-0000-7000-8000-000000000001"), auth, cliproxyexecutor.Request{
		Model:   "gpt-6.1-sol",
		Payload: []byte(`{"model":"gpt-6.1-sol","input":"ping","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}

	row := capture.await(t)
	want := `{"at":0,"accountId":"linked-codex.json","provider":"openai","model":"gpt-6.1-sol","status":200,"ms":0,` +
		`"tokens":{"input":1000,"output":50,"cacheRead":11000,"cacheWrite":0},"sessionKey":"019a0000-0000-7000-8000-000000000001"}`
	if got := rowJSON(t, row); got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
	if row.Tokens.Total() != 12050 {
		t.Errorf("row total = %d, want Codex's total_tokens 12050", row.Tokens.Total())
	}
}

func TestBuffUsageRowFromErrorHasStatusAndNoTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
		w.Header().Set("retry-after", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit reached"}}`))
	}))
	defer server.Close()
	capture := captureBuffRow(t, "linked-claude_5.json")

	auth := &cliproxyauth.Auth{ID: "linked-claude_5.json", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": server.URL}}
	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"ping"}],"stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, Stream: true})
	if err == nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("error = %v, want the upstream 429", err)
	}

	want := `{"at":0,"accountId":"linked-claude_5.json","provider":"anthropic","model":"claude-opus-5-5","status":429,"ms":0,"tokens":null,"sessionKey":null}`
	if got := rowJSON(t, capture.await(t)); got != want {
		t.Errorf("row = %s\nwant  %s", got, want)
	}
}
