package models

import (
	"testing"

	"opencode2api/internal/config"
)

// TestLimitOverridesWin pins the models.limits contract: operator overrides
// beat both the upstream capability catalog and the models.dev fill-in, and
// only the named model is affected.
func TestLimitOverridesWin(t *testing.T) {
	catalog := NewCatalog(config.TierZen, nil)
	catalog.ReplaceWithCapabilities(
		[]string{"deepseek-v4.1-flash-free", "other-model"},
		nil, nil, nil,
		map[config.Tier]map[string]Metadata{
			config.TierZen: {
				"deepseek-v4.1-flash-free": {ContextWindow: 200000, MaxOutput: 32000, Reasoning: true, ToolCall: true},
				"other-model":              {ContextWindow: 128000, MaxOutput: 8000},
			},
			config.TierGo: {},
		},
	)

	baseline := catalog.MetadataForTier("deepseek-v4.1-flash-free", config.TierZen)
	if baseline.ContextWindow != 200000 || baseline.MaxOutput != 32000 {
		t.Fatalf("baseline metadata = %+v", baseline)
	}

	contextWindow, maxOutput := 1048576, 64000
	catalog.SetLimits(map[string]config.ModelLimits{
		"deepseek-v4.1-flash-free": {ContextWindow: &contextWindow, MaxOutput: &maxOutput},
	})

	overridden := catalog.MetadataForTier("deepseek-v4.1-flash-free", config.TierZen)
	if overridden.ContextWindow != 1048576 {
		t.Fatalf("context_window = %d, want 1048576", overridden.ContextWindow)
	}
	if overridden.MaxOutput != 64000 {
		t.Fatalf("max_output = %d, want 64000", overridden.MaxOutput)
	}
	// Capability flags the catalog published must survive the override.
	if !overridden.Reasoning || !overridden.ToolCall {
		t.Fatalf("capability flags were lost: %+v", overridden)
	}

	untouched := catalog.MetadataForTier("other-model", config.TierZen)
	if untouched.ContextWindow != 128000 || untouched.MaxOutput != 8000 {
		t.Fatalf("an unrelated model was modified: %+v", untouched)
	}
}

// TestLimitOverrideAppliesWithoutPricingStore covers the discovery path used
// before the first models.dev refresh: the override must still apply.
func TestLimitOverrideAppliesWithoutPricingStore(t *testing.T) {
	catalog := NewCatalog(config.TierZen, nil)
	contextWindow := 500000
	catalog.SetLimits(map[string]config.ModelLimits{"m1": {ContextWindow: &contextWindow}})
	if got := catalog.MetadataForTier("m1", config.TierZen).ContextWindow; got != 500000 {
		t.Fatalf("context_window = %d, want 500000", got)
	}
}
