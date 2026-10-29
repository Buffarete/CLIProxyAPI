package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func writePlan(t *testing.T, dir, tier string) {
	t.Helper()
	profile := `{"oauthAccount":{"emailAddress":"will@example.com","organizationType":"claude_max","organizationRateLimitTier":"` + tier + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := `{"claudeAiOauth":{"subscriptionType":"max","rateLimitTier":"` + tier + `"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanWatchRechecksAnAccountWhosePlanChanged(t *testing.T) {
	authDir, claudeDir := t.TempDir(), t.TempDir()
	writePlan(t, claudeDir, "default_claude_max_5x")
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "linked-claude_7.json", Provider: "claude", Metadata: map[string]any{linked.MetadataKey: claudeDir},
		Unavailable: true, NextRetryAfter: time.Now().Add(60 * time.Hour),
		Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(60 * time.Hour), ObservedAt: time.Now(),
			Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "1.0"}}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	w := newPlanWatch(authDir)
	if got := w.check(context.Background(), manager); len(got) != 0 {
		t.Fatalf("first sight rechecked %v; it should only record the plan", got)
	}
	writePlan(t, claudeDir, "default_claude_max_20x")
	if got := w.check(context.Background(), manager); len(got) != 1 || got[0] != auth.ID {
		t.Fatalf("after the upgrade check() = %v, want [%s]", got, auth.ID)
	}
	updated, _ := manager.GetByID(auth.ID)
	if updated.Unavailable || len(updated.Quota.Signals) != 0 {
		t.Fatalf("after the upgrade: unavailable=%v signals=%v", updated.Unavailable, updated.Quota.Signals)
	}
	if got := w.check(context.Background(), manager); len(got) != 0 {
		t.Fatalf("an unchanged plan rechecked %v", got)
	}
	// A restart reads the saved plans, so a change made while the proxy was down is caught.
	writePlan(t, claudeDir, "default_claude_max_5x")
	if got := newPlanWatch(authDir).check(context.Background(), manager); len(got) != 1 {
		t.Fatalf("after a restart check() = %v, want the change caught", got)
	}
}
