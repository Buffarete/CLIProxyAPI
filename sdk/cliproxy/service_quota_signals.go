package cliproxy

// Buff Cloud fork: persist quota observations next to the auth files.

import (
	"path/filepath"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// configureQuotaSignalStore gives the manager a store in the auth directory and
// returns it, or nil when there is no auth directory.
func (s *Service) configureQuotaSignalStore(cfg *config.Config) *coreauth.QuotaSignalStore {
	if s == nil || s.coreManager == nil || cfg == nil {
		return nil
	}
	authDir, errResolve := resolveCooldownStateAuthDir(cfg)
	if errResolve != nil || authDir == "" {
		if errResolve != nil {
			log.Warnf("quota signals: resolve auth directory: %v", errResolve)
		}
		return nil
	}
	store := coreauth.NewQuotaSignalStore(filepath.Join(authDir, coreauth.QuotaSignalFileName))
	s.coreManager.SetQuotaSignalStore(store)
	return store
}
