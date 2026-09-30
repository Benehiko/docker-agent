package root

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/workflow"
)

type workflowTestProvider struct {
	answer string
	onCall func([]chat.Message)
}

func (p workflowTestProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/model") }
func (p workflowTestProvider) BaseConfig() base.Config { return base.Config{} }
func (p workflowTestProvider) MaxTokens() int          { return 0 }
func (p workflowTestProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	if p.onCall != nil {
		p.onCall(messages)
	}
	return &workflowTestStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: p.answer}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}},
	}}, nil
}

type workflowTestStream struct {
	responses []chat.MessageStreamResponse
	index     int
}

func (s *workflowTestStream) Recv() (chat.MessageStreamResponse, error) {
	if s.index >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[s.index]
	s.index++
	return response, nil
}
func (s *workflowTestStream) Close() {}

type workflowTestEvaluator struct{}

func (workflowTestEvaluator) Evaluate(context.Context, any) (*evaluator.Result, error) {
	return &evaluator.Result{Type: "choice", Model: "jev", Choice: "assistant2", Probabilities: map[string]float64{"assistant2": 0.9, "quick": 0.1}}, nil
}

func TestWorkflowCLIExecutesNestedReturn(t *testing.T) {
	model := new("test")
	wfs := map[string]latest.WorkflowConfig{
		"assistant": {Entry: "router", Nodes: map[string]latest.WorkflowNode{
			"router":     {Type: "decision", Evaluator: "route", DefaultNode: "quick", AllowedNodes: []string{"quick", "assistant2"}},
			"quick":      {Type: "agent", Model: model, Instruction: model},
			"assistant2": {Type: "workflow", Workflow: "child", Next: "finish"},
			"finish":     {Type: "agent", Model: model, Instruction: model},
		}},
		"child": {Entry: "research", Nodes: map[string]latest.WorkflowNode{
			"research": {Type: "agent", Model: model, Instruction: model, Next: "review"},
			"review":   {Type: "agent", Model: model, Instruction: model},
		}},
	}
	agents := []*agent.Agent{agent.New("root", "root", agent.WithModel(workflowTestProvider{answer: "root"}))}
	for name, wf := range wfs {
		for id, n := range wf.Nodes {
			if n.Type == "agent" {
				agents = append(agents, agent.New(teamloader.WorkflowAgentName(name, id), "test", agent.WithModel(workflowTestProvider{answer: id + "-answer"})))
			}
		}
	}
	loaded := &teamloader.LoadResult{Team: team.New(team.WithAgents(agents...)), Workflows: wfs, Routers: map[string]map[string]evaluator.Evaluator{"assistant": {"router": workflowTestEvaluator{}}}}
	rt, err := runtime.NewLocalRuntime(t.Context(), loaded.Team)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyRestricted), session.WithWorkingDir(t.TempDir()))
	var buf bytes.Buffer
	f := &runExecFlags{workflowName: "assistant"}
	err = f.handleWorkflow(t.Context(), cli.NewPrinter(&buf), rt, sess, loaded, []string{"workflow.yaml", "request"})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "finish-answer")
	assert.Contains(t, buf.String(), "assistant/router [route, model jev]: assistant2 -> assistant2")
	assert.Len(t, sess.MessagesSnapshot(), 4)
}

type recordingWorkflowEvaluator struct {
	states []map[string]string
}

func (e *recordingWorkflowEvaluator) Evaluate(_ context.Context, state any) (*evaluator.Result, error) {
	e.states = append(e.states, state.(map[string]string))
	return &evaluator.Result{Type: "choice", Choice: "answer", Probabilities: map[string]float64{"answer": 0.95, "other": 0.05}}, nil
}

func TestWorkflowNativeStreamFollowUps(t *testing.T) {
	for _, mode := range []string{"next turn", "follow up", "steer"} {
		t.Run(mode, func(t *testing.T) {
			router := &recordingWorkflowEvaluator{}
			graphs := map[string]latest.WorkflowConfig{"gordon": {Entry: "route", Nodes: map[string]latest.WorkflowNode{
				"route":  {Type: "decision", AllowedNodes: []string{"answer", "other"}, DefaultNode: "other"},
				"answer": {Type: "agent"}, "other": {Type: "agent"},
			}}}
			var prompts [][]chat.Message
			var rt *runtime.LocalRuntime
			var once sync.Once
			worker := agent.New(teamloader.WorkflowAgentName("gordon", "answer"), "instructions", agent.WithModel(workflowTestProvider{answer: "Redis answer", onCall: func(messages []chat.Message) {
				prompts = append(prompts, messages)
				once.Do(func() {
					switch mode {
					case "follow up":
						assert.NoError(t, rt.FollowUp(t.Context(), runtime.QueuedMessage{Content: "And persistence?"}))
					case "steer":
						assert.NoError(t, rt.Steer(t.Context(), runtime.QueuedMessage{Content: "And persistence?"}))
					}
				})
			}}))
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			rt, err = runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)), runtime.WithSessionStore(store), runtime.WithWorkflowRunner(workflow.Runner("gordon", graphs, map[string]map[string]evaluator.Evaluator{"gordon": {"route": router}})))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			sess := session.New(session.WithUserMessage("How do I run Redis?"), session.WithWorkingDir(t.TempDir()))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var starts, stops, answers int
			drain := func() {
				for event := range rt.RunStream(ctx, sess) {
					switch e := event.(type) {
					case *runtime.StreamStartedEvent:
						if e.SessionID == sess.ID {
							starts++
						}
					case *runtime.StreamStoppedEvent:
						if e.SessionID == sess.ID {
							stops++
							assert.Equal(t, "normal", e.Reason)
						}
					case *runtime.AgentChoiceEvent:
						answers++
					case *runtime.ErrorEvent:
						t.Errorf("workflow error: %s", e.Error)
					}
				}
			}
			drain()
			if mode == "next turn" {
				sess, err = store.GetSession(ctx, sess.ID)
				require.NoError(t, err)
				sess.AddMessage(session.UserMessage("And persistence?"))
				drain()
			}
			require.Len(t, router.states, 2, "each follow-up must go through routing")
			assert.Equal(t, "And persistence?", router.states[1]["input"])
			assert.Contains(t, router.states[1]["conversation"], "How do I run Redis?")
			assert.Contains(t, router.states[1]["conversation"], "Redis answer")
			require.Len(t, prompts, 2)
			var secondPrompt strings.Builder
			for _, msg := range prompts[1] {
				secondPrompt.WriteString(msg.Content)
			}
			assert.Contains(t, secondPrompt.String(), "How do I run Redis?")
			assert.Contains(t, secondPrompt.String(), "Redis answer")
			assert.Equal(t, starts, stops)
			assert.Equal(t, 2, answers, "final answers should not be rendered twice")
			assert.Equal(t, "Redis answer", sess.GetLastAssistantMessageContent())
		})
	}
}
