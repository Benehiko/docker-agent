package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
	evaluatorprovider "github.com/docker/docker-agent/pkg/evaluator/provider"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type workflowStubEvaluator struct {
	result *evaluator.Result
	err    error
}

func (s workflowStubEvaluator) Evaluate(context.Context, any) (*evaluator.Result, error) {
	return s.result, s.err
}

func TestWorkflowEvaluatorSharesBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"jev","answers":[],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	t.Cleanup(server.Close)
	client, err := evaluatorprovider.New(t.Context(), latest.EvaluatorConfig{Provider: "typesafe", Type: "choice", Model: "jev", Instructions: "Choose", BaseURL: server.URL, Choices: map[string]string{"a": "A", "b": "B"}}, environment.NewMapEnvProvider(map[string]string{"TYPESAFE_API_KEY": "test"}))
	require.NoError(t, err)
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithBudget(&latest.BudgetConfig{MaxTokens: 5}), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	sess := session.New()
	sink := &collectSink{}
	_, err = rt.EvaluateWorkflow(t.Context(), sess, "root", "router", client, map[string]string{"input": "hello"}, sink)
	var exceeded *WorkflowBudgetError
	require.ErrorAs(t, err, &exceeded)
	require.Len(t, evaluationItems(sess), 1)
	assert.Equal(t, int64(3), evaluationItems(sess)[0].Usage.InputTokens)
	assert.Nil(t, evaluationItems(sess)[0].Cost, "unknown spend must remain unknown")
	err = rt.WorkflowBudget("root")
	require.ErrorAs(t, err, &exceeded)
}

func TestWorkflowEvaluatorCancellationNeverFallsBack(t *testing.T) {
	rt, _ := newTestRuntime(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := rt.EvaluateWorkflow(ctx, session.New(), "root", "route", workflowStubEvaluator{err: errors.New("failed")}, nil, &collectSink{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestWorkflowStepIsPinnedAndPropagatesPolicy(t *testing.T) {
	root := agent.New("root", "default", agent.WithModel(&mockProvider{id: "test/model"}))
	worker := agent.New("worker", "answer", agent.WithModel(&mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("answer").AddStopWithUsage(1, 1).Build()}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	parent := session.New(session.WithSafetyPolicy(session.SafetyPolicyRestricted), session.WithWorkingDir(t.TempDir()))
	events := &collectSink{}
	answer, err := rt.RunWorkflowStep(t.Context(), parent, "worker", "task", events)
	require.NoError(t, err)
	assert.NotEmpty(t, answer)
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()))
	children := parent.MessagesSnapshot()
	require.Len(t, children, 1)
	child := children[0].SubSession
	require.NotNil(t, child)
	assert.Equal(t, "worker", child.AgentName)
	assert.Equal(t, parent.WorkingDir, child.WorkingDir)
	assert.Equal(t, session.SafetyPolicyRestricted, child.GetSafetyPolicy())
	assert.Equal(t, parent.NonInteractive, child.NonInteractive)
}

func TestWorkflowInteractiveApprovals(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		response                                         ResumeRequest
		policy                                           session.SafetyPolicy
		nonInteractive, deny, guard, wantPrompt, wantRun bool
	}{
		{name: "approve", response: ResumeApprove(), wantPrompt: true, wantRun: true},
		{name: "reject", response: ResumeReject("not allowed"), wantPrompt: true},
		{name: "session approval", response: ResumeApproveAutonomous(), wantPrompt: true, wantRun: true},
		{name: "deny overrides autonomous", policy: session.SafetyPolicyAutonomous, deny: true},
		{name: "guard overrides autonomous", policy: session.SafetyPolicyAutonomous, guard: true},
		{name: "headless fails closed", nonInteractive: true},
		{name: "restricted fails closed", policy: session.SafetyPolicyRestricted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var executed bool
			prov := &queueProvider{id: "test/model", streams: []chat.MessageStream{
				newStreamBuilder().AddToolCallName("call", "the_tool").AddToolCallArguments("call", "{}").AddToolCallStopWithUsage(1, 1).Build(),
				newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(),
			}}
			worker := agent.New("worker", "test", agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, recordingTool("the_tool", &executed), nil)))
			reg := hooks.NewRegistry()
			if tc.guard {
				worker = agent.New("worker", "test", agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, recordingTool("the_tool", &executed), nil)), agent.WithHooks(&latest.HooksConfig{ToolGuard: preToolUseHooksConfig("deny").PreToolUse}))
				require.NoError(t, reg.RegisterBuiltin("deny", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
					return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{HookEventName: hooks.EventToolGuard, PermissionDecision: hooks.DecisionDeny, PermissionDecisionReason: "safety guard"}}, nil
				}))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)), WithModelStore(mockModelStore{}), WithHooksRegistry(reg))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			parent := session.New(session.WithNonInteractive(tc.nonInteractive), session.WithSafetyPolicy(tc.policy), session.WithWorkingDir(t.TempDir()))
			if tc.deny {
				parent.SetPermissions(&session.PermissionsConfig{Deny: []string{"the_tool"}})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var prompted bool
			sink := EventSinkFunc(func(ev Event) {
				if _, ok := ev.(*ToolCallConfirmationEvent); ok {
					prompted = true
					rt.Resume(ctx, tc.response)
				}
			})
			_, err = rt.RunWorkflowStep(ctx, parent, "worker", "task", sink)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPrompt, prompted)
			assert.Equal(t, tc.wantRun, executed)
			if tc.name == "session approval" {
				assert.Equal(t, session.SafetyPolicyAutonomous, parent.GetSafetyPolicy())
			}
		})
	}
}

func TestWorkflowRootCancellation(t *testing.T) {
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model"}))
	entered := make(chan struct{})
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}), WithWorkflowRunner(func(ctx context.Context, _ *LocalRuntime, _ *session.Session, _ EventSink) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sess := session.New(session.WithUserMessage("hello"))
	events := rt.RunStream(ctx, sess)
	<-entered
	cancel()
	var starts, stops int
	for event := range events {
		switch e := event.(type) {
		case *StreamStartedEvent:
			starts++
		case *StreamStoppedEvent:
			stops++
			assert.Equal(t, sess.ID, e.SessionID)
			assert.Equal(t, "canceled", e.Reason)
		}
	}
	assert.Equal(t, 1, starts)
	assert.Equal(t, 1, stops)
}
