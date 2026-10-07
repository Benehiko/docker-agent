package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockCatalogStore implements ModelStore for testing
type mockCatalogStore struct {
	ModelStore

	db *modelsdev.Database
}

func (m *mockCatalogStore) GetDatabase(_ context.Context) (*modelsdev.Database, error) {
	return m.db, nil
}

// configProvider is a provider.Provider whose BaseConfig is fully controlled
// by the test, used to drive thinking-level cycling without a real client.
type configProvider struct {
	base.Config
}

func (configProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("not implemented")
}

func newConfigProvider(cfg latest.ModelConfig) *configProvider {
	return &configProvider{Config: base.Config{ModelConfig: cfg}}
}

func testProviderRegistry() *provider.Registry {
	factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		return newConfigProvider(*cfg), nil
	}
	return provider.NewRegistry(map[string]provider.Factory{
		"openai":                 factory,
		"openai_chatcompletions": factory,
		"openai_responses":       factory,
		"anthropic":              factory,
		"google":                 factory,
		"amazon-bedrock":         factory,
	})
}

func TestCurrentThinkingLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		budget *latest.ThinkingBudget
		want   effort.Level
	}{
		{"nil budget", nil, effort.None},
		{"disabled none", &latest.ThinkingBudget{Effort: "none"}, effort.None},
		{"disabled zero tokens", &latest.ThinkingBudget{Tokens: 0}, effort.None},
		{"effort high", &latest.ThinkingBudget{Effort: "high"}, effort.High},
		{"effort medium", &latest.ThinkingBudget{Effort: "medium"}, effort.Medium},
		{"token budget treated as none", &latest.ThinkingBudget{Tokens: 4096}, effort.None},
		{"adaptive treated as none", &latest.ThinkingBudget{Effort: "adaptive"}, effort.None},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{ThinkingBudget: tt.budget}
			assert.Equal(t, tt.want, currentThinkingLevel(cfg))
		})
	}
}

func TestModelSupportsThinking(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{} // nil modelsStore: rely on name heuristics only.

	tests := []struct {
		name string
		cfg  latest.ModelConfig
		want bool
	}{
		{"openai reasoning gpt-5", latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, true},
		{"openai o3", latest.ModelConfig{Provider: "openai", Model: "o3"}, true},
		{"openai non-reasoning gpt-4o", latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, false},
		{"openai gpt-5-chat is non-reasoning", latest.ModelConfig{Provider: "openai", Model: "gpt-5-chat"}, false},
		{"anthropic claude", latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5"}, true},
		{"bedrock claude", latest.ModelConfig{Provider: "amazon-bedrock", Model: "us.anthropic.claude-sonnet-4-5"}, true},
		{"gemini 3", latest.ModelConfig{Provider: "google", Model: "gemini-3-pro"}, true},
		{"gemini 2.5", latest.ModelConfig{Provider: "google", Model: "gemini-2.5-pro"}, true},
		{"gemini 2.0 no thinking", latest.ModelConfig{Provider: "google", Model: "gemini-2.0-flash"}, false},
		{"explicit thinking budget overrides heuristic", latest.ModelConfig{Provider: "dmr", Model: "deepseek-r1", ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"}}, true},
		{"disabled thinking budget does not count", latest.ModelConfig{Provider: "dmr", Model: "llama3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			assert.Equal(t, tt.want, r.modelSupportsThinking(t.Context(), &cfg))
		})
	}
}

func TestCycleAgentThinkingLevel_Errors(t *testing.T) {
	t.Parallel()

	t.Run("nil modelSwitcherCfg is unsupported", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{team: team.New(team.WithAgents(root))}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.ErrorIs(t, err, ErrUnsupported)
	})

	t.Run("agent not found", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{
			team:             team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "missing")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent not found")
	})

	t.Run("non-reasoning model is unsupported", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-4o"})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team:             team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{},
		}

		_, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.ErrorIs(t, err, ErrUnsupported)
		assert.False(t, root.HasModelOverride(), "no override should be set when cycling is unsupported")
	})
}

func TestAgentThinkingLabel(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{} // nil modelsStore: name heuristics only.

	tests := []struct {
		name string
		cfg  latest.ModelConfig
		want string
	}{
		{"reasoning model, no budget shows off", latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, "off"},
		{"reasoning model with effort", latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}, "high"},
		{"reasoning model disabled shows off", latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}}, "off"},
		{"adaptive budget shows adaptive", latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7", ThinkingBudget: &latest.ThinkingBudget{Effort: "adaptive"}}, "adaptive"},
		{"token budget shows token count", latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096}}, "4096"},
		{"non-reasoning model hides line", latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := agent.New("root", "test", agent.WithModel(newConfigProvider(tt.cfg)))
			assert.Equal(t, tt.want, r.agentThinkingLabel(t.Context(), a))
		})
	}
}

