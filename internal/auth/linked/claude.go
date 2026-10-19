package linked

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Claude Code's own OAuth client and token endpoint (Claude Code 2.1.303 binary).
var (
	claudeTokenURL = "https://platform.claude.com/v1/oauth/token"
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// Claude Code locks with proper-lockfile: a directory, stale after 60 s.
const (
	lockStale    = 60 * time.Second
	lockAttempts = 10
)

var lockRetryDelay = time.Second

type claudeProvider struct{}

func claudeCredentialsPath(dir string) string { return filepath.Join(dir, ".credentials.json") }

func (claudeProvider) read(dir string) (token, error) {
	raw, err := os.ReadFile(claudeCredentialsPath(dir))
	if err != nil {
		return token{}, fmt.Errorf("linked credentials: %w", err)
	}
	oauth := gjson.GetBytes(raw, "claudeAiOauth")
	if !oauth.Exists() || oauth.Get("refreshToken").String() == "" {
		return token{}, errNoToken
	}
	t := token{access: oauth.Get("accessToken").String(), refresh: oauth.Get("refreshToken").String()}
	if ms := oauth.Get("expiresAt").Int(); ms > 0 {
		t.expires = time.UnixMilli(ms)
	}
	return t, nil
}

type claudeRefreshResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
}

// refresh takes the same two locks as Claude Code, in the same order, so the
// proxy and a running Claude Code never spend one refresh token twice.
func (claudeProvider) refresh(ctx context.Context, client *http.Client, dir string, needs func(token) bool, now time.Time) error {
	release, err := lockClaudeDir(ctx, dir)
	if err != nil {
		return err
	}
	defer release()

	file := claudeCredentialsPath(dir)
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("linked credentials: %w", err)
	}
	current, err := claudeProvider{}.read(dir)
	if err != nil {
		return err
	}
	if !needs(current) {
		return nil
	}

	oauth := gjson.GetBytes(raw, "claudeAiOauth")
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": current.refresh,
		"client_id":     claudeClientID,
	}
	if id := oauth.Get("clientId").String(); id != "" {
		body["client_id"] = id
	}
	var scopes []string
	for _, s := range oauth.Get("scopes").Array() {
		scopes = append(scopes, s.String())
	}
	if len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	var resp claudeRefreshResponse
	if err = postJSON(ctx, client, claudeTokenURL, body, &resp); err != nil {
		return err
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("linked credentials: token refresh returned no access token")
	}

	set := []jsonEdit{
		{"claudeAiOauth.accessToken", resp.AccessToken},
		{"claudeAiOauth.expiresAt", now.Add(time.Duration(resp.ExpiresIn) * time.Second).UnixMilli()},
	}
	if resp.RefreshToken != "" {
		set = append(set, jsonEdit{"claudeAiOauth.refreshToken", resp.RefreshToken})
	}
	if resp.RefreshTokenExpiresIn > 0 {
		set = append(set, jsonEdit{"claudeAiOauth.refreshTokenExpiresAt", now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second).UnixMilli()})
	}
	if fields := strings.Fields(resp.Scope); len(fields) > 0 {
		set = append(set, jsonEdit{"claudeAiOauth.scopes", fields})
	}
	return writeEdited(file, raw, set)
}

// lockClaudeDir takes <dir>/.oauth_refresh.lock, then the legacy <realpath(dir)>.lock.
func lockClaudeDir(ctx context.Context, dir string) (func(), error) {
	releaseNew, err := lockDir(ctx, filepath.Join(dir, ".oauth_refresh.lock"))
	if err != nil {
		return nil, err
	}
	real, errReal := filepath.EvalSymlinks(dir)
	if errReal != nil {
		real = dir
	}
	releaseLegacy, err := lockDir(ctx, filepath.Clean(real)+".lock")
	if err != nil {
		releaseNew()
		return nil, err
	}
	return func() {
		releaseLegacy()
		releaseNew()
	}, nil
}

func lockDir(ctx context.Context, path string) (func(), error) {
	for attempt := 1; ; attempt++ {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.RemoveAll(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("linked credentials: lock: %w", err)
		}
		if info, errStat := os.Stat(path); errStat == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.RemoveAll(path)
			continue
		}
		if attempt >= lockAttempts {
			return nil, fmt.Errorf("linked credentials: another process is refreshing %s", filepath.Dir(path))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockRetryDelay):
		}
	}
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("linked credentials: token refresh request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("linked credentials: read token refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		code := gjson.GetBytes(data, "error").String()
		if strings.ContainsAny(code, "{[") || len(code) > 64 {
			code = gjson.GetBytes(data, "error.type").String()
		}
		return errRefreshStatus{status: resp.StatusCode, code: code}
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("linked credentials: parse token refresh response: %w", err)
	}
	return nil
}
