package modelsdev

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostForServiceTier(t *testing.T) {
	t.Parallel()

	cost := &Cost{
		Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5,
		Tiers: []CostTier{{
			Rates: Rates{Input: 20, Output: 75, CacheRead: 2, CacheWrite: 25},
			Tier:  TierSpec{Type: "context", Size: 272_000},
		}},
	}
	for _, tier := range []string{"fast", "priority", "ultrafast"} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			factor := 2.0
			if tier == "ultrafast" {
				factor = 6
			}
			got := cost.ForServiceTier(NewID("openai", "gpt-6-astra"), tier)
			require.NotSame(t, cost, got)
			assert.Equal(t, Rates{Input: 10 * factor, Output: 50 * factor, CacheRead: factor, CacheWrite: 12.5 * factor}, got.RatesFor(272_000))
			assert.Equal(t, Rates{Input: 20 * factor, Output: 75 * factor, CacheRead: 2 * factor, CacheWrite: 25 * factor}, got.RatesFor(272_001))
			assert.Equal(t, cost.Tiers[0].Tier, got.Tiers[0].Tier)
			assert.Equal(t, Rates{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}, cost.RatesFor(272_000))
			assert.Equal(t, Rates{Input: 20, Output: 75, CacheRead: 2, CacheWrite: 25}, cost.RatesFor(272_001), "shared catalogue tiers must not be mutated")
		})
	}
}

func TestCostForServiceTierModels(t *testing.T) {
	t.Parallel()

	cost := &Cost{Input: 1}
	for _, model := range []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-6.1-sol"} {
		for _, tier := range []string{"fast", "priority"} {
			assert.InDelta(t, 2.0, cost.ForServiceTier(NewID("openai", model), tier).Input, 1e-9, "%s/%s", model, tier)
			assert.InDelta(t, 2.0, cost.ForServiceTier(NewID("openai", model+"-2026-09-04"), tier).Input, 1e-9, "dated %s/%s", model, tier)
		}
	}
	assert.InDelta(t, 6.0, cost.ForServiceTier(NewID("openai", "gpt-6-astra-2026-09-04"), "ultrafast").Input, 1e-9)
}

func TestCostForServiceTierUnchanged(t *testing.T) {
	t.Parallel()

	cost := &Cost{Input: 1}
	for _, tc := range []struct {
		provider, model, tier string
	}{
		{"openai", "gpt-6-astra", ""},
		{"openai", "gpt-6-astra", "default"},
		{"openai", "gpt-6-astra", "auto"},
		{"openai", "gpt-6-astra", "flex"},
		{"openai", "gpt-6-astra", "scale"},
		{"openai", "gpt-6-astra", "future-tier"},
		{"openai", "gpt-5.6-luna", "ultrafast"},
		{"openai", "gpt-6.1-sol", "ultrafast"},
		{"openai", "gpt-5.5", "fast"},
		{"openai", "gpt-7-astra", "fast"},
		{"openai", "gpt-6-astra-fast", "fast"},
		{"openai", "gpt-6-astra-extra", "fast"},
		{"openai", "gpt-6-astra-2026-02-30", "fast"},
		{"vercel", "openai/gpt-6-astra-fast", "fast"},
		{"azure", "gpt-6-astra", "fast"},
		{"chatgpt", "gpt-6-astra", "ultrafast"},
		{"custom", "gpt-6-astra", "fast"},
	} {
		assert.Same(t, cost, cost.ForServiceTier(NewID(tc.provider, tc.model), tc.tier), "%+v", tc)
	}
	var missing *Cost
	assert.Nil(t, missing.ForServiceTier(NewID("openai", "gpt-6-astra"), "ultrafast"))
}
