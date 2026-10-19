package auth

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFileTokenStoreLinkedAuthTokensStayInLinkedDir(t *testing.T) {
	linkedDir := filepath.Join(t.TempDir(), ".claude_3")
	if err := os.Mkdir(linkedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UnixMilli()
	credentials := `{"claudeAiOauth":{"accessToken":"at-linked","refreshToken":"rt-linked","expiresAt":` + strconv.FormatInt(expires, 10) + `}}`
	if err := os.WriteFile(filepath.Join(linkedDir, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	authFile := filepath.Join(authDir, "linked-claude_3.json")
	if err := os.WriteFile(authFile, []byte(`{"type":"claude","linked_dir":"`+linkedDir+`","label":"Claude 3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)

	auths, err := store.List(context.Background())
	if err != nil || len(auths) != 1 {
		t.Fatalf("list: %v, %d auths", err, len(auths))
	}
	auth := auths[0]
	if auth.Label != "Claude 3" || auth.Metadata["access_token"] != "at-linked" || auth.Metadata["refresh_token"] != "rt-linked" {
		t.Fatalf("linked auth not loaded from its dir: label %q metadata %v", auth.Label, auth.Metadata)
	}

	auth.Metadata["access_token"] = "at-refreshed"
	auth.Metadata["email"] = "a@example.com"
	if _, err = store.Save(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(authFile)
	for _, secret := range []string{"at-", "rt-", "expired"} {
		if strings.Contains(string(saved), secret) {
			t.Fatalf("auth file holds %q: %s", secret, saved)
		}
	}
	if !strings.Contains(string(saved), "a@example.com") || auth.Metadata["access_token"] != "at-refreshed" {
		t.Fatalf("saved %s, in-memory token %v", saved, auth.Metadata["access_token"])
	}
	if got, _ := os.ReadFile(filepath.Join(linkedDir, ".credentials.json")); string(got) != credentials {
		t.Fatal("Save touched the linked dir")
	}
}
