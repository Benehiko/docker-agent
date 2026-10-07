package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestRunStreamServiceTierCost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, model, tier string
		fallback          bool
		cost              *latest.CostConfig
		want              float64
	}{
		{name: "Luna priority", model: "gpt-5.6-luna", tier: "priority", want: 0.0478},
		{name: "Astra fast", model: "gpt-6-astra", tier: "fast", want: 2.29},
		{name: "Astra ultrafast", model: "gpt-6-astra", tier: "ultrafast", want: 6.87},
		{name: "standard downgrade", model: "gpt-6-astra", tier: "default", want: 1.145},
		{name: "fallback ultrafast", model: "gpt-6-astra", tier: "ultrafast", fallback: true, want: 6.87},
		{name: "fallback override", model: "gpt-6-astra", tier: "ultrafast", fallback: true, cost: &latest.CostConfig{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}, want: 0.0995},
		{name: "free override", model: "gpt-6-astra", tier: "fast", cost: &latest.CostConfig{}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			usage := &chat.Usage{InputTokens: 50_000, CachedInputTokens: 20_000, CacheWriteTokens: 30_000, OutputTokens: 5_000, ReasoningTokens: 3_000, ServiceTier: tc.tier}
			stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(0, 0).Build()
			stream.responses[len(stream.responses)-1].Usage = usage
			id := "openai/" + tc.model
			model := &pricingProvider{Provider: &mockProvider{id: id, stream: stream}, cost: tc.cost}
			opts := []agent.Opt{
				agent.WithModel(model),
				agent.WithHooks(&latest.HooksConfig{AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "capture-tier-cost"}}}),
			}
			if tc.fallback {
				opts = append(opts,
					agent.WithModel(&countingProvider{id: "openai/gpt-5.6-luna", failCount: 100, err: errors.New("401 unauthorized")}),
					agent.WithFallbackModel(model), agent.WithFallbackRetries(-1))
			}
			root := agent.New("root", "test", opts...)
			rec := &recordingTelemetry{}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithModelStore(modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot())),
				WithSessionCompaction(false), WithTelemetry(rec), WithBudget(&latest.BudgetConfig{MaxCost: 100}))
			require.NoError(t, err)
			var captured *hooks.Input
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("capture-tier-cost",
				func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
					snapshot := *in
					captured = &snapshot
					return nil, nil
				}))
			sess := session.New(session.WithUserMessage("hi"))
			sess.Title = "Service tier test"
			var lastUsage *MessageUsage
			var budget *BudgetStatus
			for ev := range rt.RunStream(t.Context(), sess) {
				switch ev := ev.(type) {
				case *ErrorEvent:
					t.Errorf("unexpected runtime error: %s", ev.Error)
				case *TokenUsageEvent:
					if ev.Usage != nil && ev.Usage.LastMessage != nil {
						lastUsage = ev.Usage.LastMessage
					}
				case *BudgetUsageEvent:
					for _, b := range ev.Budgets {
						if b.Name == runBudgetName {
							budget = &b
						}
					}
				}
			}
			require.NotNil(t, captured)
			require.NotNil(t, captured.Cost)
			assert.Equal(t, id, captured.ModelID)
			require.NotNil(t, captured.Usage)
			assert.Equal(t, tc.tier, captured.Usage.ServiceTier)
			assert.InDelta(t, tc.want, *captured.Cost, 1e-9)
			assert.InDelta(t, tc.want, sess.OwnCost(), 1e-9)
			require.NotNil(t, lastUsage)
			assert.InDelta(t, tc.want, lastUsage.Cost, 1e-9)
			require.NotNil(t, budget)
			assert.InDelta(t, tc.want, budget.Cost, 1e-9)
			records := rec.snapshot().tokenUsages
			require.Len(t, records, 1)
			assert.InDelta(t, tc.want, records[0].Cost, 1e-9)
			assert.Equal(t, int64(100_000), records[0].InputTokens)
			assert.Equal(t, int64(5_000), records[0].OutputTokens)
		})
	}
}
