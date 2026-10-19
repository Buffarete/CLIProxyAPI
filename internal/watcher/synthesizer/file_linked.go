package synthesizer

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	log "github.com/sirupsen/logrus"
)

// linkedAuthLabel loads a linked auth's token from its dir into metadata
// (memory only) and returns the file's label, falling back to fallback.
func linkedAuthLabel(metadata map[string]any, fallback string) string {
	if err := linked.Sync(metadata); err != nil {
		log.WithError(err).Warnf("linked auth %s: token not loaded", linked.Dir(metadata))
	}
	if label, _ := metadata["label"].(string); label != "" {
		return label
	}
	return fallback
}
