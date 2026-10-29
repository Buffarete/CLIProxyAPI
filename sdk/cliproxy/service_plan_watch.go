package cliproxy

// Buff Cloud fork: recheck a Claude account when its plan changes. The proxy
// learns an account's room only from response headers, and an account at its
// limit gets no responses, so an upgrade would stay unseen until the old
// cooldown ends. Claude Code rewrites the plan fields in the linked dir when it
// fetches the profile; this watch compares them every two minutes and, on a
// change, clears the account's cooldown and reading (Manager.RecheckAuth).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// planWatchReadUsage reads a Claude account's usage; tests replace it.
var planWatchReadUsage = buffcloud.ReadClaudeUsage

const (
	planWatchInterval = 2 * time.Minute
	planStateFileName = "plans.state"
)

// planWatch keeps the last plan seen per credential, saved beside the auth files
// so a change made while the proxy was down is caught at start.
type planWatch struct {
	path  string
	plans map[string]string
	// read is when each Claude account's usage was last read by the watch.
	read map[string]time.Time
}

// planWatchUsageEvery is how often the watch reads a Claude account's usage
// through Claude Code when no request has brought a reading in that time.
const planWatchUsageEvery = 3 * time.Hour

func newPlanWatch(authDir string) *planWatch {
	w := &planWatch{path: filepath.Join(authDir, planStateFileName), plans: map[string]string{}, read: map[string]time.Time{}}
	if data, err := os.ReadFile(w.path); err == nil {
		_ = json.Unmarshal(data, &w.plans)
	}
	return w
}

// check compares every linked Claude credential's plan with the last one seen
// and rechecks those that changed. It returns the ids it rechecked.
func (w *planWatch) check(ctx context.Context, manager *coreauth.Manager) []string {
	var rechecked []string
	changed := false
	for _, auth := range manager.List() {
		if auth == nil || !strings.EqualFold(auth.Provider, "claude") {
			continue
		}
		dir := linked.Dir(auth.Metadata)
		if dir == "" {
			continue
		}
		w.readStale(ctx, manager, auth, dir)
		plan := linked.ClaudePlan(dir)
		if plan == "" || plan == w.plans[auth.ID] {
			continue
		}
		previous, known := w.plans[auth.ID]
		w.plans[auth.ID] = plan
		changed = true
		if !known {
			continue
		}
		if _, err := manager.RecheckAuth(ctx, auth.ID); err != nil {
			log.Warnf("plan watch: recheck %s: %v", auth.ID, err)
			continue
		}
		log.Infof("plan watch: %s plan changed from %s to %s; cleared its cooldown and reading", auth.ID, previous, plan)
		rechecked = append(rechecked, auth.ID)
	}
	if changed {
		if data, err := json.MarshalIndent(w.plans, "", "  "); err == nil {
			if errWrite := os.WriteFile(w.path, data, 0o600); errWrite != nil {
				log.Warnf("plan watch: save %s: %v", w.path, errWrite)
			}
		}
	}
	return rechecked
}

// startPlanWatch runs the watch until ctx ends.
func (s *Service) startPlanWatch(ctx context.Context, authDir string) {
	if s == nil || s.coreManager == nil || authDir == "" {
		return
	}
	w := newPlanWatch(authDir)
	go func() {
		ticker := time.NewTicker(planWatchInterval)
		defer ticker.Stop()
		for {
			w.check(ctx, s.coreManager)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// readStale reads an account's usage through Claude Code's /usage when its last
// reading, from a request or from this watch, is over planWatchUsageEvery old,
// so an idle or spent account still shows and routes on current numbers.
func (w *planWatch) readStale(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, dir string) {
	now := time.Now()
	if now.Sub(auth.Quota.ObservedAt) < planWatchUsageEvery || now.Sub(w.read[auth.ID]) < planWatchUsageEvery || planWatchReadUsage == nil {
		return
	}
	w.read[auth.ID] = now
	headers, err := planWatchReadUsage(ctx, dir, now)
	if err != nil {
		log.Warnf("plan watch: read usage of %s: %v", auth.ID, err)
		return
	}
	if _, err := manager.RecheckAuth(ctx, auth.ID); err == nil {
		manager.ObserveQuota(auth.ID, headers, now)
		log.Infof("plan watch: read usage of %s through Claude Code", auth.ID)
	}
}
