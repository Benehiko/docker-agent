package root

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestResolveDataDir_Precedence(t *testing.T) {
	t.Setenv(envDataDir, "")
	assert.Empty(t, resolveDataDir(""))

	t.Setenv(envDataDir, "/from-env")
	assert.Equal(t, "/from-env", resolveDataDir(""))
	assert.Equal(t, "/from-flag", resolveDataDir("/from-flag"))
}

func TestDataDir_RootCommand(t *testing.T) {
	for _, tt := range []struct {
		name   string
		env    bool
		flag   bool
		preset bool
		nested bool
	}{
		{name: "default"},
		{name: "environment", env: true},
		{name: "flag", flag: true},
		{name: "flag overrides environment", env: true, flag: true},
		{name: "nested environment", env: true, nested: true},
		{name: "preserves programmatic override", preset: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv(envConfigDir, t.TempDir())
			t.Setenv(cagentEnvConfigDir, "")
			t.Setenv(envDataDir, "")
			logger := slog.Default()
			t.Cleanup(func() {
				slog.SetDefault(logger)
				paths.SetDataDir("")
				paths.SetConfigDir("")
			})

			expected := filepath.Join(home, ".cagent")
			if tt.preset {
				expected = t.TempDir()
				paths.SetDataDir(expected)
			}

			root := NewRootCmd()
			root.SetContext(t.Context())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)

			if tt.env {
				expected = t.TempDir()
				t.Setenv(envDataDir, expected)
			}

			probe := &cobra.Command{
				Use: "test-probe",
				RunE: func(*cobra.Command, []string) error {
					assert.Equal(t, expected, paths.GetDataDir())
					assert.Equal(t, filepath.Join(expected, "session.db"), sessionDBPath(""))
					h, err := history.New("")
					require.NoError(t, err)
					require.NoError(t, h.Add("test prompt"))
					assert.FileExists(t, filepath.Join(expected, "history"))
					return nil
				},
			}
			runConfig := config.RuntimeConfig{
				EnvProviderForTests: environment.NewMapEnvProvider(nil),
			}
			addGatewayFlags(probe, &runConfig, func() (*userconfig.Config, error) {
				return &userconfig.Config{}, nil
			})

			args := []string{"test-probe"}
			if tt.nested {
				parent := &cobra.Command{Use: "test-parent"}
				parent.AddCommand(probe)
				root.AddCommand(parent)
				args = append([]string{"test-parent"}, args...)
			} else {
				root.AddCommand(probe)
			}
			if tt.flag {
				expected = t.TempDir()
				args = append(args, "--data-dir", expected)
			}
			root.SetArgs(args)
			require.NoError(t, root.Execute())
		})
	}
}