func TestCycleAgentThinkingLevel_AdvancesAndOverrides(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
		},
	}

	// gpt-5 has no thinking budget configured → current is None, so the first
	// cycle step lands on Minimal (the OpenAI cycle is none→minimal→…).
	level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
	require.NoError(t, err)
	assert.Equal(t, effort.Minimal, level)
	require.True(t, root.HasModelOverride(), "cycling must install a runtime override")

	override := root.Model(t.Context())
	require.NotNil(t, override)
	budget := override.BaseConfig().ModelConfig.ThinkingBudget
	require.NotNil(t, budget)
	assert.Equal(t, "minimal", budget.Effort)

	// Next step: minimal → low.
	level, err = r.CycleAgentThinkingLevel(t.Context(), "root")
	require.NoError(t, err)
	assert.Equal(t, effort.Low, level)
}

func TestSetAgentThinkingLevel(t *testing.T) {
	t.Parallel()

	newThinkingRuntime := func(cfg latest.ModelConfig, env map[string]string) (*LocalRuntime, *agent.Agent) {
		root := agent.New("root", "test", agent.WithModel(newConfigProvider(cfg)))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(env),
			},
		}
		return r, root
	}

	t.Run("supported level installs override", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)
		require.True(t, root.HasModelOverride(), "setting a level must install a runtime override")

		override := root.Model(t.Context())
		require.NotNil(t, override)
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget)
		assert.Equal(t, "high", budget.Effort)
	})

	t.Run("unsupported level errors and lists supported levels", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		// gpt-5 tops out at high: max must be rejected, not clamped.
		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")
		assert.Contains(t, err.Error(), "high")
		assert.False(t, root.HasModelOverride(), "no override should be set on rejection")
	})

	t.Run("non-reasoning model is unsupported", func(t *testing.T) {
		t.Parallel()
		r, root := newThinkingRuntime(
			latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.ErrorIs(t, err, ErrUnsupported)
		assert.False(t, root.HasModelOverride())
	})

	t.Run("nil modelSwitcherCfg is unsupported", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		r := &LocalRuntime{team: team.New(team.WithAgents(root))}

		_, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.High)
		require.ErrorIs(t, err, ErrUnsupported)
	})

	t.Run("model-specific top tier is accepted", func(t *testing.T) {
		t.Parallel()
		r, _ := newThinkingRuntime(
			latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"},
			map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		)

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
		require.NoError(t, err)
		assert.Equal(t, effort.Max, level)
	})
}

// TestSetAgentThinkingLevel_ConcurrentOverrideBetweenResolutionAndApply is
// the regression test for the single-snapshot fix: applyAgentThinkingLevel
// must resolve capabilities and re-create providers from the SAME
// EffectiveModels snapshot. A second, later snapshot would let a
// concurrent /model change or scoped override land between resolution and
// application, validating the effort against one model but applying it to
// another (see #3731 follow-up).
func TestSetAgentThinkingLevel_ConcurrentOverrideBetweenResolutionAndApply(t *testing.T) {
	t.Parallel()

	t.Run("override swapped mid-flight does not leak into the applied provider", func(t *testing.T) {
		t.Parallel()

		// gpt-5 tops out at high; opus 4.7 accepts max. If applyAgentThinkingLevel
		// re-read EffectiveModels after pick ran, it would recreate the override
		// from opus instead of gpt-5, silently applying "high" to a model that was
		// never validated against it.
		modelA := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
		modelB := newConfigProvider(latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"})

		root := agent.New("root", "test", agent.WithModel(modelA))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider: environment.NewMapEnvProvider(map[string]string{
					"OPENAI_API_KEY":    "sk-test",
					"ANTHROPIC_API_KEY": "sk-test",
				}),
			},
		}

		// pick runs after the snapshot is resolved but before providers are
		// re-created; a concurrent /model change landing here is exactly the
		// race the bug exposed.
		level, err := r.applyAgentThinkingLevel(t.Context(), "root", func(supported []effort.Level, _ effort.Level) (effort.Level, error) {
			require.NotContains(t, supported, effort.Max, "resolution must see model A (gpt-5), which tops out at high")
			root.SetModelOverride(modelB)
			return effort.High, nil
		})
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		assert.Equal(t, "openai", override.BaseConfig().ModelConfig.Provider,
			"the applied provider must be re-created from the model validated against (A), not the one swapped in mid-flight (B)")
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget)
		assert.Equal(t, "high", budget.Effort)
	})

	t.Run("override cleared mid-flight does not install an empty override", func(t *testing.T) {
		t.Parallel()

		// No configured models: the agent's only model comes from the runtime
		// override, so clearing it mid-flight reproduces the empty second-snapshot
		// case the bug allowed through.
		modelA := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5"})
		root := agent.New("root", "test")
		root.SetModelOverride(modelA)

		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.applyAgentThinkingLevel(t.Context(), "root", func(_ []effort.Level, _ effort.Level) (effort.Level, error) {
			// Simulate a concurrent override reset (e.g. /model default) landing
			// between resolution and provider recreation.
			root.SetModelOverride()
			return effort.High, nil
		})
		require.NoError(t, err)
		assert.Equal(t, effort.High, level)

		override := root.Model(t.Context())
		require.NotNil(t, override, "must not silently install an empty override while reporting success")
		assert.Equal(t, "openai", override.BaseConfig().ModelConfig.Provider)
	})
}

