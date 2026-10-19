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
	return auth != nil && linked.Dir(auth.Metadata) != ""
}

// LinkedAuthNeedsPrepare reports whether the linked dir holds a different or
// nearly expired token compared with the one in auth's metadata.
func LinkedAuthNeedsPrepare(auth *cliproxyauth.Auth) bool {
	return IsLinkedAuth(auth) && linked.NeedsSync(auth.Metadata, time.Now())
}

// PrepareLinkedAuth loads the linked dir's current token into auth, refreshing
// it first when it is about to expire.
func PrepareLinkedAuth(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if err := linked.Prepare(ctx, linkedRefreshClient(cfg, auth), auth.Metadata, time.Now()); err != nil {
		return nil, err
	}
	return auth, nil
}

// RefreshLinkedAuth refreshes the linked dir's token under the dir's lock and
// loads the result into auth.
func RefreshLinkedAuth(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	force := cliproxyauth.IsForceRefreshContext(ctx)
	if err := linked.Refresh(ctx, linkedRefreshClient(cfg, auth), auth.Metadata, force, time.Now()); err != nil {
		return nil, err
	}
	return auth, nil
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
