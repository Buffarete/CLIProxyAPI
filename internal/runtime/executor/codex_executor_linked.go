package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ShouldPrepareRequestAuth is true only for a linked Codex home whose
// auth.json holds a different or nearly expired token (Buff Cloud fork).
func (e *CodexExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	return helps.LinkedAuthNeedsPrepare(auth)
}

// PrepareRequestAuth loads the linked Codex home's current token into auth.
func (e *CodexExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if !helps.IsLinkedAuth(auth) {
		return auth, nil
	}
	return helps.PrepareLinkedAuth(ctx, e.cfg, auth)
}