func TestCurrentAgentThinkingLevels(t *testing.T) {
	t.Parallel()

	newThinkingRuntimeWithRouter := func(cfg latest.ModelConfig, env map[string]string) *LocalRuntime {
		root := agent.New("root", "test", agent.WithModel(newConfigProvider(cfg)))
		tm := team.New(team.WithAgents(root))
		return &LocalRuntime{
			team:   tm,
			agents: newAgentRouter(tm, "root"),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(env),
			},
		}
	}

	t.Run("reasoning model returns its supported levels", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		levels := r.CurrentAgentThinkingLevels(t.Context())
		assert.NotEmpty(t, levels)
		assert.Contains(t, levels, effort.High)
		assert.NotContains(t, levels, effort.Max, "gpt-5 tops out below max")
	})

	t.Run("non-reasoning model is nil", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			map[string]string{"OPENAI_API_KEY": "sk-test"},
		)

		assert.Nil(t, r.CurrentAgentThinkingLevels(t.Context()))
	})

	t.Run("nil modelSwitcherCfg is nil", func(t *testing.T) {
		t.Parallel()
		root := agent.New("root", "test")
		tm := team.New(team.WithAgents(root))
		r := &LocalRuntime{team: tm, agents: newAgentRouter(tm, "root")}

		assert.Nil(t, r.CurrentAgentThinkingLevels(t.Context()))
	})

	t.Run("matches the levels SetAgentThinkingLevel validates against", func(t *testing.T) {
		t.Parallel()
		r := newThinkingRuntimeWithRouter(
			latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-7"},
			map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
		)

		levels := r.CurrentAgentThinkingLevels(t.Context())
		require.Contains(t, levels, effort.Max, "opus 4.7 top tier")

		// Every level CurrentAgentThinkingLevels reports must be one
		// SetAgentThinkingLevel actually accepts -- the single-source-of-truth
		// guarantee #3731 depends on.
		for _, level := range levels {
			_, err := r.SetAgentThinkingLevel(t.Context(), "root", level)
			assert.NoError(t, err, "level %q reported as supported but rejected", level)
		}
	})
}

// TestSetAgentThinkingLevel_NoneSurvivesOnGPT56 is the regression test for
// the gpt-5.6+ "none" gating: cycling an agent's model to None on a model
// with a real API-level none effort must produce a provider whose config
// still carries an explicit ThinkingBudget{Effort: "none"} (not nil), so the
// value actually reaches the OpenAI client instead of silently reverting to
// the model's default effort. Older OpenAI models (no none effort) keep the
// existing nil-normalize behavior.
func TestSetAgentThinkingLevel_NoneSurvivesOnGPT56(t *testing.T) {
	t.Parallel()

	t.Run("gpt-5.6 preserves explicit none", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.None)
		require.NoError(t, err)
		assert.Equal(t, effort.None, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		budget := override.BaseConfig().ModelConfig.ThinkingBudget
		require.NotNil(t, budget, "gpt-5.6 has a real API none effort; the budget must survive provider construction")
		assert.Equal(t, "none", budget.Effort)
	})

	t.Run("gpt-5.2 normalizes none to nil", func(t *testing.T) {
		t.Parallel()
		model := newConfigProvider(latest.ModelConfig{Provider: "openai", Model: "gpt-5.2", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}})
		root := agent.New("root", "test", agent.WithModel(model))
		r := &LocalRuntime{
			team: team.New(team.WithAgents(root)),
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
			},
		}

		level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.None)
		require.NoError(t, err)
		assert.Equal(t, effort.None, level)

		override := root.Model(t.Context())
		require.NotNil(t, override)
		assert.Nil(t, override.BaseConfig().ModelConfig.ThinkingBudget, "gpt-5.2 has no API none effort; it must keep the nil-normalize behavior")
	})
}

