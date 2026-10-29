package management

// Buff Cloud fork: POST /buff/recheck, behind Buff Cloud's Recheck and refresh
// buttons.

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// PostBuffRecheck serves POST /v8/management/buff/recheck. The body names the
// accounts, {"ids": ["linked-claude_7.json"]}; an empty list or body means every
// account. Each one's cooldown and last quota reading are cleared, so its next
// request reads fresh headers, and a Codex account's wham/usage is fetched
// again now. The answer is GET /buff/accounts' list, read after the recheck,
// with the time of the recheck.
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
	for _, id := range ids {
		if _, err := h.authManager.RecheckAuth(ctx, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "recheck " + id + ": " + err.Error()})
			return
		}
		buffCodexProbe.Forget(id)
	}
	now := time.Now()
	c.JSON(http.StatusOK, gin.H{
		"rechecked":   ids,
		"recheckedAt": now.UnixMilli(),
		"settings":    h.buffSettings(),
		"accounts":    h.buffAccounts(ctx, now, true),
	})
}
