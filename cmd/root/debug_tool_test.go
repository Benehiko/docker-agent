package root

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

const debugToolConfig = `
agents:
  root:
    model: test
    toolsets:
      - type: think
      - type: filesystem
        tools: [read_file]
  helper:
    model: test
    toolsets:
      - type: shell
        defer: true
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`

func runDebugTool(t *testing.T, flags *debugFlags, args ...string) (string, error) {
	t.Helper()

	return runDebugToolConfig(t, flags, debugToolConfig, args...)
}

func runDebugToolConfig(t *testing.T, flags *debugFlags, cfg string, args ...string) (string, error) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from a tool"), 0o600))
	flags.runConfig.WorkingDir = dir
	flags.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(t.Context())

	err := flags.runDebugToolCommand(cmd, append([]string{path}, args...))
	return out.String(), err
}

func TestDebugToolCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		flags  debugFlags
		args   []string
		output string
		err    string
	}{
		{
			name:   "default agent",
			args:   []string{"think", `{"thought":"testing tools"}`},
			output: "Thoughts:\ntesting tools\n",
		},
		{
			name:   "default parameters",
			args:   []string{"think"},
			output: "Thoughts:\n\n",
		},
		{
			name:   "working directory",
			args:   []string{"read_file", `{"path":"hello.txt"}`},
			output: "hello from a tool",
		},
		{
			name:   "selected agent and deferred tool",
			flags:  debugFlags{toolAgent: "helper"},
			args:   []string{"shell", `{"cmd":"echo hello"}`},
			output: "hello\n",
		},
		{
			name:  "unknown agent",
			flags: debugFlags{toolAgent: "missing"},
			args:  []string{"think"},
			err:   "agent not found: missing (available agents: root, helper)",
		},
		{
			name: "tool belongs to another agent",
			args: []string{"shell"},
			err:  `tool "shell" not found for agent "root"`,
		},
		{
			name: "filtered tool",
			args: []string{"write_file", `{"path":"hello.txt","content":"overwrite"}`},
			err:  `tool "write_file" not found for agent "root"`,
		},
		{
			name: "invalid JSON",
			args: []string{"think", `{"thought":`},
			err:  "parameters must be a JSON object",
		},
		{
			name: "array parameters",
			args: []string{"think", `[]`},
			err:  "parameters must be a JSON object",
		},
		{
			name: "null parameters",
			args: []string{"think", `null`},
			err:  "parameters must be a JSON object, not null",
		},
		{
			name: "invalid parameter type",
			args: []string{"think", `{"thought":{}}`},
			err:  `calling tool "think"`,
		},
		{
			name:   "tool error",
			args:   []string{"read_file", `{"path":"missing.txt"}`},
			output: "not found\n",
			err:    `tool "read_file" returned an error for agent "root"`,
		},
	}
	for i := range tests {
		tt := &tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := runDebugTool(t, &tt.flags, tt.args...)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
			} else {
				require.NoError(t, err)
			}
			if tt.output != "" {
				assert.Contains(t, out, tt.output)
			} else {
				assert.Empty(t, out)
			}
		})
	}
}

func TestDebugToolCommand_JSON(t *testing.T) {
	t.Parallel()

	for _, isError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "tool error"}[isError], func(t *testing.T) {
			t.Parallel()

			args := []string{"think", `{"thought":"hello"}`}
			if isError {
				args = []string{"read_file", `{"path":"missing.txt"}`}
			}
			out, err := runDebugTool(t, &debugFlags{toolJSON: true}, args...)
			if isError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			var result tools.ToolCallResult
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			assert.Equal(t, isError, result.IsError)
			assert.NotEmpty(t, result.Output)
		})
	}
}