// TestCycleAgentThinkingLevel_PerModelTopTier verifies that cycling only
// offers the top effort tiers to the Claude models whose API accepts them.
func TestCycleAgentThinkingLevel_PerModelTopTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modelID   string
		wantCycle []effort.Level
	}{
		{
			name:      "sonnet 4.5 never reaches xhigh or max",
			modelID:   "claude-sonnet-4-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.None},
		},
		{
			name:      "opus 4.5 never reaches xhigh or max",
			modelID:   "claude-opus-4-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.None},
		},
		{
			name:      "sonnet 4.6 reaches max but not xhigh",
			modelID:   "claude-sonnet-4-6",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
		{
			name:      "opus 4.6 reaches max but not xhigh",
			modelID:   "claude-opus-4-6",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
		{
			name:      "opus 4.7 reaches both xhigh and max",
			modelID:   "claude-opus-4-7",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "opus 4.8 reaches both xhigh and max",
			modelID:   "claude-opus-4-8",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "fable 5 reaches both xhigh and max",
			modelID:   "claude-fable-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "mythos 5 reaches both xhigh and max",
			modelID:   "claude-mythos-5",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None},
		},
		{
			name:      "mythos preview reaches max but not xhigh",
			modelID:   "claude-mythos-preview",
			wantCycle: []effort.Level{effort.Low, effort.Medium, effort.High, effort.Max, effort.None},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := newConfigProvider(latest.ModelConfig{Provider: "anthropic", Model: tt.modelID})
			root := agent.New("root", "test", agent.WithModel(model))
			r := &LocalRuntime{
				team: team.New(team.WithAgents(root)),
				modelSwitcherCfg: &ModelSwitcherConfig{
					ProviderRegistry: testProviderRegistry(),
					EnvProvider:      environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "sk-test"}),
				},
			}

			// Walk one full cycle starting from None and record every level.
			var got []effort.Level
			for range tt.wantCycle {
				level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
				require.NoError(t, err)
				got = append(got, level)
			}
			assert.Equal(t, tt.wantCycle, got)
		})
	}
}

// TestCycleAgentThinkingLevel_VercelQualifiedGPT56 is the runtime regression
// test for the second-review Vercel finding: the exact default from
// pkg/config/auto.go ("vercel" provider, "openai/gpt-5.6-sol" model) must
// cycle through the full gpt-5.6 ladder — including xhigh and max — even
// though the provider name itself ("vercel") is not "openai". Minimal is
// skipped: gpt-5.6 drops it.
func TestCycleAgentThinkingLevel_VercelQualifiedGPT56(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"AI_GATEWAY_API_KEY": "vck-test"}),
		},
	}

	wantCycle := []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max, effort.None}
	var got []effort.Level
	for range wantCycle {
		level, err := r.CycleAgentThinkingLevel(t.Context(), "root")
		require.NoError(t, err)
		got = append(got, level)
	}
	assert.Equal(t, wantCycle, got)
}

// TestSetAgentThinkingLevel_VercelQualifiedGPT56Max verifies that "max" is
// accepted (not rejected as unsupported) for the exact Vercel default model,
// and that the resulting override still carries the "vercel" provider and
// the "openai/" qualified model id.
func TestSetAgentThinkingLevel_VercelQualifiedGPT56Max(t *testing.T) {
	t.Parallel()

	model := newConfigProvider(latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol"})
	root := agent.New("root", "test", agent.WithModel(model))
	r := &LocalRuntime{
		team: team.New(team.WithAgents(root)),
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{"AI_GATEWAY_API_KEY": "vck-test"}),
		},
	}

	level, err := r.SetAgentThinkingLevel(t.Context(), "root", effort.Max)
	require.NoError(t, err)
	assert.Equal(t, effort.Max, level)

	override := root.Model(t.Context())
	require.NotNil(t, override)
	cfg := override.BaseConfig().ModelConfig
	assert.Equal(t, "vercel", cfg.Provider)
	assert.Equal(t, "openai/gpt-5.6-sol", cfg.Model)
	require.NotNil(t, cfg.ThinkingBudget)
	assert.Equal(t, "max", cfg.ThinkingBudget.Effort)
}

func TestIsInlineAlloySpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		modelRef string
		want     bool
	}{
		{
			name:     "single inline model",
			modelRef: "openai/gpt-4o",
			want:     false,
		},
		{
			name:     "two inline models",
			modelRef: "openai/gpt-4o,anthropic/claude-sonnet-4-0",
			want:     true,
		},
		{
			name:     "three inline models",
			modelRef: "openai/gpt-4o,anthropic/claude-sonnet-4-0,google/gemini-2.0-flash",
			want:     true,
		},
		{
			name:     "with spaces",
			modelRef: "openai/gpt-4o, anthropic/claude-sonnet-4-0",
			want:     true,
		},
		{
			name:     "named model (no slash)",
			modelRef: "my_fast_model",
			want:     false,
		},
		{
			name:     "comma separated named models (not inline alloy)",
			modelRef: "fast_model,smart_model",
			want:     false,
		},
		{
			name:     "mixed named and inline",
			modelRef: "fast_model,openai/gpt-4o",
			want:     false, // "fast_model" doesn't contain "/" so it's not an inline alloy
		},
		{
			name:     "empty string",
			modelRef: "",
			want:     false,
		},
		{
			name:     "just commas",
			modelRef: ",,",
			want:     false, // No valid parts after trimming
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isInlineAlloySpec(tt.modelRef)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsEmbeddingModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		family string
		model  string
		want   bool
	}{
		// Family-based detection
		{"family text-embedding", "text-embedding", "Text Embedding 3 Large", true},
		{"family cohere-embed", "cohere-embed", "Embed v4.0", true},
		{"family mistral-embed", "mistral-embed", "Mistral Embed", true},
		{"family gemini-embedding", "gemini-embedding", "Gemini Embedding", true},
		// Name-based fallback (empty family)
		{"name fallback embed", "", "Text Embedding 3 Large", true},
		{"name fallback mistral", "", "Mistral Embed", true},
		// Non-embedding models
		{"gpt family", "gpt", "GPT-4o", false},
		{"claude family", "claude-sonnet", "Claude Sonnet 4", false},
		{"llama family", "llama", "Llama 3.1 70B", false},
		{"empty both", "", "GPT-4o", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isEmbeddingModel(tt.family, tt.model)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMapModelsDevProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		providerID   string
		wantProvider string
		wantSupport  bool
	}{
		{"openai", "openai", true},
		{"anthropic", "anthropic", true},
		{"google", "google", true},
		{"mistral", "mistral", true},
		{"xai", "xai", true},
		{"amazon-bedrock", "amazon-bedrock", true},
		{"unsupported-provider", "", false},
		{"cohere", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			t.Parallel()
			gotProvider, gotSupport := mapModelsDevProvider(tt.providerID)
			assert.Equal(t, tt.wantProvider, gotProvider)
			assert.Equal(t, tt.wantSupport, gotSupport)
		})
	}
}

func TestGetAvailableProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		envVars       map[string]string
		modelsGateway string
		wantProviders []string
	}{
		{
			name: "only openai key",
			envVars: map[string]string{
				"OPENAI_API_KEY": "sk-test",
			},
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			name: "openai and anthropic keys",
			envVars: map[string]string{
				"OPENAI_API_KEY":    "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
			},
			wantProviders: []string{"openai", "anthropic", "dmr", "ollama"},
		},
		{
			// A gateway no longer replaces availability with a hardcoded
			// provider list: direct credentials keep counting.
			name: "with gateway - direct credentials still count",
			envVars: map[string]string{
				"DOCKER_TOKEN":   "test-token",
				"OPENAI_API_KEY": "sk-test",
			},
			modelsGateway: "https://gateway.example.com",
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			// A generic gateway must not require the Docker token for the
			// credential-based fallback either.
			name: "with gateway but no token",
			envVars: map[string]string{
				"OPENAI_API_KEY": "sk-test",
			},
			modelsGateway: "https://gateway.example.com",
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			name: "aws access key for bedrock",
			envVars: map[string]string{
				"AWS_ACCESS_KEY_ID": "AKIA...",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws profile for bedrock",
			envVars: map[string]string{
				"AWS_PROFILE": "my-profile",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws web identity for bedrock (EKS/IRSA)",
			envVars: map[string]string{
				"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/token",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws container credentials for bedrock (ECS)",
			envVars: map[string]string{
				"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/...",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &LocalRuntime{
				modelSwitcherCfg: &ModelSwitcherConfig{
					ProviderRegistry: testProviderRegistry(),
					EnvProvider:      environment.NewMapEnvProvider(tt.envVars),
					ModelsGateway:    tt.modelsGateway,
				},
			}

			got := r.getAvailableProviders(t.Context())

			for _, want := range tt.wantProviders {
				assert.True(t, got[want], "expected provider %s to be available", want)
			}
		})
	}
}

// TestGetAvailableProviders_GatewayDoesNotHardcodeProviders pins the removal
// of the old gateway special case: a Docker token alone must not surface a
// hardcoded provider list, since the fallback taken when gateway discovery
// fails is purely credential-based.
func TestGetAvailableProviders_GatewayDoesNotHardcodeProviders(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"DOCKER_TOKEN": "test-token",
			}),
			ModelsGateway: "https://gateway.example.com",
		},
	}

	got := r.getAvailableProviders(t.Context())

	for _, name := range []string{"openai", "anthropic", "google", "mistral", "xai"} {
		assert.False(t, got[name], "provider %s must not be available from the Docker token alone", name)
	}
	assert.True(t, got["dmr"], "local providers stay available")
	assert.True(t, got["ollama"], "local providers stay available")
}

