package dmrmodels

import (
	"context"
	"net"
	"os/exec"
	"slices"
)

type dockerConnectionKey struct{}

type dockerConnection struct {
	args []string
	dial func(context.Context) (net.Conn, error)
}

// ContextWithDockerConnection supplies the Docker CLI's connection settings for
// both Model Runner subprocesses and HTTP requests through the engine.
func ContextWithDockerConnection(ctx context.Context, args []string, dial func(context.Context) (net.Conn, error)) context.Context {
	return context.WithValue(ctx, dockerConnectionKey{}, &dockerConnection{args: slices.Clone(args), dial: dial})
}

// HasDockerConnection reports whether the caller supplied a Docker connection.
func HasDockerConnection(ctx context.Context) bool {
	_, ok := ctx.Value(dockerConnectionKey{}).(*dockerConnection)
	return ok
}

// DockerCommand creates a Docker subprocess using the selected connection.
func DockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	if conn, ok := ctx.Value(dockerConnectionKey{}).(*dockerConnection); ok {
		args = slices.Concat(conn.args, args)
	}
	return exec.CommandContext(ctx, "docker", args...)
}
