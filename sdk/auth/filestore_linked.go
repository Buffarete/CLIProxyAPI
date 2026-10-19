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

// applyLinkedAuth loads a linked auth's token from its dir into metadata
// (memory only) and gives it the linked refresh lead.
func applyLinkedAuth(auth *cliproxyauth.Auth) {
	if err := linked.Sync(auth.Metadata); err != nil {
		log.WithError(err).Warnf("linked auth %s: token not loaded", linked.Dir(auth.Metadata))
	}
	auth.Runtime = linked.Runtime{}
}
