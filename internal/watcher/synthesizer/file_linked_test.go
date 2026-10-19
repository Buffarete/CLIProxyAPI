package synthesizer

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFileSynthesizerLoadsLinkedTokenFromItsDir(t *testing.T) {
	linkedDir := t.TempDir()
	expires := time.Now().Add(time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"at-linked","refreshToken":"rt-linked","expiresAt":` + strconv.FormatInt(expires, 10) + `}}`
	if err := os.WriteFile(filepath.Join(linkedDir, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	authFile := `{"type":"claude","linked_dir":"` + linkedDir + `","label":"Claude 4","email":"a@example.com"}`
	if err := os.WriteFile(filepath.Join(authDir, "linked-claude_4.json"), []byte(authFile), 0o600); err != nil {
		t.Fatal(err)
	}

	auths, err := NewFileSynthesizer().Synthesize(&SynthesisContext{Config: &config.Config{}, AuthDir: authDir, Now: time.Now(), IDGenerator: NewStableIDGenerator()})
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesize: %v, %d auths", err, len(auths))
	}
	auth := auths[0]
	if auth.Label != "Claude 4" || auth.Metadata["access_token"] != "at-linked" || auth.Metadata["refresh_token"] != "rt-linked" {
		t.Fatalf("label %q metadata %v", auth.Label, auth.Metadata)
	}
	if exp, ok := auth.ExpirationTime(); !ok || exp.Unix() != expires/1000 {
		t.Fatalf("expiry %v %v, want %d", exp, ok, expires/1000)
	}
	// The provider default would refresh this hour-old token now; the linked lead waits for the last 5 minutes.
	if lead := coreauth.ProviderRefreshLead("claude", auth.Runtime); lead == nil || *lead != 5*time.Minute {
		t.Fatalf("refresh lead %v, want 5m", lead)
	}
}
