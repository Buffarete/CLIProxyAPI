package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func withUA(ua string) context.Context {
	return logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{UserAgent: ua})
}

func managerWithClaude(t *testing.T, authKind string) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "claude-" + authKind, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"auth_kind": authKind}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m
}

func TestClaudeCodeOnly(t *testing.T) {
	cases := []struct {
		name      string
		ua        string
		providers []string
		want      []string
		blocked   bool
	}{
		{"claude code keeps claude", "claude-cli/2.3.1 (external, cli)", []string{"claude"}, []string{"claude"}, false},
		{"agent sdk keeps claude", "claude-cli/2.3.1 (external, sdk-ts)", []string{"claude"}, []string{"claude"}, false},
		{"opencode is refused claude", "opencode/2.0.18", []string{"claude"}, nil, true},
		{"no user agent is refused claude", "", []string{"claude"}, nil, true},
		{"curl is refused claude", "curl/8.9.1", []string{"claude"}, nil, true},
		{"codex keeps codex", "codex_cli_rs/0.160.0", []string{"codex"}, []string{"codex"}, false},
		{"mixed drops only claude", "pi/1.2", []string{"claude", "codex"}, []string{"codex"}, false},
	}
	m := managerWithClaude(t, "oauth")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.claudeCodeOnly(withUA(tc.ua), tc.providers)
			if tc.blocked {
				var authErr *Error
				if !errors.As(err, &authErr) || authErr.Code != ErrorCodeClaudeCodeOnly || authErr.HTTPStatus != http.StatusForbidden {
					t.Fatalf("want a %s 403 error, got %v", ErrorCodeClaudeCodeOnly, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestClaudeCodeOnlyLeavesClaudeAPIKeysAlone(t *testing.T) {
	m := managerWithClaude(t, "apikey")
	got, err := m.claudeCodeOnly(withUA("opencode/2.0.18"), []string{"claude"})
	if err != nil || !reflect.DeepEqual(got, []string{"claude"}) {
		t.Fatalf("got %v, %v; want [claude]", got, err)
	}
}

func TestExecuteRefusesClaudeSubscriptionToOtherClients(t *testing.T) {
	m := managerWithClaude(t, "oauth")
	req := cliproxyexecutor.Request{Model: "claude-opus-5-5"}
	_, err := m.Execute(withUA("opencode/2.0.18"), []string{"claude"}, req, cliproxyexecutor.Options{})
	var authErr *Error
	if !errors.As(err, &authErr) || authErr.Code != ErrorCodeClaudeCodeOnly {
		t.Fatalf("Execute: want %s, got %v", ErrorCodeClaudeCodeOnly, err)
	}
	if _, err := m.ExecuteStream(withUA("curl/8.9.1"), []string{"claude"}, req, cliproxyexecutor.Options{}); !errors.As(err, &authErr) || authErr.Code != ErrorCodeClaudeCodeOnly {
		t.Fatalf("ExecuteStream: want %s, got %v", ErrorCodeClaudeCodeOnly, err)
	}
}

// claudeCodeUA is the User-Agent Claude Code sends; tests of Claude subscription behavior use it so
// the claude_code_only gate lets their requests through.
const claudeCodeUA = "claude-cli/2.3.1 (external, cli)"