// TestGetAvailableProviders_TemplatedAlias verifies that an alias with a
// templated base URL (Cloudflare's account/gateway-scoped endpoints) is only
// advertised once every env var its URL references is set, not on the token
// alone. Otherwise the picker would surface catalog models that cannot be
// selected (they fail at build time on the missing account/gateway id).
func TestGetAvailableProviders_TemplatedAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		envVars     map[string]string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:       "token alone does not advertise Cloudflare",
			envVars:    map[string]string{"CLOUDFLARE_API_TOKEN": "cf-test"},
			wantAbsent: []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"},
		},
		{
			name: "token and account id advertise workers-ai but not the gateway",
			envVars: map[string]string{
				"CLOUDFLARE_API_TOKEN":  "cf-test",
				"CLOUDFLARE_ACCOUNT_ID": "acc-123",
			},
			wantPresent: []string{"cloudflare-workers-ai"},
			wantAbsent:  []string{"cloudflare-ai-gateway"},
		},
		{
			name: "token, account and gateway ids advertise both",
			envVars: map[string]string{
				"CLOUDFLARE_API_TOKEN":  "cf-test",
				"CLOUDFLARE_ACCOUNT_ID": "acc-123",
				"CLOUDFLARE_GATEWAY_ID": "gw-9",
			},
			wantPresent: []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"},
		},
		{
			name:        "static-URL alias is still advertised on its token alone",
			envVars:     map[string]string{"AI_GATEWAY_API_KEY": "vck-test"},
			wantPresent: []string{"vercel"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &LocalRuntime{
				modelSwitcherCfg: &ModelSwitcherConfig{
					ProviderRegistry: testProviderRegistry(),
					EnvProvider:      environment.NewMapEnvProvider(tt.envVars),
				},
			}

			got := r.getAvailableProviders(t.Context())

			for _, want := range tt.wantPresent {
				assert.True(t, got[want], "expected provider %s to be available", want)
			}
			for _, absent := range tt.wantAbsent {
				assert.False(t, got[absent], "expected provider %s to NOT be available", absent)
			}
		})
	}
}

