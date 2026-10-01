package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func leanTranscriptText(m *model) string {
	return ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
}

func TestCanonicalAssistantContentReconcilesLeanTranscript(t *testing.T) {
	t.Parallel()
	for _, partial := range []string{"", "FINAL-", "ANSWER", "FINAL-ANSWER"} {
		t.Run(partial, func(t *testing.T) {
			m := bareModel(40)
			m.handleEvent(t.Context(), runtime.StreamStarted("session", "root"))
			if partial != "" {
				m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", partial, "answer"))
			}
			msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "answer", Content: "FINAL-ANSWER"})
			for range 2 {
				m.handleEvent(t.Context(), runtime.MessageAdded("session", msg, "root"))
			}
			require.Equal(t, 1, strings.Count(leanTranscriptText(m), "FINAL-ANSWER"))
		})
	}
}

func TestLeanRetryMessageIDsKeepAnswerVisible(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", "[diagnostic](", "attempt-one"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "session", "FINAL-ANSWER)", "attempt-two"))
	require.Contains(t, leanTranscriptText(m), "FINAL-ANSWER")
}

func TestLeanInterleavedSessionsDoNotMergeMarkdown(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "child", "[diagnostic](", "answer"))
	m.handleEvent(t.Context(), runtime.AgentChoice("root", "parent", "FINAL-ANSWER)", "answer"))
	require.Contains(t, leanTranscriptText(m), "FINAL-ANSWER")
}

func TestRetiredLeanEventsCannotAffectReplacement(t *testing.T) {
	t.Parallel()
	m := bareModel(40)
	old := leanEvent{generation: 0, inner: runtime.AgentChoice("root", "same-session", "OLD-ANSWER", "old")}
	m.eventGeneration.Add(1)
	m.handleEvent(t.Context(), old)
	m.handleEvent(t.Context(), leanEvent{generation: 1, inner: runtime.AgentChoice("root", "same-session", "NEW-ANSWER", "new")})
	require.NotContains(t, leanTranscriptText(m), "OLD-ANSWER")
	require.Contains(t, leanTranscriptText(m), "NEW-ANSWER")
}
