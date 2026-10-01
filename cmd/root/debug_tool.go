package root

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
)

func (f *debugFlags) runDebugToolCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := backgroundjobs.WithoutBackgroundJobs(cmd.Context())
	// Tool parameters may contain secrets; keep them out of command telemetry.
	telemetry.TrackCommand(ctx, "debug", []string{"tool"})
	defer func() {
		if commandErr != nil {
			telemetry.TrackCommandError(ctx, "debug", []string{"tool"}, errors.New("tool invocation failed"))
		}
	}()

	arguments := "{}"
	if len(args) == 3 {
		arguments = args[2]
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &params); err != nil {
		return fmt.Errorf("parameters must be a JSON object: %w", err)
	}
	if params == nil {
		return errors.New("parameters must be a JSON object, not null")
	}

	t, err := f.loadTeam(ctx, args[0])
	if err != nil {
		return err
	}
	defer stopToolSets(ctx, t)

	agent, err := t.AgentOrDefault(f.toolAgent)
	if err != nil {
		return err
	}

	// Include deferred tools: activation would otherwise be lost between CLI calls.
	available, err := agent.ToolsWithCatalog(ctx)
	for _, warning := range agent.DrainWarnings() {
		fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", warning)
	}
	if err != nil {
		return fmt.Errorf("listing tools for agent %q: %w", agent.Name(), err)
	}

	index := slices.IndexFunc(available, func(tool tools.Tool) bool { return tool.Name == args[1] })
	if index < 0 {
		return fmt.Errorf("tool %q not found for agent %q; use 'debug toolsets --json' to list tools", args[1], agent.Name())
	}

	result, err := callDebugTool(ctx, available[index], arguments)
	if err != nil {
		return err
	}

	if f.toolJSON {
		err = encodeJSON(cmd, result)
	} else {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Output)
	}
	if err != nil {
		return err
	}
	if result.IsError {
		return fmt.Errorf("tool %q returned an error for agent %q", args[1], agent.Name())
	}
	return nil
}

func callDebugTool(ctx context.Context, tool tools.Tool, arguments string) (*tools.ToolCallResult, error) {
	if tool.RuntimeHandler != "" || tool.Handler == nil {
		return nil, fmt.Errorf("tool %q requires an agent runtime and cannot be called directly", tool.Name)
	}
	toolCall := tools.ToolCall{
		ID:   "debug_" + tool.Name,
		Type: "function",
		Function: tools.FunctionCall{
			Name:      tool.Name,
			Arguments: arguments,
		},
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := tool.Handler(ctx, toolCall, tools.NopRuntime{})
	if err != nil {
		return nil, fmt.Errorf("calling tool %q: %w", tool.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("tool %q returned no result", tool.Name)
	}
	return result, nil
}
