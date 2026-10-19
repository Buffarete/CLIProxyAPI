// Package linked lets a claude or codex auth file point at a Claude Code config
// dir or a Codex home instead of holding tokens. The token is read from that
// dir's own credentials file on every use, refreshed under the dir's own lock and
// written back there in the file's own shape, so the proxy and the CLI that owns
// the dir share one login. Tokens live only in memory on the proxy side.
package linked

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/sjson"
)

// MetadataKey marks an auth file as linked; its value is the absolute dir path.
const MetadataKey = "linked_dir"

// refreshMargin is how close to expiry both CLIs refresh on use.
const refreshMargin = 5 * time.Minute

// secretKeys are the in-memory token fields that never reach the auth file.
var secretKeys = []string{
	"access_token", "refresh_token", "id_token", "expired", "last_refresh",
	"accessToken", "refreshToken", "idToken", "expire", "expires_at", "expiresAt", "siwc",
}

// token is what a linked credentials file holds that the proxy needs in memory.
type token struct {
	access      string
	refresh     string
	expires     time.Time
	idToken     string
	accountID   string
	lastRefresh string
}

func (t token) nearExpiry(now time.Time) bool {
	return t.expires.IsZero() || !t.expires.After(now.Add(refreshMargin))
}

// provider is one CLI's credential file format and refresh endpoint.
type provider interface {
	read(dir string) (token, error)
	// refresh re-reads the file (under the dir's lock where the CLI takes one) and
	// spends the refresh token only if needs reports the file's token still needs it.
	refresh(ctx context.Context, client *http.Client, dir string, needs func(token) bool, now time.Time) error
}

func providerFor(metadata map[string]any) (provider, error) {
	switch t, _ := metadata["type"].(string); strings.ToLower(strings.TrimSpace(t)) {
	case "claude":
		return claudeProvider{}, nil
	case "codex":
		return codexProvider{}, nil
	default:
		return nil, fmt.Errorf("linked credentials: unsupported type %q", t)
	}
}

// Dir returns the linked dir of an auth's metadata, or "" for an ordinary auth.
func Dir(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	dir, _ := metadata[MetadataKey].(string)
	return strings.TrimSpace(dir)
}

// StoreFields returns a copy of metadata without token fields, for persisting
// a linked auth to the auth dir.
func StoreFields(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	for _, k := range secretKeys {
		delete(out, k)
	}
	return out
}

// NeedsSync reports whether the linked file holds a different token than
// metadata, or one close enough to expiry that it should be refreshed first.
func NeedsSync(metadata map[string]any, now time.Time) bool {
	p, err := providerFor(metadata)
	if err != nil {
		return true
	}
	current, err := p.read(Dir(metadata))
	if err != nil {
		return true
	}
	access, _ := metadata["access_token"].(string)
	return current.access != access || current.nearExpiry(now)
}

// Sync copies the linked file's token into metadata (memory only).
func Sync(metadata map[string]any) error {
	p, err := providerFor(metadata)
	if err != nil {
		return err
	}
	current, err := p.read(Dir(metadata))
	if err != nil {
		return err
	}
	apply(metadata, current)
	return nil
}

// Prepare syncs metadata from the linked file before a request and refreshes
// the file first when its token is about to expire, as the CLI itself would.
func Prepare(ctx context.Context, client *http.Client, metadata map[string]any, now time.Time) error {
	if err := Sync(metadata); err != nil {
		return err
	}
	if p, _ := providerFor(metadata); p != nil {
		if current, err := p.read(Dir(metadata)); err == nil && !current.nearExpiry(now) {
			return nil
		}
	}
	return Refresh(ctx, client, metadata, false, now)
}

// Refresh refreshes the linked file's token and syncs metadata from it. Unless
// force is set, a token another process already replaced (one that differs
// from metadata's and is not about to expire) is adopted without a refresh.
func Refresh(ctx context.Context, client *http.Client, metadata map[string]any, force bool, now time.Time) error {
	p, err := providerFor(metadata)
	if err != nil {
		return err
	}
	dir := Dir(metadata)
	stale, _ := metadata["access_token"].(string)
	needs := func(current token) bool {
		return force || current.access == stale || current.nearExpiry(now)
	}
	if err = p.refresh(ctx, client, dir, needs, now); err != nil {
		return err
	}
	return Sync(metadata)
}

func apply(metadata map[string]any, t token) {
	metadata["access_token"] = t.access
	metadata["refresh_token"] = t.refresh
	if t.expires.IsZero() {
		delete(metadata, "expired")
	} else {
		metadata["expired"] = t.expires.UTC().Format(time.RFC3339)
	}
	if t.idToken != "" {
		metadata["id_token"] = t.idToken
	}
	if t.accountID != "" {
		metadata["account_id"] = t.accountID
	}
	if t.lastRefresh != "" {
		metadata["last_refresh"] = t.lastRefresh
	}
}

type jsonEdit struct {
	path  string
	value any
}

// writeEdited applies edits with sjson, which changes values in place, so key
// order, formatting and every key the proxy does not know (mcpOAuth,
// OPENAI_API_KEY, ...) stay exactly as the CLI wrote them.
func writeEdited(file string, raw []byte, edits []jsonEdit) error {
	var err error
	for _, e := range edits {
		if raw, err = sjson.SetBytes(raw, e.path, e.value); err != nil {
			return fmt.Errorf("linked credentials: update %s: %w", filepath.Base(file), err)
		}
	}
	return writeAtomic(file, raw)
}

// writeAtomic replaces file with data through a temp file and rename, mode 600,
// so a CLI reading concurrently sees either the old or the new file.
func writeAtomic(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, file)
}

// errRefreshStatus is a refused token refresh; the response body is not kept
// because it may echo credentials.
type errRefreshStatus struct {
	status int
	code   string
}

func (e errRefreshStatus) Error() string {
	if e.code != "" {
		return fmt.Sprintf("linked credentials: token refresh failed with status %d (%s)", e.status, e.code)
	}
	return fmt.Sprintf("linked credentials: token refresh failed with status %d", e.status)
}

// StatusCode lets the auth manager classify a refused refresh like any other.
func (e errRefreshStatus) StatusCode() int { return e.status }

var errNoToken = errors.New("linked credentials: file holds no OAuth token")

// Runtime is the auth runtime of a linked auth. Its refresh lead replaces the
// provider's (4 h for Claude, 96 h for Codex) with the CLIs' own 5 minutes, so
// the proxy rotates a shared login no earlier than Claude Code or Codex would.
type Runtime struct{}

// RefreshLead is read by the auth manager's refresh scheduler.
func (Runtime) RefreshLead() *time.Duration {
	lead := refreshMargin
	return &lead
}