func TestDebugToolCommand_Arguments(t *testing.T) {
	t.Parallel()

	cmd, _, err := newDebugCmd().Find([]string{"tool"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, err)
	require.Equal(t, "tool", cmd.Name())
	for _, args := range [][]string{nil, {"agent.yaml"}, {"agent.yaml", "think", "{}", "extra"}} {
		require.Error(t, cmd.Args(cmd, args))
	}
	assert.NoError(t, cmd.Args(cmd, []string{"agent.yaml", "think"}))
	assert.NoError(t, cmd.Args(cmd, []string{"agent.yaml", "think", "{}"}))
	require.NoError(t, cmd.ParseFlags([]string{"-a", "helper", "--json"}))
	assert.NoError(t, cmd.Help())
	assert.Contains(t, out.String(), "Call a tool of an agent directly")
}

func TestCallDebugTool(t *testing.T) {
	t.Parallel()

	t.Run("call shape and full result", func(t *testing.T) {
		t.Parallel()

		expected := &tools.ToolCallResult{
			Output:            "result",
			Images:            []tools.MediaContent{{Data: "aGVsbG8=", MimeType: "image/png"}},
			StructuredContent: map[string]any{"key": "value"},
		}
		tool := tools.Tool{Name: "test", Handler: func(ctx context.Context, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
			assert.Equal(t, t.Context(), ctx)
			assert.Equal(t, "debug_test", tc.ID)
			assert.Equal(t, tools.ToolType("function"), tc.Type)
			assert.Equal(t, tools.FunctionCall{Name: "test", Arguments: `{"value":42}`}, tc.Function)
			assert.IsType(t, tools.NopRuntime{}, rt)
			return expected, nil
		}}
		result, err := callDebugTool(t.Context(), tool, `{"value":42}`)
		require.NoError(t, err)
		assert.Same(t, expected, result)
	})

	t.Run("handler error", func(t *testing.T) {
		t.Parallel()

		expected := errors.New("handler failed")
		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return nil, expected
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorIs(t, err, expected)
	})

	t.Run("nil result", func(t *testing.T) {
		t.Parallel()

		tool := tools.Tool{Name: "test", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return nil, nil
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorContains(t, err, "returned no result")
	})

	t.Run("runtime tool", func(t *testing.T) {
		t.Parallel()

		tool := tools.Tool{Name: "test", RuntimeHandler: "runtime", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			t.Fatal("runtime handler must not be called")
			return nil, nil
		}}
		_, err := callDebugTool(t.Context(), tool, "{}")
		require.ErrorContains(t, err, "requires an agent runtime")
	})

	t.Run("missing handler", func(t *testing.T) {
		t.Parallel()

		_, err := callDebugTool(t.Context(), tools.Tool{Name: "test"}, "{}")
		require.ErrorContains(t, err, "requires an agent runtime")
	})
}

func TestDebugToolCommand_MCP(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "debug-test", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, args struct {
		Message string `json:"message"`
	},
	) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: args.Message},
				&mcp.ImageContent{Data: []byte("image"), MIMEType: "image/png"},
			},
		}, map[string]string{"message": args.Message}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil))
	t.Cleanup(httpServer.Close)

	cfg := fmt.Sprintf(`agents:
  root:
    model: test
    toolsets:
      - type: mcp
        name: test
        allow_private_ips: true
        remote:
          url: %s
          transport_type: streamable
models:
  test:
    provider: openai
    model: gpt-4o
    max_tokens: 100
`, httpServer.URL)
	out, err := runDebugToolConfig(t, &debugFlags{toolJSON: true}, cfg, "test_echo", `{"message":"hello MCP"}`)
	require.NoError(t, err)
	var result tools.ToolCallResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "hello MCP", result.Output)
	assert.False(t, result.IsError)
	assert.Equal(t, []tools.MediaContent{{Data: "aW1hZ2U=", MimeType: "image/png"}}, result.Images)
	assert.Equal(t, map[string]any{"message": "hello MCP"}, result.StructuredContent)
}

func TestDebugToolsetsCommand_IncludesDeferredTools(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(debugToolConfig), 0o600))
	flags := &debugFlags{toolsetsJSON: true}
	flags.runConfig.EnvProviderOverride = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test-key"})
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	require.NoError(t, flags.runDebugToolsetsCommand(cmd, []string{path}))
	var infos []agentToolsInfo
	require.NoError(t, json.Unmarshal(out.Bytes(), &infos))
	require.Len(t, infos, 2)
	for _, info := range infos {
		if info.Agent != "helper" {
			continue
		}
		for _, tool := range info.Tools {
			if tool.Name == "shell" {
				assert.NotNil(t, tool.Parameters)
				return
			}
		}
	}
	t.Fatal("deferred shell tool must be discoverable")
}
