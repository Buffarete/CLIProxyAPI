package auth

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// saveLinkedAuthFile persists a linked auth without its token fields, which
// the auth holds in memory only and which belong to the linked dir's own file.
func saveLinkedAuthFile(path string, auth *cliproxyauth.Auth) error {
	auth.Metadata["disabled"] = auth.Disabled
	raw, err := json.Marshal(linked.StoreFields(auth.Metadata))
	if err != nil {
		return fmt.Errorf("auth filestore: marshal linked auth failed: %w", err)
	}
	if existing, errRead := os.ReadFile(path); errRead == nil && jsonEqual(existing, raw) {
		return nil
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("auth filestore: write linked auth failed: %w", err)
	}
	return nil
}

// syncLinkedMetadata loads a linked auth's token from its dir into metadata.
func syncLinkedMetadata(metadata map[string]any) {
	if err := linked.Sync(metadata); err != nil {
		log.WithError(err).Warnf("linked auth %s: token not loaded", linked.Dir(metadata))
	}
}
