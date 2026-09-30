package tui_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/workflow"
)

type tuiWorkflowRouter struct {
	mu     sync.Mutex
	states []map[string]string
}

func (r *tuiWorkflowRouter) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, state.(map[string]string))
	return &evaluator.Result{Type: "choice", Choice: "answer", Probabilities: map[string]float64{"answer": 0.95, "other": 0.05}}, nil
}

func TestWorkflowConversation(t *testing.T) {
	isolateState(t)
	router := &tuiWorkflowRouter{}
	graphs := map[string]latest.WorkflowConfig{"gordon": {Entry: "route", Nodes: map[string]latest.WorkflowNode{
		"route":  {Type: "decision", AllowedNodes: []string{"answer", "other"}, DefaultNode: "other"},
		"answer": {Type: "agent"}, "other": {Type: "agent"},
	}}}
	worker := agent.New(teamloader.WorkflowAgentName("gordon", "answer"), "Answer the request.", agent.WithModel(&scriptedProvider{
		id: "test/workflow", contextSize: 10000,
		scripts: [][]chat.MessageStreamResponse{contentScript("Redis is running.", 10, 5), contentScript("Persistence uses a volume.", 20, 5)},
	}))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(worker)),
		runtime.WithModelStore(stubModelStore{}),
		runtime.WithWorkflowRunner(workflow.Runner("gordon", graphs, map[string]map[string]evaluator.Evaluator{"gordon": {"route": router}})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	workingDir := t.TempDir()
	application := app.New(t.Context(), rt, session.New(session.WithWorkingDir(workingDir)))
	d := tuitest.New(t, tui.New(t.Context(), nil, application, workingDir, func() {}), 120, 40)
	d.Type("Run Redis").Enter().WaitFor(tuitest.Contains("Redis is running."))
	d.Type("And persistence?").Enter().WaitFor(tuitest.Contains("Persistence uses a volume."))
	router.mu.Lock()
	defer router.mu.Unlock()
	require.Len(t, router.states, 2)
	assert.Equal(t, "And persistence?", router.states[1]["input"])
	assert.Contains(t, router.states[1]["conversation"], "Run Redis")
	assert.Contains(t, router.states[1]["conversation"], "Redis is running.")
}

type workflowToolset struct{ tool tools.Tool }

func (s workflowToolset) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{s.tool}, nil
}

func TestWorkflowApprovalAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "approve", true: "cancel"}[cancelRun], func(t *testing.T) {
			isolateState(t)
			graphs := map[string]latest.WorkflowConfig{"gordon": {Entry: "answer", Nodes: map[string]latest.WorkflowNode{"answer": {Type: "agent"}}}}
			var calls atomic.Int32
			cancelled := make(chan struct{})
			tool := tools.Tool{Name: "workflow_action", Description: "Perform a test action", Parameters: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				calls.Add(1)
				if cancelRun {
					<-ctx.Done()
					close(cancelled)
					return nil, ctx.Err()
				}
				return tools.ResultSuccess("approved"), nil
			}}
			worker := agent.New(teamloader.WorkflowAgentName("gordon", "answer"), "Answer", agent.WithToolSets(workflowToolset{tool: tool}), agent.WithModel(&scriptedProvider{
				id: "test/workflow", contextSize: 10000,
				scripts: [][]chat.MessageStreamResponse{toolCallScript("call", "workflow_action", "{}", 10, 5), contentScript("Action completed.", 20, 5)},
			}))
			rt, err := runtime.New(t.Context(), team.New(team.WithAgents(worker)), runtime.WithModelStore(stubModelStore{}), runtime.WithWorkflowRunner(workflow.Runner("gordon", graphs, nil)))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			workingDir := t.TempDir()
			application := app.New(t.Context(), rt, session.New(session.WithWorkingDir(workingDir), session.WithSafetyPolicy(session.SafetyPolicyStrict)))
			d := tuitest.New(t, tui.New(t.Context(), nil, application, workingDir, func() {}), 120, 40)
			d.Type("Perform the action").Enter().WaitFor(tuitest.Contains("always allow workflow_action"))
			assert.Zero(t, calls.Load(), "the workflow must wait for interactive approval")
			d.Type("y")
			if cancelRun {
				require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, time.Millisecond)
				d.Press(tea.KeyEscape).WaitFor(tuitest.Contains("Stop the current response?")).Type("y")
				select {
				case <-cancelled:
				case <-time.After(5 * time.Second):
					t.Fatal("Escape did not cancel the workflow tool")
				}
				d.Assert(tuitest.Absent("Action completed."))
			} else {
				d.WaitFor(tuitest.Contains("Action completed."))
			}
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}
