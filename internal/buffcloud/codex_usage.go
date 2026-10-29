package buffcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// CodexUsageURL is the ChatGPT backend endpoint Codex's /status reads. It
// carries what the response headers do not, notably rate_limit_reset_credits
// (banked resets) and per-model availability.
const CodexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// codexUsageTTL keeps the probe to one call a minute per account however often
// Buff Cloud polls.
const codexUsageTTL = time.Minute

// codexUsageFields are the parts of the response passed on; the account's
// email, user id and account id are left out.
var codexUsageFields = []string{
	"plan_type", "rate_limit", "code_review_rate_limit", "additional_rate_limits", "model_usage",
	"credits", "spend_control", "rate_limit_reached_type", "promo", "rate_limit_reset_credits",
}

// CodexUsage is the last wham/usage answer for one account.
type CodexUsage struct {
	// FetchedAt is epoch ms of the last attempt.
	FetchedAt int64 `json:"fetchedAt"`
	// Error is set when the last attempt failed; Fields then hold the previous answer.
	Error  string                     `json:"error,omitempty"`
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
}

// CodexCredential is what the probe needs: the account's current token and
// ChatGPT workspace id.
type CodexCredential struct {
	AccessToken string
	AccountID   string
}

// CodexUsageProbe fetches and caches wham/usage per account.
type CodexUsageProbe struct {
	URL string

	mu      sync.Mutex
	entries map[string]*codexUsageEntry
}

type codexUsageEntry struct {
	mu    sync.Mutex
	usage CodexUsage
}

// NewCodexUsageProbe returns a probe against CodexUsageURL.
func NewCodexUsageProbe() *CodexUsageProbe {
	return &CodexUsageProbe{URL: CodexUsageURL, entries: map[string]*codexUsageEntry{}}
}

// Forget drops the account's cached answer, so the next Get fetches.
func (p *CodexUsageProbe) Forget(accountID string) {
	p.mu.Lock()
	delete(p.entries, accountID)
	p.mu.Unlock()
}

// Get returns the account's usage, fetching it when the cached answer is older
// than a minute. credential is called only when a fetch is due.
func (p *CodexUsageProbe) Get(ctx context.Context, client *http.Client, accountID string, credential func() (CodexCredential, error), now time.Time) CodexUsage {
	p.mu.Lock()
	entry := p.entries[accountID]
	if entry == nil {
		entry = &codexUsageEntry{}
		p.entries[accountID] = entry
	}
	p.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.usage.FetchedAt != 0 && now.Sub(time.UnixMilli(entry.usage.FetchedAt)) < codexUsageTTL {
		return entry.usage
	}
	entry.usage.FetchedAt = now.UnixMilli()
	fields, err := p.fetch(ctx, client, credential)
	if err != nil {
		entry.usage.Error = err.Error()
	} else {
		entry.usage.Error = ""
		entry.usage.Fields = fields
	}
	return entry.usage
}

func (p *CodexUsageProbe) fetch(ctx context.Context, client *http.Client, credential func() (CodexCredential, error)) (map[string]json.RawMessage, error) {
	cred, err := credential()
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	if cred.AccessToken == "" {
		return nil, fmt.Errorf("credential has no access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if cred.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", cred.AccountID)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wham/usage: %w", redactBearer(err, cred.AccessToken))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("wham/usage: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wham/usage: status %d", resp.StatusCode)
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("wham/usage: response is not JSON")
	}
	fields := map[string]json.RawMessage{}
	for _, key := range codexUsageFields {
		if value := gjson.GetBytes(body, key); value.Exists() {
			fields[key] = json.RawMessage(value.Raw)
		}
	}
	return fields, nil
}

func redactBearer(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "<token>"))
}
