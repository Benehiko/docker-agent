package teamloader

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestWorkflowRouterBindingsAreIndependent(t *testing.T) {
	var mu sync.Mutex
	choices := []map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		choices = append(choices, req.Questions["evaluation"].Criteria)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"model":"jev","answers":{"evaluation":{"type":"choice","choice":"a","probabilities":{"a":0.9,"b":0.1}}},"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	source := config.NewBytesSource("workflows.yaml", []byte(`
models: {main: {provider: openai, model: gpt-5-mini}}
evaluators:
  route: {provider: typesafe, model: jev, type: choice, instructions: Choose, base_url: `+server.URL+`}
workflows:
  first:
    entry: router
    nodes:
      router: {type: decision, evaluator: route, allowed_nodes: [a, b], default_node: a}
      a: {type: agent, model: main, instruction: test, description: Alpha}
      b: {type: agent, model: main, instruction: test, description: Beta}
  second:
    entry: router
    nodes:
      router: {type: decision, evaluator: route, allowed_nodes: [a, b], default_node: a}
      a: {type: agent, model: main, instruction: test, description: Apple}
      b: {type: agent, model: main, instruction: test, description: Banana}
`))
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "fake-chat-key", "TYPESAFE_API_KEY": "fake-route-key"})}
	loaded, err := LoadWithConfig(t.Context(), source, rc, withTestProviderRegistry(WithWorkflows(), WithStrict(config.FeatureWorkflows, config.FeatureEvaluators))...)
	require.NoError(t, err)
	for _, name := range []string{"first", "second"} {
		_, err = loaded.Routers[name]["router"].Evaluate(t.Context(), map[string]string{"input": "test", "output": "test"})
		require.NoError(t, err)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []map[string]string{{"a": "Alpha", "b": "Beta"}, {"a": "Apple", "b": "Banana"}}, choices)
	assert.Len(t, loaded.Team.AgentNames(), 4)
}

func TestWorkflowDeclarationsDoNotExposeInternalAgents(t *testing.T) {
	source := config.NewBytesSource("mixed.yaml", []byte(`models: {main: {provider: openai, model: gpt-5-mini}}
evaluators:
  route: {provider: typesafe, model: jev, type: choice, instructions: Choose}
agents:
  root: {model: main, instruction: test}
workflows:
  w:
    entry: router
    nodes:
      router: {type: decision, evaluator: route, allowed_nodes: [a, b], default_node: a}
      a: {type: agent, model: main, instruction: test, description: Alpha}
      b: {type: agent, model: main, instruction: test, description: Beta}
`))
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})}
	loaded, err := LoadWithConfig(t.Context(), source, rc, withTestProviderRegistry(WithWorkflowDeclarations())...)
	require.NoError(t, err)
	assert.Equal(t, []string{"root"}, loaded.Team.AgentNames())
	assert.Empty(t, loaded.Routers)
	assert.Contains(t, loaded.Workflows, "w")
}

func TestWorkflowUnsupportedWithoutCapability(t *testing.T) {
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()}
	source := config.NewBytesSource("w.yaml", []byte(`models: {main: {provider: openai, model: gpt-5-mini}}
workflows:
  w:
    entry: a
    nodes:
      a: {type: agent, model: main, instruction: test}
`))
	_, err := LoadWithConfig(t.Context(), source, rc, withTestProviderRegistry()...)
	require.ErrorContains(t, err, "require a local CLI run")
}

func TestWorkflowStrictCapabilityBeforeCredentials(t *testing.T) {
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewNoEnvProvider()}
	source := config.NewBytesSource("w.yaml", []byte(`models: {main: {provider: openai, model: gpt-5-mini}}
workflows:
  w:
    entry: a
    nodes:
      a: {type: agent, model: main, instruction: test}
`))
	_, err := LoadWithConfig(t.Context(), source, rc, withTestProviderRegistry(WithWorkflows(), WithStrict())...)
	require.ErrorContains(t, err, `feature "workflows"`)
}

func TestAutoWorkflowPreparesSoleWorkflow(t *testing.T) {
	source := config.NewBytesSource("workflow.yaml", []byte(`models: {main: {provider: openai, model: gpt-5-mini}}
workflows:
  gordon:
    entry: answer
    nodes:
      answer: {type: agent, model: main, instruction: test}
`))
	rc := &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})}
	loaded, err := LoadWithConfig(t.Context(), source, rc, withTestProviderRegistry(WithAutoWorkflow())...)
	require.NoError(t, err)
	assert.Equal(t, []string{WorkflowAgentName("gordon", "answer")}, loaded.Team.AgentNames())
}
