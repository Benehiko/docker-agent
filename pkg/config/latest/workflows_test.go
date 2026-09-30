package latest

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const workflowFixture = `models:
  base: {provider: openai, model: gpt-5-mini}
evaluators:
  route: {provider: typesafe, model: jev, type: choice, instructions: Choose a node}
workflows:
  primary:
    entry: router
    nodes:
      base: {type: agent, abstract: true, model: base, instruction: original, toolsets: [{type: filesystem}]}
      first: {inherits: base, description: First}
      second: {inherits: first, instruction: "", toolsets: [], description: Second}
      router: {type: decision, evaluator: route, allowed_nodes: [first, second], default_node: first}
`

func TestWorkflowInheritanceAndChoices(t *testing.T) {
	var c Config
	require.NoError(t, yaml.UnmarshalWithOptions([]byte(workflowFixture), &c, yaml.Strict()))
	resolved, err := c.ResolvedWorkflows()
	require.NoError(t, err)
	first := resolved["primary"].Nodes["first"]
	second := resolved["primary"].Nodes["second"]
	assert.Equal(t, "base", *second.Model)
	assert.Empty(t, *second.Instruction)
	require.NotNil(t, second.Toolsets)
	assert.Empty(t, *second.Toolsets)
	assert.Len(t, *first.Toolsets, 1)
	assert.True(t, resolved["primary"].Nodes["base"].Abstract)
	assert.Equal(t, map[string]string{"first": "First", "second": "Second"}, c.RouterEvaluator(resolved["primary"], resolved["primary"].Nodes["router"]).Choices)
	assert.Nil(t, c.Evaluators["route"].Choices)
	data, err := json.Marshal(c)
	require.NoError(t, err)
	var fromJSON Config
	require.NoError(t, json.Unmarshal(data, &fromJSON))
	_, err = fromJSON.ResolvedWorkflows()
	require.NoError(t, err)
	assert.Empty(t, *fromJSON.Workflows["primary"].Nodes["second"].Toolsets)
}

func TestWorkflowUnknownField(t *testing.T) {
	var c Config
	err := yaml.UnmarshalWithOptions([]byte(strings.Replace(workflowFixture, "description: First", "description: First, hallucinated: true", 1)), &c, yaml.Strict())
	require.ErrorContains(t, err, "hallucinated")
}

func TestWorkflowInvalidGraphs(t *testing.T) {
	cases := []struct{ name, from, to, want string }{
		{"unknown parent", "inherits: base", "inherits: missing", "unknown node"},
		{"inheritance cycle", "base: {type: agent, abstract: true, model: base, instruction: original, toolsets: [{type: filesystem}]}", "base: {type: agent, inherits: first}", "inheritance cycle"},
		{"abstract target", "allowed_nodes: [first, second]", "allowed_nodes: [base, second]", "invalid or duplicate destination"},
		{"duplicate target", "allowed_nodes: [first, second]", "allowed_nodes: [first, first]", "invalid or duplicate destination"},
		{"unknown transition", "description: First}", "description: First, next: missing}", "invalid next"},
		{"flow cycle", "description: First}", "description: First, next: router}", "control-flow cycle"},
		{"invalid probability", "default_node: first}", "default_node: first, min_probability: 0}", "min_probability"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			err := yaml.UnmarshalWithOptions([]byte(replaceOne(workflowFixture, tc.from, tc.to)), &c, yaml.Strict())
			require.ErrorContains(t, err, tc.want)
		})
	}
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte(workflowFixture), &c))
	wf := c.Workflows["primary"]
	wf.Nodes["child"] = WorkflowNode{Type: "workflow", Workflow: "primary"}
	c.Workflows["primary"] = wf
	_, err := c.ResolvedWorkflows()
	require.ErrorContains(t, err, "recursive workflow")
	threshold := math.NaN()
	wf.Nodes["router"] = WorkflowNode{Type: "decision", Evaluator: "route", AllowedNodes: []string{"first", "second"}, DefaultNode: "first", MinProbability: &threshold}
	c.Workflows["primary"] = wf
	_, err = c.ResolvedWorkflows()
	require.ErrorContains(t, err, "min_probability")
}

func replaceOne(s, from, to string) string { return strings.Replace(s, from, to, 1) }