// TestGetAvailableProviders_AnthropicWIF verifies that a workspace configured
// with Workload Identity Federation surfaces the anthropic provider in the
// model picker even without ANTHROPIC_API_KEY in the env.
func TestGetAvailableProviders_AnthropicWIF(t *testing.T) {
	t.Parallel()

	wifAuth := &latest.AuthConfig{
		Type: latest.AuthTypeWorkloadIdentityFederation,
		Federation: &latest.FederationAuthConfig{
			FederationRuleID: "fdrl_x",
			OrganizationID:   "org",
			IdentityToken:    &latest.IdentityTokenSourceConfig{File: "/t"},
		},
	}

	t.Run("model-level auth", func(t *testing.T) {
		t.Parallel()
		r := &LocalRuntime{
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(nil),
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "anthropic", Model: "claude-x", Auth: wifAuth},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.True(t, got["anthropic"], "WIF should surface anthropic in available providers")
	})

	t.Run("provider-level auth", func(t *testing.T) {
		t.Parallel()
		r := &LocalRuntime{
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(nil),
				Providers: map[string]latest.ProviderConfig{
					"claude": {Provider: "anthropic", Auth: wifAuth},
				},
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "claude", Model: "claude-x"},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.True(t, got["anthropic"], "WIF on provider should surface anthropic")
	})

	t.Run("no auth and no api key", func(t *testing.T) {
		t.Parallel()
		r := &LocalRuntime{
			modelSwitcherCfg: &ModelSwitcherConfig{
				ProviderRegistry: testProviderRegistry(),
				EnvProvider:      environment.NewMapEnvProvider(nil),
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "anthropic", Model: "claude-x"},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.False(t, got["anthropic"], "plain anthropic config without API key must not be available")
	})
}

func TestBuildCatalogChoices(t *testing.T) {
	t.Parallel()

	// Create a mock database with some models
	db := &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"gpt-4o": {
						Name: "GPT-4o",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
					"dall-e-3": {
						Name: "DALL-E 3",
						Modalities: modelsdev.Modalities{
							Output: []string{"image"}, // Not a text model
						},
					},
					"text-embedding-3-large": {
						Name:   "Text Embedding 3 Large",
						Family: "text-embedding",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"}, // Embedding model - identified by family field
						},
					},
				},
			},
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-0": {
						Name: "Claude Sonnet 4",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
			"unsupported": {
				Models: map[string]modelsdev.Model{
					"some-model": {
						Name: "Some Model",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
		},
	}

	r := &LocalRuntime{
		modelsStore: &mockCatalogStore{db: db},
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"OPENAI_API_KEY":    "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
			}),
			Models: map[string]latest.ModelConfig{
				"my_model": {Provider: "openai", Model: "gpt-4o"}, // This should be excluded from catalog (duplicate)
			},
		},
	}

	choices := r.buildCatalogChoices(t.Context())

	// Should include Claude Sonnet (not a duplicate)
	var foundClaude bool
	for _, c := range choices {
		if c.Ref == "anthropic/claude-sonnet-4-0" {
			foundClaude = true
			assert.True(t, c.IsCatalog)
			assert.Equal(t, "Claude Sonnet 4", c.Name)
		}
	}
	require.True(t, foundClaude, "should include Claude Sonnet from catalog")

	// Should NOT include DALL-E 3 (not a text model)
	for _, c := range choices {
		assert.NotEqual(t, "openai/dall-e-3", c.Ref, "should not include non-text models")
	}

	// Should NOT include embedding models
	for _, c := range choices {
		assert.NotEqual(t, "openai/text-embedding-3-large", c.Ref, "should not include embedding models")
	}

	// Should NOT include unsupported provider
	for _, c := range choices {
		assert.NotEqual(t, "unsupported", c.Provider, "should not include unsupported providers")
	}
}

func TestBuildCatalogChoicesWithDuplicates(t *testing.T) {
	t.Parallel()

	db := &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"gpt-4o": {
						Name: "GPT-4o",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
		},
	}

	r := &LocalRuntime{
		modelsStore: &mockCatalogStore{db: db},
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"OPENAI_API_KEY": "sk-test",
			}),
			Models: map[string]latest.ModelConfig{
				// This model has the same provider/model as the catalog entry
				"my_gpt4o": {Provider: "openai", Model: "gpt-4o"},
			},
		},
	}

	choices := r.buildCatalogChoices(t.Context())

	// Should NOT include gpt-4o since it's already in config
	for _, c := range choices {
		assert.NotEqual(t, "openai/gpt-4o", c.Ref, "should not include duplicates from config")
	}
}

func TestResolveModelRef_RejectsAlloyConfig(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			Models: map[string]latest.ModelConfig{
				// Alloy config: no provider, comma-separated models
				"alloy_model": {Model: "openai/gpt-4o,anthropic/claude-sonnet-4-0"},
			},
		},
	}

	_, err := r.resolveModelRef(t.Context(), "alloy_model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alloy")
}

func TestResolveModelRef_NilConfig(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{}

	_, err := r.resolveModelRef(t.Context(), "openai/gpt-4o")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestResolveModelRef_InvalidFormat(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{
		modelSwitcherCfg: &ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			Models:           map[string]latest.ModelConfig{},
		},
	}

	tests := []struct {
		name     string
		modelRef string
	}{
		{"no slash", "invalid"},
		{"empty provider", "/model"},
		{"empty model", "provider/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.resolveModelRef(t.Context(), tt.modelRef)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid model reference")
		})
	}
}

