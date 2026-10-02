//go:build darwin && !no_audio

package transcribe

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStart_MissingAPIKey(t *testing.T) {
	t.Parallel()

	transcriber := New("")
	err := transcriber.Start(t.Context(), nil)

	require.EqualError(t, err, "/speak sends audio directly to OpenAI; set a separate OPENAI_API_KEY environment variable even when chat uses Docker's models gateway, which does not support /speak")
	assert.False(t, transcriber.IsRunning())
	assert.Nil(t, transcriber.conn)
}

func TestStart_ConnectionFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	transcriber := New("test-api-key")
	err := transcriber.Start(ctx, nil)

	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "connect /speak directly to OpenAI Realtime API (not Docker's models gateway): ")
	assert.False(t, transcriber.IsRunning())
	assert.Nil(t, transcriber.conn)
}
