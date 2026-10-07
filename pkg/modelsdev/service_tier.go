package modelsdev

import (
	"slices"
	"time"
)

// ForServiceTier adjusts catalogue rates for the actual response tier without mutating c.
// Unlisted models, providers and tiers retain their catalogue pricing.
func (c *Cost) ForServiceTier(id ID, tier string) *Cost {
	if c == nil || id.Provider != "openai" {
		return c
	}
	model := id.Model
	if len(model) > len("-2006-01-02") {
		cut := len(model) - len("2006-01-02")
		if model[cut-1] == '-' {
			if _, err := time.Parse(time.DateOnly, model[cut:]); err == nil {
				model = model[:cut-1]
			}
		}
	}

	// https://developers.openai.com/api/docs/pricing; don't extrapolate to future models.
	factor := 1.0
	switch tier {
	case "fast", "priority":
		switch model {
		case "gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
			"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-6.1-sol":
			factor = 2
		}
	case "ultrafast":
		if model == "gpt-6-astra" {
			factor = 6
		}
	}
	if factor == 1 {
		return c
	}

	out := *c
	out.Input *= factor
	out.Output *= factor
	out.CacheRead *= factor
	out.CacheWrite *= factor
	out.Tiers = slices.Clone(c.Tiers)
	for i := range out.Tiers {
		out.Tiers[i].Input *= factor
		out.Tiers[i].Output *= factor
		out.Tiers[i].CacheRead *= factor
		out.Tiers[i].CacheWrite *= factor
	}
	return &out
}
