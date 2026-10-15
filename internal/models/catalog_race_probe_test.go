package models

import (
	"sync"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// Concurrency probe: catalog hot-replace (management refresh / config apply)
// racing request-path routing reads. Adapted from the 2api-gateway-review
// race-probe template.
func TestZZRaceProbeCatalogReplaceVsRoute(t *testing.T) {
	catalog := NewCatalog(config.TierZen, map[string]string{"m1": "chat"})
	zen := []string{"m1", "m2", "m3"}
	goModels := []string{"m2", "m3", "m4"}
	native := map[config.Tier]map[string]wire.Protocol{
		config.TierZen: {"m1": wire.Chat, "m2": wire.Responses},
		config.TierGo:  {"m2": wire.Anthropic, "m3": wire.Chat},
	}
	unsupported := map[config.Tier]map[string]bool{
		config.TierZen: {"m3": true},
		config.TierGo:  {},
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // management path: refresh swap + availability toggles
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			catalog.ReplaceWithCapabilities(zen, goModels, native, unsupported, nil)
			catalog.SetFreeOnly(i%2 == 0)
		}
	}()
	go func() { // request path: routing reads
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			route, err := catalog.Route("m2", true, true, true)
			if err == nil {
				_ = route.ProtocolFor(config.TierGo)
			}
			_, _ = catalog.Route("m3", true, false, false)
			_ = catalog.Supported("m1")
			_ = catalog.IsFreeModel("m2")
		}
	}()
	go func() { // admin readers: diagnostics and snapshots
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			_ = catalog.List()
			_ = catalog.Snapshot()
			_ = catalog.Diagnostic("m2", wire.Chat, true, true, true)
			_, _ = catalog.AvailableModels(true, true, true)
			_ = catalog.MetadataForTier("m2", config.TierGo)
		}
	}()
	wg.Wait()
}