func TestDecorateModelChoices(t *testing.T) {
	t.Parallel()

	t.Run("default marked current when no override is set", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"",
			nil,
		)
		require.Len(t, got, 2)
		assert.True(t, got[0].IsCurrent, "the IsDefault model must be marked IsCurrent when no override is set")
		assert.False(t, got[1].IsCurrent)
	})

	t.Run("override matching a known choice marks it current", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"openai/gpt-4o",
			nil,
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when an override is active")
		assert.True(t, got[1].IsCurrent)
	})

	t.Run("synthesizes choice for inline override not in list", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"anthropic/claude-sonnet-4-0",
			nil,
		)
		require.Len(t, got, 2)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[1].Ref)
		assert.Equal(t, "anthropic", got[1].Provider)
		assert.Equal(t, "claude-sonnet-4-0", got[1].Model)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	t.Run("appends custom refs from session history", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"},
		)
		require.Len(t, got, 3)
		assert.Equal(t, "openai/gpt-4o-mini", got[0].Ref)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCustom)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[2].Ref)
		assert.True(t, got[2].IsCustom)
	})

	t.Run("does not duplicate custom ref already in list", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 1)
		assert.Equal(t, "openai/gpt-4o", got[0].Ref)
	})

	t.Run("non-provider override does not synthesize a fabricated choice", func(t *testing.T) {
		t.Parallel()
		// "my_model" is a config key (no slash); when not in the runtime's
		// list we should NOT fabricate a choice for it because we have no
		// provider/model breakdown to display.
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"my_model",
			nil,
		)
		require.Len(t, got, 1)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when override is unknown")
	})

	t.Run("custom ref matching the active override is marked current", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"openai/gpt-4o",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	// AvailableModels implementations may return a slice backed by an
	// internal cache; mutating its IsCurrent flag in place would leak
	// state across sessions. The function must therefore never modify
	// the input slice or its underlying array.
	t.Run("does not mutate the input slice", func(t *testing.T) {
		t.Parallel()
		input := []ModelChoice{
			{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
			{Name: "other", Ref: "openai/gpt-4o"},
		}
		orig := slices.Clone(input)

		_ = DecorateModelChoices(input, "openai/gpt-4o", []string{"anthropic/claude-sonnet-4-0"})

		assert.Equal(t, orig, input, "DecorateModelChoices must not modify the input slice")
	})
}

func TestAgentThinkingConfigurationIsLocalAndConservative(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cfg      latest.ModelConfig
		multiple bool
		want     bool
	}{
		{name: "explicit", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}, want: true},
		{name: "unknown", cfg: latest.ModelConfig{Provider: "custom", Model: "unknown"}},
		{name: "budget-only recognition", cfg: latest.ModelConfig{Provider: "openai", Model: "custom-reasoner", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}}},
		{name: "router", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5", Routing: []latest.RoutingRule{{}}}},
		{name: "alloy", cfg: latest.ModelConfig{Provider: "openai", Model: "gpt-5"}, multiple: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := []provider.Provider{newConfigProvider(tc.cfg)}
			if tc.multiple {
				models = append(models, newConfigProvider(tc.cfg))
			}
			a := agent.New("root", "test")
			for _, model := range models {
				agent.WithModel(model)(a)
			}
			r := &LocalRuntime{team: team.New(team.WithAgents(a)), modelSwitcherCfg: &ModelSwitcherConfig{}}
			levels, current := r.AgentThinkingConfiguration("root")
			assert.Equal(t, tc.want, len(levels) > 0)
			if tc.want {
				assert.Equal(t, effort.High, current)
			}
		})
	}
}

func TestBuildCatalogChoices_CanonicalProviders(t *testing.T) {
	t.Parallel()
	for _, legacy := range []string{"fireworks", "together", "moonshot", "opencode-zen"} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", legacy, custom), func(t *testing.T) {
				t.Parallel()
				canonical := modelsdev.CanonicalProviderID(legacy)
				alias, ok := provider.LookupAlias(canonical)
				require.True(t, ok)
				r := &LocalRuntime{
					modelsStore: &mockCatalogStore{db: &modelsdev.Database{Providers: map[string]modelsdev.Provider{
						canonical: {Models: map[string]modelsdev.Model{
							"configured": {Modalities: modelsdev.Modalities{Output: []string{"text"}}},
							"available":  {Modalities: modelsdev.Modalities{Output: []string{"text"}}},
						}},
					}}},
					modelSwitcherCfg: &ModelSwitcherConfig{
						ProviderRegistry: testProviderRegistry(),
						EnvProvider:      environment.NewMapEnvProvider(map[string]string{alias.TokenEnvVar: "test-key"}),
						Models:           map[string]latest.ModelConfig{"mine": {Provider: legacy, Model: "configured"}},
					},
				}
				if custom {
					r.modelSwitcherCfg.Providers = map[string]latest.ProviderConfig{legacy: {BaseURL: "https://custom.invalid/v1"}}
				}
				choices := r.buildCatalogChoices(t.Context())
				wantLen := 1
				if custom {
					wantLen = 2
				}
				require.Len(t, choices, wantLen)
				for _, choice := range choices {
					assert.Equal(t, canonical, choice.Provider)
					if !custom {
						assert.Equal(t, canonical+"/available", choice.Ref)
					}
				}
			})
		}
	}
}
