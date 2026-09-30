package root

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func TestWorkflowFlagRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no agent", []string{"--exec", "--workflow", "assistant", "--agent", "root", "./agent.yaml", "prompt"}, "--workflow cannot be combined"},
		{"no resume", []string{"--exec", "--workflow", "assistant", "--session", "-1", "./agent.yaml", "prompt"}, "--workflow cannot be combined"},
		{"no picker", []string{"--exec", "--workflow", "assistant", "--agent-picker", "./agent.yaml", "prompt"}, "--workflow cannot be combined"},
		{"no remote", []string{"--exec", "--workflow", "assistant", "--remote", "http://localhost", "./agent.yaml", "prompt"}, "--workflow cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRunCmd()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestWorkflowAgentNames(t *testing.T) {
	cmd := newRunCmd()
	assert.NotNil(t, cmd.Flags().Lookup("workflow"))
}

func TestWorkflowSelection(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		workflows                      []string
		agent, selected, want, wantErr string
		ordinaryAgent                  bool
	}{
		{name: "sole workflow", workflows: []string{"gordon"}, want: "gordon"},
		{name: "sole workflow with ordinary root", workflows: []string{"gordon"}, ordinaryAgent: true, want: "gordon"},
		{name: "explicit agent", workflows: []string{"gordon"}, agent: "root", ordinaryAgent: true},
		{name: "ambiguous", workflows: []string{"a", "b"}, wantErr: "multiple workflows"},
		{name: "explicit workflow", workflows: []string{"a", "b"}, selected: "b", want: "b"},
		{name: "unknown", workflows: []string{"a"}, selected: "missing", wantErr: "not found"},
		{name: "ordinary config", ordinaryAgent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := &teamloader.LoadResult{Team: team.New(), Workflows: map[string]latest.WorkflowConfig{}}
			if tc.ordinaryAgent {
				loaded.Team = team.New(team.WithAgents(agent.New("root", "test")))
			}
			for _, name := range tc.workflows {
				loaded.Workflows[name] = latest.WorkflowConfig{}
			}
			flags := &runExecFlags{agentName: tc.agent, workflowName: tc.selected}
			err := flags.selectWorkflow(loaded)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, flags.workflowName)
		})
	}
}
