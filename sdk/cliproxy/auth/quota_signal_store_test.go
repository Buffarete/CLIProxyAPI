package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// memoryStore is a Store whose List returns fresh copies of the saved auths, as a
// file store does after a restart: no quota observation.
type memoryStore struct{ auths map[string]*Auth }

func (s *memoryStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.auths))
	for _, a := range s.auths {
		out = append(out, &Auth{ID: a.ID, Provider: a.Provider, Label: a.Label})
	}
	return out, nil
}

func (s *memoryStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.auths[auth.ID] = auth.Clone()
	return auth.ID, nil
}

func (s *memoryStore) Delete(_ context.Context, id string) error {
	delete(s.auths, id)
	return nil
}

func TestQuotaObservationSurvivesReloadAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), QuotaSignalFileName)
	store := &memoryStore{auths: map[string]*Auth{"linked-claude.json": {ID: "linked-claude.json", Provider: "claude"}}}
	manager := NewManager(store, nil, nil)
	manager.SetQuotaSignalStore(NewQuotaSignalStore(path))
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}

	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.42"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       []string{"1792427400"},
	})
	manager.MarkResult(ctx, Result{AuthID: "linked-claude.json", Provider: "claude", Model: "claude-opus-5-5", Success: true})

	// The auth file watcher replaces the credential with one read from disk.
	if _, err := manager.Update(context.Background(), &Auth{ID: "linked-claude.json", Provider: "claude"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, _ := manager.GetByID("linked-claude.json")
	if got := reloaded.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Utilization"]; got != "0.42" {
		t.Fatalf("after reload 7d utilization = %q, want 0.42", got)
	}

	if err := manager.quotaSignals.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	restarted := NewManager(store, nil, nil)
	restarted.SetQuotaSignalStore(NewQuotaSignalStore(path))
	if err = restarted.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	auth, ok := restarted.GetByID("linked-claude.json")
	if !ok {
		t.Fatal("credential missing after restart")
	}
	rooms := RoomsFromQuota("claude", auth.Quota)
	if len(rooms) != 1 || rooms[0].Window != "7d" || rooms[0].Used != 0.42 || rooms[0].ResetsAt.Unix() != 1792427400 {
		t.Fatalf("rooms after restart = %+v, want the saved 7d observation", rooms)
	}
}

func TestQuotaSignalStoreDropsObservationsOlderThanAWeek(t *testing.T) {
	path := filepath.Join(t.TempDir(), QuotaSignalFileName)
	store := NewQuotaSignalStore(path)
	store.record("old", QuotaState{ObservedAt: time.Now().Add(-9 * 24 * time.Hour), Signals: map[string]string{"a": "1"}})
	store.record("new", QuotaState{ObservedAt: time.Now(), Signals: map[string]string{"a": "1"}})
	if err := store.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reopened := NewQuotaSignalStore(path)
	if _, ok := reopened.get("old"); ok {
		t.Fatal("observation older than 8 days was kept")
	}
	if _, ok := reopened.get("new"); !ok {
		t.Fatal("fresh observation was dropped")
	}
}
