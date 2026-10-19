package synthesizer

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// applyLinkedAuth loads a linked auth's token from its dir into metadata
// (memory only), gives it the linked refresh lead and its file's label.
func applyLinkedAuth(a *coreauth.Auth) {
	if err := linked.Sync(a.Metadata); err != nil {
		log.WithError(err).Warnf("linked auth %s: token not loaded", linked.Dir(a.Metadata))
	}
	a.Runtime = linked.Runtime{}
	if label, _ := a.Metadata["label"].(string); label != "" {
		a.Label = label
	}
}
