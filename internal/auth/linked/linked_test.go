package linked

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// tokenServer answers token refreshes with reply and records each request body.
func tokenServer(t *testing.T, status int, reply string, target *string) (*[]map[string]any, *atomic.Int32) {
	t.Helper()
	var bodies []map[string]any
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("refresh body is not JSON: %v", err)
		}
		bodies = append(bodies, body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	old := *target
	*target = server.URL
	t.Cleanup(func() { *target = old })
	return &bodies, &calls
}

func claudeCredentials(access string, expires time.Time) string {
	return `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"rt-old","expiresAt":` +
		jsonNumber(expires.UnixMilli()) + `,"refreshTokenExpiresAt":1,"scopes":["user:inference","user:profile"],` +
		`"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"},"mcpOAuth":{"srv|abc":{"accessToken":"mcp-secret"}}}`
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func claudeDir(t *testing.T, access string, expires time.Time) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".claude_2")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, claudeCredentialsPath(dir), claudeCredentials(access, expires))
	return dir
}

func TestClaudeRefreshKeepsFileShapeAndLocks(t *testing.T) {
	dir := claudeDir(t, "at-old", now.Add(time.Hour))
	bodies, _ := tokenServer(t, 200, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":28800,"refresh_token_expires_in":2592000}`, &claudeTokenURL)
	metadata := map[string]any{"type": "claude", MetadataKey: dir}

	if err := Refresh(context.Background(), nil, metadata, true, now); err != nil {
		t.Fatal(err)
	}

	want := `{"claudeAiOauth":{"accessToken":"at-new","refreshToken":"rt-new","expiresAt":` + jsonNumber(now.Add(8*time.Hour).UnixMilli()) +
		`,"refreshTokenExpiresAt":` + jsonNumber(now.Add(30*24*time.Hour).UnixMilli()) + `,"scopes":["user:inference","user:profile"],` +
		`"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"},"mcpOAuth":{"srv|abc":{"accessToken":"mcp-secret"}}}`
	if got := readFile(t, claudeCredentialsPath(dir)); got != want {
		t.Fatalf("credentials file\n got %s\nwant %s", got, want)
	}
	info, _ := os.Stat(claudeCredentialsPath(dir))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries after refresh, want only .credentials.json", len(entries))
	}
	if _, err := os.Stat(dir + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("legacy lock left behind: %v", err)
	}
	body := (*bodies)[0]
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt-old" || body["client_id"] != claudeClientID || body["scope"] != "user:inference user:profile" {
		t.Fatalf("refresh body %v", body)
	}
	if metadata["access_token"] != "at-new" || metadata["refresh_token"] != "rt-new" {
		t.Fatalf("metadata not synced: %v", metadata)
	}
}

func TestClaudeRefreshAdoptsTokenAnotherProcessRefreshed(t *testing.T) {
	dir := claudeDir(t, "at-from-claude-code", now.Add(8*time.Hour))
	_, calls := tokenServer(t, 500, `{}`, &claudeTokenURL)
	metadata := map[string]any{"type": "claude", MetadataKey: dir, "access_token": "at-rejected"}

	if err := Refresh(context.Background(), nil, metadata, false, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("refreshed %d times, want 0", calls.Load())
	}
	if metadata["access_token"] != "at-from-claude-code" {
		t.Fatalf("access token %v", metadata["access_token"])
	}
}

