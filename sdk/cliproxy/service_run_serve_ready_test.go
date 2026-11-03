package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// A client that connects the moment the port opens must not be told that a
// model from a stored auth is unknown, even while the file watcher is still
// doing its first full load.
func TestServiceRun_ServesStoredAuthModelsFromFirstRequest(t *testing.T) {
	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if errClose := ln.Close(); errClose != nil {
		t.Fatalf("close listener: %v", errClose)
	}

	authDir := t.TempDir()
	fileName := "codex-serve-ready.json"
	teamIDToken := "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ICJ1c2VyQGV4YW1wbGUuY29tIiwgImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6IHsiY2hhdGdwdF9wbGFuX3R5cGUiOiAidGVhbSIsICJjaGF0Z3B0X2FjY291bnRfaWQiOiAiYWNjLTEyMyJ9fQ.sig"
	authJSON := fmt.Sprintf(`{"type":"codex","email":"user@example.com","access_token":"token-initial","id_token":%q}`, teamIDToken)
	if errWrite := os.WriteFile(filepath.Join(authDir, fileName), []byte(authJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	cfgPath := filepath.Join(authDir, "config.yaml")
	if errWrite := os.WriteFile(cfgPath, []byte("{}"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeys: []string{"client-key"},
			// Any upstream call fails locally instead of reaching the network.
			ProxyURL: "http://127.0.0.1:1",
		},
		Host:    "127.0.0.1",
		Port:    port,
		AuthDir: authDir,
	}

	reg := internalregistry.GetGlobalRegistry()
	reg.UnregisterClient(fileName)
	t.Cleanup(func() { reg.UnregisterClient(fileName) })

	releaseWatcher := make(chan struct{})
	watcherFactory := func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
		return &WatcherWrapper{
			start: func(ctx context.Context) error {
				select {
				case <-releaseWatcher:
				case <-ctx.Done():
				}
				return nil
			},
			stop:                func() error { return nil },
			setConfig:           func(*config.Config) {},
			setPluginAuthParser: func(PluginAuthParser) {},
		}, nil
	}

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithWatcherFactory(watcherFactory).
		Build()
	if errBuild != nil {
		t.Fatalf("Build: %v", errBuild)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- service.Run(runCtx) }()
	t.Cleanup(func() {
		close(releaseWatcher)
		cancel()
		select {
		case errRun := <-runErr:
			if errRun != nil && !errors.Is(errRun, context.Canceled) {
				t.Errorf("Run: %v", errRun)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Run did not exit")
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 10 * time.Second}
	post := func(model string) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"input":"hi","stream":false}`, model)
		req, errReq := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(body))
		if errReq != nil {
			t.Fatalf("new request: %v", errReq)
		}
		req.Header.Set("Authorization", "Bearer client-key")
		req.Header.Set("Content-Type", "application/json")
		resp, errDo := client.Do(req)
		if errDo != nil {
			t.Fatalf("POST %s: %v", model, errDo)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, errDial := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if errDial == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never accepted connections: %v", errDial)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if status, body := post("gpt-6.1-sol"); status == http.StatusBadRequest || strings.Contains(body, "model_not_found") {
		t.Fatalf("first request for a stored auth's model got %d %s; want it routed", status, body)
	}
	if status, body := post("no-such-model-xyz"); status != http.StatusBadRequest || !strings.Contains(body, "model_not_found") {
		t.Fatalf("unknown model got %d %s; want 400 model_not_found", status, body)
	}
}
