package management

// Buff Cloud fork: POST /buff/recheck, behind Buff Cloud's Recheck and refresh
// buttons.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
)

// PostBuffRecheck serves POST /v8/management/buff/recheck. The body names the
// accounts, {"ids": ["linked-claude_7.json"]}; an empty list or body means every
// account. All of them are read at once: a Claude account through Claude
// Code's /usage (no request with its token from the proxy), a Codex account
// through wham/usage. Each success replaces the account's reading and clears
// its cooldown; a failure leaves the account as it was. An account read in the
// last two minutes is not read again. The answer has one result per account
// ({id, at, cached, error}) and GET /buff/accounts' list after the reads.
func (h *Handler) PostBuffRecheck(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"ids": ["<account id>", ...]}`})
			return
		}
	}
	known := map[string]bool{}
	var all []string
	for _, auth := range h.authManager.List() {
		if auth != nil && isAuthFileListable(auth) {
			known[auth.ID] = true
			all = append(all, auth.ID)
		}
	}
	ids := all
	if len(body.IDs) > 0 {
		ids = nil
		for _, id := range body.IDs {
			id = strings.TrimSpace(id)
			if !known[id] {
				c.JSON(http.StatusNotFound, gin.H{"error": "no account " + id})
				return
			}
			ids = append(ids, id)
		}
	}
	ctx := c.Request.Context()
	results := make([]buffRecheckResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			results[i] = h.recheckOne(ctx, id, time.Now())
		}(i, id)
	}
	wg.Wait()
	now := time.Now()
	c.JSON(http.StatusOK, gin.H{
		"rechecked":   ids,
		"results":     results,
		"recheckedAt": now.UnixMilli(),
		"settings":    h.buffSettings(),
		"accounts":    h.buffAccounts(ctx, now, true),
	})
}

// buffRecheckFresh is how long a recheck's reading stands: a press within it
// answers with that reading instead of reading the account again.
const buffRecheckFresh = 2 * time.Minute

var (
	buffRecheckMu   sync.Mutex
	buffRecheckLast = map[string]time.Time{}
	// buffClaudeUsage reads a Claude account through Claude Code; tests replace it.
	buffClaudeUsage = buffcloud.ReadClaudeUsage
)

type buffRecheckResult struct {
	ID string `json:"id"`
	// At is when the reading was taken (epoch ms); null when it failed.
	At     *int64 `json:"at"`
	Cached bool   `json:"cached,omitempty"`
	Error  string `json:"error,omitempty"`
}

// recheckOne reads one account's usage now: a Claude account through Claude
// Code's /usage in its linked dir, a Codex account through wham/usage. On
// success its cooldown is cleared and the new reading replaces the old one; on
// failure the account is left as it was and the error says why.
func (h *Handler) recheckOne(ctx context.Context, id string, now time.Time) buffRecheckResult {
	result := buffRecheckResult{ID: id}
	buffRecheckMu.Lock()
	last, seen := buffRecheckLast[id]
	buffRecheckMu.Unlock()
	if seen && now.Sub(last) < buffRecheckFresh {
		result.At, result.Cached = epochMs(last), true
		return result
	}
	auth, ok := h.authManager.GetByID(id)
	if !ok || auth == nil {
		result.Error = "no account"
		return result
	}
	var headers http.Header
	switch {
	case strings.EqualFold(auth.Provider, "claude") && linked.Dir(auth.Metadata) != "":
		read, err := buffClaudeUsage(ctx, linked.Dir(auth.Metadata), now)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		headers = read
	case strings.EqualFold(auth.Provider, "codex"):
		buffCodexProbe.Forget(id)
		usage := buffCodexProbe.Get(ctx, h.buffHTTPClient(auth), id, func() (buffcloud.CodexCredential, error) {
			return buffCodexCredential(auth)
		}, now)
		if usage.Error != "" {
			result.Error = usage.Error
			return result
		}
		headers = buffcloud.CodexUsageHeaders(usage)
	}
	if _, err := h.authManager.RecheckAuth(ctx, id); err != nil {
		result.Error = err.Error()
		return result
	}
	if headers != nil {
		h.authManager.ObserveQuota(id, headers, now)
	}
	buffRecheckMu.Lock()
	buffRecheckLast[id] = now
	buffRecheckMu.Unlock()
	result.At = epochMs(now)
	return result
}
