package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// upstreamHeadersWithRateLimits is an Anthropic or ChatGPT response's header
// set with both providers' rate-limit headers and the headers that must never
// reach the client.
func upstreamHeadersWithRateLimits(contentType string) http.Header {
	return http.Header{
		"Content-Type": {contentType},
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1760900000"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.17"},
		"Anthropic-Ratelimit-Unified-Status":         {"allowed"},
		"Anthropic-Ratelimit-Tokens-Remaining":       {"9000"},
		"Retry-After":                                {"30"},
		"X-Codex-Primary-Used-Percent":               {"12"},
		"X-Codex-Secondary-Reset-At":                 {"1761000000"},
		"X-Codex-Plan-Type":                          {"pro"},
		"X-Codex-Credits-Balance":                    {"0"},
		"X-Codex-Turn-State":                         {"opaque"},
		"Set-Cookie":                                 {"__cf_bm=secret; path=/"},
		"Connection":                                 {"keep-alive"},
		"Transfer-Encoding":                          {"chunked"},
		"Request-Id":                                 {"req_1"},
		"Anthropic-Organization-Id":                  {"org"},
		"Cf-Ray":                                     {"ray"},
	}
}

func ginContextWithRecorder(staleHeader string) (context.Context, *gin.Context) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// A stale value from an earlier attempt must not survive.
	ginCtx.Writer.Header().Set(staleHeader, "stale")
	return context.WithValue(context.Background(), "gin", ginCtx), ginCtx
}

func assertClientHeaders(t *testing.T, label string, got http.Header, want []string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, name := range want {
		wantSet[name] = true
	}
	for name := range got {
		if !wantSet[name] {
			t.Errorf("%s: client got %s = %q, want absent", label, name, got.Values(name))
		}
	}
	upstream := upstreamHeadersWithRateLimits("")
	for _, name := range want {
		if g, w := strings.Join(got.Values(name), ","), strings.Join(upstream.Values(name), ","); g != w {
			t.Errorf("%s: client %s = %q, want %q", label, name, g, w)
		}
	}
}

var claudeClientRateLimitHeaders = []string{
	"Anthropic-Ratelimit-Unified-5h-Utilization", "Anthropic-Ratelimit-Unified-5h-Reset",
	"Anthropic-Ratelimit-Unified-7d-Utilization", "Anthropic-Ratelimit-Unified-Status",
	"Anthropic-Ratelimit-Tokens-Remaining", "Retry-After",
}

var codexClientRateLimitHeaders = []string{
	"X-Codex-Primary-Used-Percent", "X-Codex-Secondary-Reset-At", "X-Codex-Plan-Type",
	"X-Codex-Credits-Balance", "Retry-After",
}

func TestClaudePassthroughForwardsRateLimitHeadersToClient(t *testing.T) {
	cases := []struct {
		name        string
		passthrough bool
		stream      bool
		status      int
	}{
		{"execute", true, false, http.StatusOK},
		{"stream", true, true, http.StatusOK},
		{"execute 429", true, false, http.StatusTooManyRequests},
		{"stream 429", true, true, http.StatusTooManyRequests},
		{"cloaking on", false, false, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body := `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":1,"output_tokens":1}}`
				contentType := "application/json"
				if tc.status != http.StatusOK {
					body = `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`
				} else if tc.stream {
					contentType = "text/event-stream"
					body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"model\":\"claude-opus-5-5\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: tc.status, Header: upstreamHeadersWithRateLimits(contentType), Request: req, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, ginCtx := ginContextWithRecorder("Anthropic-Ratelimit-Unified-7d-Reset")
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat01-linked"}, Metadata: claudeOAuthTestMetadata()}
			payload, incoming := claudeCodeCapturedRequest(tc.stream)
			exec := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: tc.passthrough})
			req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: incoming, Stream: tc.stream}

			var err error
			if tc.stream {
				var result *cliproxyexecutor.StreamResult
				if result, err = exec.ExecuteStream(ctx, auth, req, opts); err == nil {
					for range result.Chunks {
					}
				}
			} else {
				_, err = exec.Execute(ctx, auth, req, opts)
			}
			if (err != nil) != (tc.status != http.StatusOK) {
				t.Fatalf("err = %v, upstream status %d", err, tc.status)
			}

			if !tc.passthrough {
				if got := ginCtx.Writer.Header().Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "" {
					t.Fatalf("cloaking on: rate-limit header forwarded (%q); upstream behaviour is to forward none", got)
				}
				return
			}
			assertClientHeaders(t, tc.name, ginCtx.Writer.Header(), claudeClientRateLimitHeaders)
		})
	}
}

func TestCodexPassthroughForwardsRateLimitHeadersToClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		status int
	}{
		{"execute", false, http.StatusOK},
		{"stream", true, http.StatusOK},
		{"stream 429", true, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for name, values := range upstreamHeadersWithRateLimits("text/event-stream") {
					if name != "Connection" && name != "Transfer-Encoding" {
						w.Header()[name] = values
					}
				}
				if tc.status != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"limited"}}`)
					return
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.5\",\"output\":[]}}\n\n")
			}))
			t.Cleanup(server.Close)

			ctx, ginCtx := ginContextWithRecorder("X-Codex-Primary-Reset-At")
			cfg := &config.Config{}
			cfg.Codex.DisableCodexCloaking = true
			exec := NewCodexExecutor(cfg)
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: tc.stream}

			var err error
			if tc.stream {
				var result *cliproxyexecutor.StreamResult
				if result, err = exec.ExecuteStream(ctx, codexOAuthTestAuth(server.URL), req, opts); err == nil {
					for range result.Chunks {
					}
				}
			} else {
				_, err = exec.Execute(ctx, codexOAuthTestAuth(server.URL), req, opts)
			}
			if (err != nil) != (tc.status != http.StatusOK) {
				t.Fatalf("err = %v, upstream status %d", err, tc.status)
			}
			assertClientHeaders(t, tc.name, ginCtx.Writer.Header(), codexClientRateLimitHeaders)
		})
	}
}
