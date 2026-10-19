package executor

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestClaudeExecutorPreparesLinkedAuthFromItsDir(t *testing.T) {
	dir := t.TempDir()
	write := func(access string) {
		expires := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
		data := `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"rt","expiresAt":` + expires + `}}`
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sk-ant-oat01-first")
	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "linked-claude.json", Provider: "claude", Metadata: map[string]any{
		"type": "claude", "linked_dir": dir, "account_uuid": "acc", "device_ids": []any{},
	}}

	if !exec.ShouldPrepareRequestAuth(auth) {
		t.Fatal("unsynced linked auth does not ask to be prepared")
	}
	prepared, err := exec.PrepareRequestAuth(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := claudeCreds(prepared); key != "sk-ant-oat01-first" {
		t.Fatalf("credential %q", key)
	}

	// Claude Code refreshed the dir behind the proxy's back.
	write("sk-ant-oat01-second")
	if !exec.ShouldPrepareRequestAuth(prepared) {
		t.Fatal("a token changed in the dir does not trigger a sync")
	}
	prepared, err = exec.PrepareRequestAuth(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := claudeCreds(prepared); key != "sk-ant-oat01-second" {
		t.Fatalf("credential %q", key)
	}
}

func TestCodexExecutorPreparesOnlyLinkedAuth(t *testing.T) {
	dir := t.TempDir()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`))
	access := "h." + claims + ".s"
	data := `{"tokens":{"id_token":"","access_token":"` + access + `","refresh_token":"rt","account_id":"acct-9"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := NewCodexExecutor(&config.Config{})
	if exec.ShouldPrepareRequestAuth(&cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "x"}}) {
		t.Fatal("ordinary codex auth asks to be prepared")
	}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "linked_dir": dir}}
	if !exec.ShouldPrepareRequestAuth(auth) {
		t.Fatal("unsynced linked auth does not ask to be prepared")
	}
	prepared, err := exec.PrepareRequestAuth(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := codexCreds(prepared); key != access || prepared.Metadata["account_id"] != "acct-9" {
		t.Fatalf("credential %q, metadata %v", key, prepared.Metadata)
	}
	if exec.ShouldPrepareRequestAuth(prepared) {
		t.Fatal("synced linked auth asks to be prepared again")
	}
}
