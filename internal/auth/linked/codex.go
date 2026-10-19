package linked

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Codex's own OAuth client and token endpoint (openai/codex rust-v0.163.0,
// codex-rs/login/src/auth/manager.rs).
var (
	codexTokenURL = "https://auth.openai.com/oauth/token"
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

type codexProvider struct{}

func codexAuthPath(dir string) string { return filepath.Join(dir, "auth.json") }

func (codexProvider) read(dir string) (token, error) {
	raw, err := os.ReadFile(codexAuthPath(dir))
	if err != nil {
		return token{}, fmt.Errorf("linked credentials: %w", err)
	}
	tokens := gjson.GetBytes(raw, "tokens")
	if !tokens.Exists() || tokens.Get("refresh_token").String() == "" {
		return token{}, errNoToken
	}
	t := token{
		access:      tokens.Get("access_token").String(),
		refresh:     tokens.Get("refresh_token").String(),
		idToken:     tokens.Get("id_token").String(),
		accountID:   tokens.Get("account_id").String(),
		lastRefresh: gjson.GetBytes(raw, "last_refresh").String(),
	}
	if exp := jwtClaims(t.access).Get("exp").Int(); exp > 0 {
		t.expires = time.Unix(exp, 0)
	}
	return t, nil
}

type codexRefreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// refresh follows Codex, which takes no lock another process can see: it
// re-reads auth.json first and treats a changed token as refreshed, and after a
// refused refresh it reads once more in case Codex spent the refresh token first.
func (codexProvider) refresh(ctx context.Context, client *http.Client, dir string, needs func(token) bool, now time.Time) error {
	file := codexAuthPath(dir)
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("linked credentials: %w", err)
	}
	current, err := codexProvider{}.read(dir)
	if err != nil {
		return err
	}
	if !needs(current) {
		return nil
	}
	var resp codexRefreshResponse
	errPost := postJSON(ctx, client, codexTokenURL, map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": current.refresh,
	}, &resp)
	if errPost != nil {
		if reread, errRead := (codexProvider{}).read(dir); errRead == nil && reread.access != current.access && !reread.nearExpiry(now) {
			return nil
		}
		return errPost
	}
	if resp.AccessToken == "" {
		return fmt.Errorf("linked credentials: token refresh returned no access token")
	}
	edits := []jsonEdit{
		{"tokens.access_token", resp.AccessToken},
		{"last_refresh", now.UTC().Format("2006-01-02T15:04:05.000Z")},
	}
	if resp.IDToken != "" {
		edits = append(edits, jsonEdit{"tokens.id_token", resp.IDToken})
	}
	if resp.RefreshToken != "" {
		edits = append(edits, jsonEdit{"tokens.refresh_token", resp.RefreshToken})
	}
	return writeEdited(file, raw, edits)
}

// jwtClaims decodes a JWT's claims without verifying it: the proxy only reads
// the expiry and account fields of tokens it already holds.
func jwtClaims(jwt string) gjson.Result {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return gjson.Result{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return gjson.Result{}
	}
	return gjson.ParseBytes(payload)
}
