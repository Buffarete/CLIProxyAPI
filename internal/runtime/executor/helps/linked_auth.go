package helps

import (
	"context"
	"net/http"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// IsLinkedAuth reports whether auth reads its token from a linked Claude Code
// config dir or Codex home (Buff Cloud fork).
func IsLinkedAuth(auth *cliproxyauth.Auth) bool {
	return auth != nil && strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, linked.MetadataKey)) != ""
}

// LinkedAuthNeedsPrepare reports whether the linked dir holds a different or
// nearly expired token compared with the one in auth's metadata.
func LinkedAuthNeedsPrepare(auth *cliproxyauth.Auth) bool {
	return IsLinkedAuth(auth) && linked.NeedsSync(linkedAuthMetadata(auth), time.Now())
}

// PrepareLinkedAuth loads the linked dir's current token into auth, refreshing
// it first when it is about to expire.
func PrepareLinkedAuth(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	metadata := linkedAuthMetadata(auth)
	if err := linked.Prepare(ctx, linkedRefreshClient(cfg, auth), metadata, time.Now()); err != nil {
		return nil, err
	}
	storeLinkedAuthMetadata(auth, metadata)
	return auth, nil
}

// RefreshLinkedAuth refreshes the linked dir's token under the dir's lock and
// loads the result into auth.
func RefreshLinkedAuth(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	force := cliproxyauth.IsForceRefreshContext(ctx)
	metadata := linkedAuthMetadata(auth)
	if err := linked.Refresh(ctx, linkedRefreshClient(cfg, auth), metadata, force, time.Now()); err != nil {
		return nil, err
	}
	storeLinkedAuthMetadata(auth, metadata)
	return auth, nil
}

// Read linked token fields through the same lock as upstream Claude profile metadata.
// Disk and refresh operations work on a private map without holding that lock.
func linkedAuthMetadata(auth *cliproxyauth.Auth) map[string]any {
	metadata := make(map[string]any)
	for _, key := range []string{"type", linked.MetadataKey, "access_token", "refresh_token", "expired", "login_expires_at", "id_token", "account_id", "last_refresh"} {
		if value := claudeauth.ReadMetadataString(&auth.Metadata, key); value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func storeLinkedAuthMetadata(auth *cliproxyauth.Auth, metadata map[string]any) {
	for _, key := range []string{"access_token", "refresh_token", "expired", "login_expires_at", "id_token", "account_id", "last_refresh"} {
		claudeauth.StoreMetadataValue(&auth.Metadata, key, metadata[key])
	}
}

// linkedRefreshClient builds the same token-endpoint client the provider's own
// refresh uses, honouring the auth's proxy over the global one.
func linkedRefreshClient(cfg *config.Config, auth *cliproxyauth.Auth) *http.Client {
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
	}
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		sdkCfg.ProxyURL = proxyURL
	}
	if strings.EqualFold(auth.Provider, "claude") {
		return claudeauth.NewAnthropicHttpClient(&sdkCfg)
	}
	return util.SetProxy(&sdkCfg, &http.Client{})
}
