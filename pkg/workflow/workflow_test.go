package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/session"
)

type stubRunner struct {
	mu    sync.Mutex
	calls []string
	fail  string
}

func (r *stubRunner) RunAgent(_ context.Context, wf, node, input, output string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%s/%s:%s:%s", wf, node, input, output))
	if r.fail == node {
		return "", errors.New("step failed")
	}
	return node + "-answer", nil
}

type stubEvaluator struct {
	result *evaluator.Result
	err    error
	state  any
}

func (s *stubEvaluator) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	s.state = state
	return s.result, s.err
}

func testGraph(client evaluator.Evaluator, runner AgentRunner) Executor {
	text := new("test")
	return Executor{
		Workflows: map[string]latest.WorkflowConfig{
			"parent": {Entry: "router", Nodes: map[string]latest.WorkflowNode{
				"router":     {Type: "decision", Evaluator: "route", AllowedNodes: []string{"quick", "assistant2"}, DefaultNode: "quick"},
				"quick":      {Type: "agent", Instruction: text, Model: text},
				"assistant2": {Type: "workflow", Workflow: "child", Next: "finish"},
				"finish":     {Type: "agent", Instruction: text, Model: text},
			}},
			"child": {Entry: "research", Nodes: map[string]latest.WorkflowNode{
				"research": {Type: "agent", Next: "review"}, "review": {Type: "agent"},
			}},
		}, Routers: map[string]map[string]evaluator.Evaluator{"parent": {"router": client}}, Agents: runner,
	}
}

func TestNestedRouteAndReturn(t *testing.T) {
	runner := &stubRunner{}
	router := &stubEvaluator{result: &evaluator.Result{Type: "choice", Choice: "assistant2", Model: "jev", Probabilities: map[string]float64{"assistant2": 0.91, "quick": 0.09}}}
	exec := testGraph(router, runner)
	var routes []Route
	exec.OnRoute = func(r Route) { routes = append(routes, r) }
	answer, err := exec.Run(t.Context(), "parent", "request")
	require.NoError(t, err)
	assert.Equal(t, "finish-answer", answer)
	assert.Equal(t, []string{"child/research:request:request", "child/review:request:research-answer", "parent/finish:request:review-answer"}, runner.calls)
	assert.Equal(t, map[string]string{"input": "request", "output": "request"}, router.state)
	require.Len(t, routes, 1)
	assert.Equal(t, "assistant2", routes[0].Destination)
	assert.Equal(t, "jev", routes[0].Model)
}

func TestRoutesAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, choice string
		probs        map[string]float64
		err          error
		want         string
	}{
		{"quick", "quick", map[string]float64{"quick": 0.85, "assistant2": 0.15}, nil, "quick"},
		{"low", "assistant2", map[string]float64{"quick": 0.2, "assistant2": 0.8}, nil, "quick"},
		{"tie", "assistant2", map[string]float64{"quick": 0.5, "assistant2": 0.5}, nil, "quick"},
		{"unknown", "unknown", map[string]float64{"unknown": 1}, nil, "quick"},
		{"missing distribution", "assistant2", map[string]float64{"assistant2": 0.9}, nil, "quick"},
		{"extra outcome", "assistant2", map[string]float64{"assistant2": 0.9, "quick": 0.05, "extra": 0.05}, nil, "quick"},
		{"provider failure", "", nil, errors.New("unavailable"), "quick"},
		{"malformed", "", nil, nil, "quick"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &stubRunner{}
			client := &stubEvaluator{err: tc.err}
			if tc.name != "malformed" {
				client.result = &evaluator.Result{Type: "choice", Choice: tc.choice, Probabilities: tc.probs}
			}
			exec := testGraph(client, runner)
			var route Route
			exec.OnRoute = func(r Route) { route = r }
			answer, err := exec.Run(t.Context(), "parent", "prompt")
			require.NoError(t, err)
			if tc.want == "quick" {
				assert.Equal(t, "quick-answer", answer)
			} else {
				assert.Equal(t, "finish-answer", answer)
			}
			assert.Equal(t, tc.want, route.Destination)
			if tc.name != "quick" {
				assert.NotEmpty(t, route.FallbackReason)
			}
		})
	}
	runner := &stubRunner{fail: "quick"}
	exec := testGraph(&stubEvaluator{err: errors.New("unavailable")}, runner)
	_, err := exec.Run(t.Context(), "parent", "prompt")
	require.ErrorContains(t, err, "step failed")
	assert.Len(t, runner.calls, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = exec.Run(ctx, "parent", "prompt")
	require.ErrorIs(t, err, context.Canceled)
}

type cancelAfterRunner struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancelAfterRunner) RunAgent(_ context.Context, _, _, _, _ string) (string, error) {
	r.calls++
	r.cancel()
	return "partial", nil
}

func TestWorkflowCancellationBetweenChildSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	runner := &cancelAfterRunner{cancel: cancel}
	exec := testGraph(&stubEvaluator{result: &evaluator.Result{Type: "choice", Choice: "assistant2", Probabilities: map[string]float64{"assistant2": 0.9, "quick": 0.1}}}, runner)
	_, err := exec.Run(ctx, "parent", "task")
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, runner.calls)
}

func TestConcurrentWorkflowRunsAreIndependent(t *testing.T) {
	client := workflowEvaluatorFunc(func(context.Context, any) (*evaluator.Result, error) {
		return &evaluator.Result{Type: "choice", Choice: "quick", Probabilities: map[string]float64{"quick": 0.9, "assistant2": 0.1}}, nil
	})
	runner := &stubRunner{}
	exec := testGraph(client, runner)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			answer, err := exec.Run(t.Context(), "parent", fmt.Sprintf("input-%d", i))
			require.NoError(t, err)
			assert.Equal(t, "quick-answer", answer)
		})
	}
	wg.Wait()
	assert.Len(t, runner.calls, 20)
}

type workflowEvaluatorFunc func(context.Context, any) (*evaluator.Result, error)

func (f workflowEvaluatorFunc) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return f(ctx, state)
}

func TestRuntimeBounds(t *testing.T) {
	runner := &stubRunner{}
	exec := testGraph(&stubEvaluator{}, runner)
	node := exec.Workflows["parent"].Nodes["quick"]
	node.Next = "quick"
	wf := exec.Workflows["parent"]
	wf.Nodes["quick"] = node
	exec.Workflows["parent"] = wf
	_, err := exec.Run(t.Context(), "parent", "prompt")
	require.ErrorContains(t, err, "100")
	assert.Len(t, runner.calls, 99)
	wf = exec.Workflows["parent"]
	wf.Entry = "assistant2"
	exec.Workflows["parent"] = wf
	child := exec.Workflows["child"]
	child.Nodes["research"] = latest.WorkflowNode{Type: "workflow", Workflow: "child"}
	exec.Workflows["child"] = child
	_, err = exec.Run(t.Context(), "parent", "prompt")
	require.ErrorContains(t, err, "8")
}

func TestConversationInputExcludesInternalDataAndRetryOutput(t *testing.T) {
	sess := session.New(session.WithSystemMessage("private system instructions"), session.WithUserMessage("first question"))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first answer"}))
	sess.AddMessage(session.UserMessage("follow up"))
	child := session.New(session.WithImplicitUserMessage("internal task data"))
	child.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "partial failed answer"}))
	child.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, Content: "private tool output"}})
	sess.AddLiveSubSession(child)
	input, conversation := conversationInput(sess)
	assert.Equal(t, "follow up", input)
	assert.JSONEq(t, `[{"role":"user","content":"first question"},{"role":"assistant","content":"first answer"}]`, conversation)
}
