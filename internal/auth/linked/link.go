package linked

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var dirNumber = regexp.MustCompile(`_(\d+)$`)

// label matches Buff Cloud's AI page: "Claude 1" for ~/.claude, "Claude N" for
// ~/.claude_N, "Codex" for ~/.codex and "Codex N" for ~/.codex_N.
func label(tool, dir string) string {
	base := filepath.Base(dir)
	if base == "."+strings.ToLower(tool) {
		if tool == "Claude" {
			return "Claude 1"
		}
		return tool
	}
	if m := dirNumber.FindStringSubmatch(base); m != nil {
		return tool + " " + m[1]
	}
	return tool + " (" + base + ")"
}

// FileName is the auth file name for a linked dir: linked-claude_2.json for ~/.claude_2.
func FileName(dir string) string {
	return "linked-" + strings.TrimPrefix(filepath.Base(dir), ".") + ".json"
}

// ClaudeDir builds the auth file metadata for a Claude Code config dir: the link,
// a label, the plan, and the account's email and ids from .claude.json when present.
func ClaudeDir(dir string) (map[string]any, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(claudeCredentialsPath(dir))
	if err != nil {
		return nil, fmt.Errorf("linked credentials: %w", err)
	}
	if _, err = (claudeProvider{}).read(dir); err != nil {
		return nil, err
	}
	metadata := map[string]any{"type": "claude", MetadataKey: dir, "label": label("Claude", dir)}
	setString(metadata, "plan_type", gjson.GetBytes(raw, "claudeAiOauth.subscriptionType").String())
	account := claudeAccount(dir)
	setString(metadata, "email", account.Get("emailAddress").String())
	setString(metadata, "account_uuid", account.Get("accountUuid").String())
	setString(metadata, "organization_uuid", account.Get("organizationUuid").String())
	setString(metadata, "organization_name", account.Get("organizationName").String())
	return metadata, nil
}

// claudeAccount reads oauthAccount from <dir>/.claude.json. Claude Code keeps
// that file in $HOME instead when it runs without CLAUDE_CONFIG_DIR, so the
// default ~/.claude dir falls back to ~/.claude.json.
func claudeAccount(dir string) gjson.Result {
	candidates := []string{filepath.Join(dir, ".claude.json")}
	if home, err := os.UserHomeDir(); err == nil && dir == filepath.Join(home, ".claude") {
		candidates = append(candidates, filepath.Join(home, ".claude.json"))
	}
	for _, file := range candidates {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if account := gjson.GetBytes(raw, "oauthAccount"); account.Get("emailAddress").String() != "" {
			return account
		}
	}
	return gjson.Result{}
}

// CodexHome builds the auth file metadata for a Codex home: the link, a label,
// and the email, plan and ChatGPT account id from its id_token.
func CodexHome(dir string) (map[string]any, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	t, err := codexProvider{}.read(dir)
	if err != nil {
		return nil, err
	}
	claims := jwtClaims(t.idToken)
	metadata := map[string]any{"type": "codex", MetadataKey: dir, "label": label("Codex", dir)}
	setString(metadata, "email", claims.Get("email").String())
	setString(metadata, "plan_type", claims.Get(`https://api\.openai\.com/auth.chatgpt_plan_type`).String())
	accountID := t.accountID
	if accountID == "" {
		accountID = claims.Get(`https://api\.openai\.com/auth.chatgpt_account_id`).String()
	}
	setString(metadata, "account_id", accountID)
	return metadata, nil
}

// WriteAuthFile writes metadata to authDir/FileName(dir), mode 600, replacing
// an earlier link to the same dir.
func WriteAuthFile(authDir string, metadata map[string]any) (string, error) {
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(StoreFields(metadata), "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(authDir, FileName(Dir(metadata)))
	return path, writeAtomic(path, data)
}

func setString(metadata map[string]any, key, value string) {
	if value != "" {
		metadata[key] = value
	}
}

// ClaudePlan names a Claude Code config dir's plan, for spotting a change:
// the organization type and rate-limit tier from .claude.json and the
// subscription type and tier beside the token in .credentials.json. It reads no
// token. Claude Code rewrites both files when it refreshes the profile or the
// token, so an upgrade shows here after the account's next Claude Code run.
func ClaudePlan(dir string) string {
	account := claudeAccount(dir)
	raw, _ := os.ReadFile(claudeCredentialsPath(dir))
	parts := []string{
		account.Get("organizationType").String(),
		account.Get("organizationRateLimitTier").String(),
		gjson.GetBytes(raw, "claudeAiOauth.subscriptionType").String(),
		gjson.GetBytes(raw, "claudeAiOauth.rateLimitTier").String(),
	}
	if strings.Join(parts, "") == "" {
		return ""
	}
	return strings.Join(parts, "/")
}

// ClaudeTier is the rate-limit tier Claude Code last fetched for the dir's
// account, such as default_claude_max_20x; "" when unknown.
func ClaudeTier(dir string) string {
	if tier := claudeAccount(dir).Get("organizationRateLimitTier").String(); tier != "" {
		return tier
	}
	raw, _ := os.ReadFile(claudeCredentialsPath(dir))
	return gjson.GetBytes(raw, "claudeAiOauth.rateLimitTier").String()
}