func TestClaudeRefreshSpendsTokenThatWasRejected(t *testing.T) {
	dir := claudeDir(t, "at-rejected", now.Add(8*time.Hour))
	_, calls := tokenServer(t, 200, `{"access_token":"at-new","expires_in":28800}`, &claudeTokenURL)
	metadata := map[string]any{"type": "claude", MetadataKey: dir, "access_token": "at-rejected"}

	if err := Refresh(context.Background(), nil, metadata, false, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || metadata["access_token"] != "at-new" || metadata["refresh_token"] != "rt-old" {
		t.Fatalf("calls %d, metadata %v", calls.Load(), metadata)
	}
}

func TestClaudeRefreshWaitsForHeldLockAndBreaksStaleOne(t *testing.T) {
	old := lockRetryDelay
	lockRetryDelay = time.Millisecond
	t.Cleanup(func() { lockRetryDelay = old })
	_, _ = tokenServer(t, 200, `{"access_token":"at-new","expires_in":28800}`, &claudeTokenURL)

	held := claudeDir(t, "at-old", now)
	if err := os.Mkdir(filepath.Join(held, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := Refresh(context.Background(), nil, map[string]any{"type": "claude", MetadataKey: held}, true, now)
	if err == nil || !strings.Contains(err.Error(), "another process is refreshing") {
		t.Fatalf("held lock: err %v", err)
	}
	if strings.Contains(readFile(t, claudeCredentialsPath(held)), "at-new") {
		t.Fatal("refreshed while Claude Code held the lock")
	}

	stale := claudeDir(t, "at-old", now)
	lock := stale + ".lock"
	if err = os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * lockStale)
	if err = os.Chtimes(lock, past, past); err != nil {
		t.Fatal(err)
	}
	if err = Refresh(context.Background(), nil, map[string]any{"type": "claude", MetadataKey: stale}, true, now); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	if _, errStat := os.Stat(lock); !os.IsNotExist(errStat) {
		t.Fatal("stale lock not removed")
	}
}

func TestClaudeRefreshFailureKeepsFile(t *testing.T) {
	dir := claudeDir(t, "at-old", now)
	before := readFile(t, claudeCredentialsPath(dir))
	_, _ = tokenServer(t, 400, `{"error":"invalid_grant","error_description":"Refresh token rt-old is invalid"}`, &claudeTokenURL)

	err := Refresh(context.Background(), nil, map[string]any{"type": "claude", MetadataKey: dir}, true, now)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "rt-old") {
		t.Fatalf("err %v", err)
	}
	if readFile(t, claudeCredentialsPath(dir)) != before {
		t.Fatal("file changed after a refused refresh")
	}
}

func TestPrepareRefreshesOnlyNearExpiry(t *testing.T) {
	_, calls := tokenServer(t, 200, `{"access_token":"at-new","expires_in":28800}`, &claudeTokenURL)

	fresh := claudeDir(t, "at-fresh", now.Add(time.Hour))
	metadata := map[string]any{"type": "claude", MetadataKey: fresh}
	if !NeedsSync(metadata, now) {
		t.Fatal("empty metadata does not need a sync")
	}
	if err := Prepare(context.Background(), nil, metadata, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || metadata["access_token"] != "at-fresh" || metadata["expired"] != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("calls %d, metadata %v", calls.Load(), metadata)
	}
	if NeedsSync(metadata, now) {
		t.Fatal("synced metadata still needs a sync")
	}

	expiring := claudeDir(t, "at-expiring", now.Add(time.Minute))
	metadata = map[string]any{"type": "claude", MetadataKey: expiring}
	if err := Prepare(context.Background(), nil, metadata, now); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || metadata["access_token"] != "at-new" {
		t.Fatalf("calls %d, metadata %v", calls.Load(), metadata)
	}
}

func jwt(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func codexHome(t *testing.T, access string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".codex")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idToken := jwt(map[string]any{"email": "will@example.com", "https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "plus", "chatgpt_account_id": "acct-1"}})
	text := "{\n  \"auth_mode\": \"chatgpt\",\n  \"OPENAI_API_KEY\": null,\n  \"tokens\": {\n    \"id_token\": \"" + idToken +
		"\",\n    \"access_token\": \"" + access + "\",\n    \"refresh_token\": \"rt-old\",\n    \"account_id\": \"acct-1\"\n  },\n" +
		"  \"last_refresh\": \"2026-10-10T00:00:00.000Z\"\n}"
	writeFile(t, codexAuthPath(dir), text)
	return dir, idToken
}

func TestCodexRefreshKeepsPrettyPrintedShape(t *testing.T) {
	dir, idToken := codexHome(t, jwt(map[string]any{"exp": now.Add(time.Hour).Unix()}))
	newAccess := jwt(map[string]any{"exp": now.Add(240 * time.Hour).Unix()})
	bodies, _ := tokenServer(t, 200, `{"access_token":"`+newAccess+`","refresh_token":"rt-new"}`, &codexTokenURL)
	metadata := map[string]any{"type": "codex", MetadataKey: dir}

	if err := Refresh(context.Background(), nil, metadata, true, now); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"auth_mode\": \"chatgpt\",\n  \"OPENAI_API_KEY\": null,\n  \"tokens\": {\n    \"id_token\": \"" + idToken +
		"\",\n    \"access_token\": \"" + newAccess + "\",\n    \"refresh_token\": \"rt-new\",\n    \"account_id\": \"acct-1\"\n  },\n" +
		"  \"last_refresh\": \"2026-10-19T12:00:00.000Z\"\n}"
	if got := readFile(t, codexAuthPath(dir)); got != want {
		t.Fatalf("auth.json\n got %s\nwant %s", got, want)
	}
	body := (*bodies)[0]
	if body["client_id"] != codexClientID || body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt-old" || len(body) != 3 {
		t.Fatalf("refresh body %v", body)
	}
	if metadata["access_token"] != newAccess || metadata["account_id"] != "acct-1" || metadata["expired"] != now.Add(240*time.Hour).Format(time.RFC3339) {
		t.Fatalf("metadata %v", metadata)
	}
}

func TestCodexRefusedRefreshAdoptsTokenCodexWrote(t *testing.T) {
	stale := jwt(map[string]any{"exp": now.Add(time.Minute).Unix()})
	dir, _ := codexHome(t, stale)
	codexWrote := jwt(map[string]any{"exp": now.Add(240 * time.Hour).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Codex spends the refresh token while the proxy's request is in flight.
		text := readFile(t, codexAuthPath(dir))
		writeFile(t, codexAuthPath(dir), strings.Replace(text, stale, codexWrote, 1))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"refresh_token_reused"}}`)
	}))
	t.Cleanup(server.Close)
	old := codexTokenURL
	codexTokenURL = server.URL
	t.Cleanup(func() { codexTokenURL = old })

	metadata := map[string]any{"type": "codex", MetadataKey: dir, "access_token": stale}
	if err := Refresh(context.Background(), nil, metadata, false, now); err != nil {
		t.Fatal(err)
	}
	if metadata["access_token"] != codexWrote {
		t.Fatal("did not adopt the token Codex wrote")
	}
}

func TestLinkFilesHoldNoTokens(t *testing.T) {
	dir := claudeDir(t, "at-secret", now)
	writeFile(t, filepath.Join(dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@example.com","accountUuid":"acc-uuid","organizationUuid":"org-uuid","organizationName":"Org"}}`)
	metadata, err := ClaudeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "claude", MetadataKey: dir, "label": "Claude 2", "plan_type": "max", "email": "a@example.com",
		"account_uuid": "acc-uuid", "organization_uuid": "org-uuid", "organization_name": "Org"}
	if got, _ := json.Marshal(metadata); string(got) != mustJSON(want) {
		t.Fatalf("claude link %s", got)
	}

	home, _ := codexHome(t, "at-secret")
	metadata, err = CodexHome(home)
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"type": "codex", MetadataKey: home, "label": "Codex", "email": "will@example.com", "plan_type": "plus", "account_id": "acct-1"}
	if got, _ := json.Marshal(metadata); string(got) != mustJSON(want) {
		t.Fatalf("codex link %s", got)
	}

	authDir := t.TempDir()
	if err = Sync(metadata); err != nil {
		t.Fatal(err)
	}
	path, err := WriteAuthFile(authDir, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "linked-codex.json" {
		t.Fatalf("file name %s", path)
	}
	text := readFile(t, path)
	for _, secret := range []string{"at-secret", "rt-old", "eyJ"} {
		if strings.Contains(text, secret) {
			t.Fatalf("auth file holds a token: %s", text)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
