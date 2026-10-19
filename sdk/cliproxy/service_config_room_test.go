package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func applyRouting(t *testing.T, service *Service, routing internalconfig.RoutingConfig, sequence uint64) {
	t.Helper()
	if !service.applyManagerConfig(context.Background(), configCommit{cfg: &internalconfig.Config{Routing: routing}, sequence: sequence}) {
		t.Fatal("applyManagerConfig failed")
	}
}

func roomStrategyOf(t *testing.T, selector coreauth.Selector) (*coreauth.SessionAffinitySelector, string) {
	t.Helper()
	affinity, ok := selector.(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector = %T, want session affinity around the room selector", selector)
	}
	return affinity, coreauth.AffinityRoomStrategy(affinity)
}

func TestRoomStrategiesAreSelectableAndSwitchInPlace(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	applyRouting(t, service, internalconfig.RoutingConfig{Strategy: "most-room", SessionAffinity: true}, 1)
	before, strategy := roomStrategyOf(t, service.coreManager.Selector())
	if strategy != coreauth.RoomStrategyMostRoom {
		t.Fatalf("strategy = %q, want most-room", strategy)
	}

	// Switching between the room strategies keeps the affinity selector and its session bindings.
	applyRouting(t, service, internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true}, 2)
	after, strategy := roomStrategyOf(t, service.coreManager.Selector())
	if after != before || strategy != coreauth.RoomStrategySoonestReset {
		t.Fatalf("after switch: same selector=%v strategy=%q, want the same selector with soonest-reset", after == before, strategy)
	}

	applyRouting(t, service, internalconfig.RoutingConfig{Strategy: "round-robin", SessionAffinity: true}, 3)
	if service.coreManager.SetRoomStrategy(coreauth.RoomStrategyMostRoom) {
		t.Fatal("round-robin still has a room selector")
	}
}
