package registry

import (
	"testing"
	"time"
)

func TestDetectChangedProviders_CodexConfigurationUpdate(t *testing.T) {
	oldData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna"}},
	}
	newData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna", SupportConfigurationUpdate: true}},
	}

	changed := detectChangedProviders(oldData, newData)
	if len(changed) != 1 || changed[0] != "codex" {
		t.Fatalf("configuration_update-only change: got providers %v, want [codex]", changed)
	}
}

func TestDetectChangedProviders_KimiAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}},
	}
	newData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}, {ID: "kimi-k3"}},
	}

	changed := detectChangedProviders(oldData, newData)
	expected := map[string]bool{
		"kimi":     false,
		"kimi-ai":  false,
		"kimi.ai":  false,
		"kimi.com": false,
	}

	for _, p := range changed {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for p, found := range expected {
		if !found {
			t.Errorf("expected changed provider %q to be reported, got %v", p, changed)
		}
	}
}

func TestModelCatalogRefreshIsFrequentAndQuietWhenUnchanged(t *testing.T) {
	if ModelCatalogRefreshInterval != 15*time.Minute {
		t.Fatalf("catalog refresh interval = %v, want 15m", ModelCatalogRefreshInterval)
	}
	if ModelsRefreshInterval != 3*time.Hour {
		t.Fatalf("entitlement refresh interval = %v, want 3h", ModelsRefreshInterval)
	}

	modelsCatalogStore.mu.Lock()
	original := modelsCatalogStore.data
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	if _, errPublish := publishCatalogBytes(embeddedModelsJSON); errPublish != nil {
		t.Fatalf("first publish: %v", errPublish)
	}
	changed, errPublish := publishCatalogBytes(embeddedModelsJSON)
	if errPublish != nil {
		t.Fatalf("second publish: %v", errPublish)
	}
	if len(changed) != 0 {
		t.Fatalf("unchanged catalog reported changed providers %v", changed)
	}
}
