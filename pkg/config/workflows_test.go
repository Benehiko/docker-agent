package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestWorkflowOnlyConfigAndInlineModels(t *testing.T) {
	const source = `evaluators:
  route: {provider: typesafe, model: jev, type: choice, instructions: Choose}
workflows:
  project:
    entry: route
    nodes:
      route: {type: decision, evaluator: route, allowed_nodes: [first, second], default_node: first}
      first: {type: agent, model: openai/gpt-5-mini, instruction: test, description: First}
      second: {type: agent, model: openai/gpt-5-mini, instruction: test, description: Second}
`
	c, err := Load(t.Context(), NewBytesSource("workflow.yaml", []byte(source)))
	require.NoError(t, err)
	require.Empty(t, c.Agents)
	require.Contains(t, c.Models, "openai/gpt-5-mini")
	assert.Equal(t, latest.Version, c.Version)
	jsonData, err := json.Marshal(c)
	require.NoError(t, err)
	fromJSON, err := Load(t.Context(), NewBytesSource("workflow.json", jsonData))
	require.NoError(t, err)
	assert.Equal(t, c.Workflows["project"].Entry, fromJSON.Workflows["project"].Entry)
	r := Requires(c)
	assert.Equal(t, []string{"workflows.project"}, r.Features[FeatureWorkflows])
	assert.Equal(t, []string{"evaluators.route"}, r.Features[FeatureEvaluators])
	_, err = Load(t.Context(), NewBytesSource("bad.yaml", []byte(strings.Replace(source, "[first, second]", "[first, missing]", 1))))
	require.ErrorContains(t, err, "destination")
}

func TestWorkflowToolsetReferences(t *testing.T) {
	const source = `models: {main: {provider: openai, model: gpt-5-mini}}
mcps:
  reusable: {type: mcp, command: echo}
workflows:
  work:
    entry: child
    nodes:
      base: {type: agent, abstract: true, model: main, instruction: test, toolsets: [{type: mcp, ref: reusable}]}
      child: {inherits: base}
`
	cfg, err := Load(t.Context(), NewBytesSource("workflow.yaml", []byte(source)))
	require.NoError(t, err)
	resolved, err := ResolveWorkflowToolsets(cfg)
	require.NoError(t, err)
	require.NotNil(t, resolved["work"].Nodes["child"].Toolsets)
	assert.Equal(t, "echo", (*resolved["work"].Nodes["child"].Toolsets)[0].Command)
	assert.Equal(t, "reusable", (*cfg.Workflows["work"].Nodes["base"].Toolsets)[0].Ref)
	_, err = Load(t.Context(), NewBytesSource("workflow.yaml", []byte(strings.Replace(source, "ref: reusable", "ref: unknown", 1))))
	require.ErrorContains(t, err, "unknown MCP definition")
}

func TestHCLRejectsWorkflow(t *testing.T) {
	_, err := hcl.ToYAML([]byte(`workflow "w" { entry = "a" }`), "w.hcl")
	require.ErrorContains(t, err, "not supported in HCL")
}
