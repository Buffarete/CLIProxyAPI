package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// A Claude Code 2.1.303 request as captured on box (abridged), in agent-sdk
// mode, which upstream treats as an unconfirmed client and re-fingerprints.
func claudeCodeCapturedRequest(stream bool) ([]byte, http.Header) {
	streamField := `false`
	if stream {
		streamField = `true`
	}
	body := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"text","text":"reply with the single word pong","cache_control":{"type":"ephemeral"}}]}],` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.303.abc; cc_entrypoint=sdk-ts; cch=1a2b3;"},{"type":"text","text":"You are a Claude agent.","cache_control":{"type":"ephemeral"}}],` +
		`"metadata":{"user_id":"{\"device_id\":\"d1\",\"account_uuid\":\"\",\"session_id\":\"d6a2d064-64d1-4d17-a87b-6a31bb7bf609\"}"},` +
		`"max_tokens":32000,"thinking":{"type":"adaptive"},"stream":` + streamField + `}`
	headers := http.Header{
		"Authorization":               {"Bearer proxy-client-key"},
		"Accept":                      {"application/json"},
		"Content-Type":                {"application/json"},
		"User-Agent":                  {"claude-cli/2.1.303 (external, sdk-ts, agent-sdk/0.3.276)"},
		"X-App":                       {"cli"},
		"X-Claude-Code-Session-Id":    {"d6a2d064-64d1-4d17-a87b-6a31bb7bf609"},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
		"X-Stainless-Package-Version": {"0.128.0"},
		"X-Stainless-Runtime":         {"node"},
		"Anthropic-Version":           {"2023-06-01"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"Anthropic-Beta":  {"claude-code-20250219,interleaved-thinking-2025-05-14,effort-2025-11-24"},
		"X-Forwarded-For": {"100.64.0.7"},
	}
	return []byte(body), headers
}

func TestClaudeExecutorCloakDisabledForwardsClaudeCodeRequestUnchanged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var seenBody []byte
		var seenHeaders http.Header
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			seenBody, _ = io.ReadAll(req.Body)
			seenHeaders = req.Header.Clone()
			if stream {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: req,
					Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))}, nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: req,
				Body: io.NopCloser(strings.NewReader(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
		})
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
		auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat01-linked"}, Metadata: claudeOAuthTestMetadata()}
		payload, incoming := claudeCodeCapturedRequest(stream)
		exec := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
		req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: incoming, Stream: stream}

		if stream {
			result, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
			}
		} else if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}

		if string(seenBody) != string(payload) {
			t.Fatalf("stream=%v: body changed\n got %s\nwant %s", stream, seenBody, payload)
		}
		for name, values := range incoming {
			switch name {
			case "Authorization", "X-Forwarded-For", "Anthropic-Beta":
				continue
			}
			if got := helps.HeaderValueCaseInsensitive(seenHeaders, name); got != strings.Join(values, ",") {
				t.Errorf("stream=%v: %s = %q, want %q", stream, name, got, strings.Join(values, ","))
			}
		}
		if got := helps.HeaderValueCaseInsensitive(seenHeaders, "Authorization"); got != "Bearer sk-ant-oat01-linked" {
			t.Errorf("stream=%v: Authorization carries the wrong credential", stream)
		}
		if got := helps.HeaderValueCaseInsensitive(seenHeaders, "Anthropic-Beta"); got != incoming.Get("Anthropic-Beta")+","+claudeOAuthBeta {
			t.Errorf("stream=%v: Anthropic-Beta = %q", stream, got)
		}
		for _, name := range []string{"X-Forwarded-For", "X-Client-Request-Id", "X-Stainless-Timeout", "X-Api-Key"} {
			if got := helps.HeaderValueCaseInsensitive(seenHeaders, name); got != "" {
				t.Errorf("stream=%v: %s = %q, want absent", stream, name, got)
			}
		}
	}
}

func TestCodexRoutingHintNotAddedWhenCloakingDisabled(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "at"}}
	body := []byte(`{"model":"gpt-6.1-sol"}`)
	cfg := &config.Config{}
	cfg.Codex.DisableCodexCloaking = true

	headers := http.Header{}
	applyCodexRoutingHint(context.Background(), cfg, headers, auth, "gpt-6.1-sol", body, http.Header{})
	if got := headers.Get(codexRoutingHintHeader); got != "" {
		t.Fatalf("routing hint %q added to a request that carried none", got)
	}

	headers = http.Header{}
	applyCodexRoutingHint(context.Background(), &config.Config{}, headers, auth, "gpt-6.1-sol", body, http.Header{})
	if got := headers.Get(codexRoutingHintHeader); got != "model=gpt-6.1-sol" {
		t.Fatalf("cloaking on: routing hint %q, want upstream behaviour", got)
	}
}
