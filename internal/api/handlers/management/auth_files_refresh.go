package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
)

// RefreshAuthFiles triggers active refresh for a single auth file or all auth files.
// Accepts query parameters (?all=true, ?name=file.json) or JSON body ({"all": true, "name": "file.json"}).
func (h *Handler) RefreshAuthFiles(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		All       bool   `json:"all"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(clienterror.HTTPStatusFromErrorOr(errBind, http.StatusBadRequest), gin.H{"error": "invalid request body: " + errBind.Error()})
			return
		}
	}
	if c.Query("all") == "true" {
		req.All = true
	}
	if queryName := strings.TrimSpace(c.Query("name")); queryName != "" && req.Name == "" {
		req.Name = queryName
	}
	if queryAuthIndex := strings.TrimSpace(c.Query("auth_index")); queryAuthIndex != "" && req.AuthIndex == "" {
		req.AuthIndex = queryAuthIndex
	}

	ctx := c.Request.Context()

	if req.All {
		results := h.authManager.ForceRefreshAll(ctx)
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"results": results,
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or all=true is required"})
		return
	}

	targetAuth, ok := h.lookupAuthFile(name, req.AuthIndex)
	if !ok || targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	refreshed, err := h.authManager.ForceRefreshAuth(ctx, targetAuth.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	// Buff Cloud fork: answer with the credentials list entry, not the in-memory
	// auth, whose metadata holds the access and refresh tokens.
	entry := h.buildAuthFileEntry(refreshed, authFileQuotaContext{v8: c.GetBool(ConfigV8ContextKey)})
	if entry == nil {
		entry = gin.H{"id": targetAuth.ID}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"auth": entry,
	})
}
