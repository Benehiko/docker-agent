package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

// runStreamRecordingClient is a stubRemoteClient variant that records the
// model ref forwarded to RunAgent / RunAgentWithAgentName and lets the test
// inject a synthetic dispatch error so the early-error path is exercised.
type runStreamRecordingClient struct {
	stubRemoteClient

	runErr        error
	gotModel      string
	gotInvocation int
}

func (c *runStreamRecordingClient) RunAgent(_ context.Context, _, _ string, _ []api.Message, model string) (<-chan Event, error) {
	c.gotInvocation++
	c.gotModel = model
	if c.runErr != nil {
		return nil, c.runErr
	}
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func (c *runStreamRecordingClient) RunAgentWithAgentName(ctx context.Context, sessionID, agent, _ string, msgs []api.Message, model string) (<-chan Event, error) {
	return c.RunAgent(ctx, sessionID, agent, msgs, model)
}

// TestRemoteRuntime_SetAgentModel_RetainsOverrideOnDispatchError pins the
// fix for the silent-drop bug: when the next RunStream's HTTP dispatch
// fails (network, auth, server unavailable), the queued override MUST
// remain queued so the next attempt still applies the user's chosen
// model. Clearing it eagerly would silently swallow the request.
func TestRemoteRuntime_SetAgentModel_RetainsOverrideOnDispatchError(t *testing.T) {
	t.Parallel()

	client := &runStreamRecordingClient{
		stubRemoteClient: stubRemoteClient{
			cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
		},
		runErr: errors.New("dial tcp: connection refused"),
	}
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)

	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "openai/gpt-4o"))

	// First RunStream fails to dispatch — drain the error event.
	sess := &session.Session{ID: "s1"}
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 1, client.gotInvocation)
	require.Equal(t, "openai/gpt-4o", client.gotModel)

	// Second RunStream must re-forward the override since the first
	// attempt never reached the server.
	client.runErr = nil
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 2, client.gotInvocation)
	assert.Equal(t, "openai/gpt-4o", client.gotModel, "override must persist after dispatch error")

	// Once successfully forwarded, a subsequent call without a new
	// SetAgentModel must NOT re-send the same override (it is now
	// owned by the server-side session state).
	client.gotModel = "sentinel"
	for range rt.RunStream(t.Context(), sess) {
	}
	require.Equal(t, 3, client.gotInvocation)
	assert.Empty(t, client.gotModel, "override must clear after successful dispatch")
}

// TestRemoteRuntime_SetAgentModel_LatestQueuedWins guards the
// concurrent-update path: if SetAgentModel is called between the
// snapshot and the post-dispatch clear, the newer ref must NOT be
// silently overwritten.
func TestRemoteRuntime_SetAgentModel_LatestQueuedWins(t *testing.T) {
	t.Parallel()

	rt, err := NewRemoteRuntime(&stubRemoteClient{
		cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
	})
	require.NoError(t, err)

	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "first"))
	require.NoError(t, rt.SetAgentModel(t.Context(), "test", "second"))

	rt.pendingMu.Lock()
	got := rt.pendingModelOverride
	rt.pendingMu.Unlock()
	assert.Equal(t, "second", got)
}

// mcpPromptsClient is a stubRemoteClient variant returning MCP prompts the
// way the HTTP client does: decoded into map[string]any, with each prompt a
// nested map — never a concrete tools.PromptInfo.
type mcpPromptsClient struct {
	stubRemoteClient
}

func (c *mcpPromptsClient) GetSessionMCPPrompts(context.Context, string) (map[string]any, error) {
	return map[string]any{
		"review": map[string]any{
			"name":        "review",
			"description": "Review code",
			"arguments": []any{
				map[string]any{"name": "path", "description": "File to review", "required": true},
			},
		},
	}, nil
}

// TestRemoteRuntime_CurrentMCPPrompts pins the JSON-decoded-map conversion:
// values arrive as map[string]any, so a plain type assertion would silently
// drop every prompt.
func TestRemoteRuntime_CurrentMCPPrompts(t *testing.T) {
	t.Parallel()

	client := &mcpPromptsClient{
		stubRemoteClient: stubRemoteClient{
			cfg: &latest.Config{Agents: latest.Agents{{Name: "test"}}},
		},
	}
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	rt.sessionID = "session-1"

	prompts := rt.CurrentMCPPrompts(t.Context())
	require.Len(t, prompts, 1)
	assert.Equal(t, "review", prompts["review"].Name)
	assert.Equal(t, "Review code", prompts["review"].Description)
	require.Len(t, prompts["review"].Arguments, 1)
	assert.Equal(t, "path", prompts["review"].Arguments[0].Name)
	assert.True(t, prompts["review"].Arguments[0].Required)
}

func TestRemoteRuntime_BackgroundEventsSurviveTurnsWithoutReplayingHistory(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	backgroundReady := make(chan struct{})
	deliverRecall := make(chan struct{})
	backgroundStopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			snapshots.Add(1)
			fmt.Fprint(w, `{"id":"s","last_event_seq":12}`)
		case strings.HasSuffix(req.URL.Path, "/events"):
			subscriptions.Add(1)
			assert.Equal(t, "12", req.Header.Get("Last-Event-ID"))
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(backgroundReady)
			select {
			case <-deliverRecall:
				fmt.Fprint(w, "id: 13\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall answer\"}\n\n")
				w.(http.Flusher).Flush()
			case <-req.Context().Done():
			}
			<-req.Context().Done()
			close(backgroundStopped)
		case req.Method == http.MethodPost:
			runs.Add(1)
			assert.Equal(t, int32(1), snapshots.Load(), "cursor must be taken before RunAgent")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"agent_choice\",\"message_id\":\"foreground\",\"content\":\"foreground answer\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan Event, 4)
	rt.OnBackgroundEvent(func(event Event) { background <- event })

	ctx, cancel := context.WithCancel(t.Context())
	var foreground []Event
	for event := range rt.RunStream(ctx, &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	require.Len(t, foreground, 2)
	cancel() // The turn context must not end the session subscription.
	select {
	case <-backgroundReady:
	case <-time.After(2 * time.Second):
		t.Fatal("background subscription did not connect")
	}
	close(deliverRecall)
	select {
	case event := <-background:
		answer, ok := event.(*AgentChoiceEvent)
		require.True(t, ok, "got %T", event)
		assert.Equal(t, "recall answer", answer.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("idle recall was not delivered")
	}

	for range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
	}
	assert.Equal(t, int32(2), runs.Load(), "subscription must not spawn extra runs")
	assert.Equal(t, int32(1), snapshots.Load(), "do not re-snapshot and skip pending events between turns")
	assert.Equal(t, int32(1), subscriptions.Load())
	assert.Empty(t, background, "foreground answers must not also reach the background sink")
	require.NoError(t, rt.Close())
	select {
	case <-backgroundStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop background subscription")
	}
}

func TestRemoteRuntime_BackgroundSubscriptionWaitsForEventLogAndDeliversElicitationOnce(t *testing.T) {
	t.Parallel()

	var logAvailable atomic.Bool
	var attempts, runs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			fmt.Fprint(w, `{"id":"s","last_event_seq":0}`)
		case strings.HasSuffix(req.URL.Path, "/events"):
			attempts.Add(1)
			assert.Equal(t, "0", req.Header.Get("Last-Event-ID"))
			if !logAvailable.Load() {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 1\ndata: {\"type\":\"elicitation_request\",\"elicitation_id\":\"eid\"}\n\nid: 2\ndata: {\"type\":\"session_exited\"}\n\n")
		case req.Method == http.MethodPost:
			runs.Add(1)
			assert.Eventually(t, func() bool { return attempts.Load() > 0 }, 2*time.Second, time.Millisecond)
			logAvailable.Store(true)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"elicitation_request\",\"elicitation_id\":\"eid\"}\n\ndata: {\"type\":\"stream_stopped\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan Event, 4)
	rt.OnBackgroundEvent(func(event Event) { background <- event })
	var foreground []Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	var requests []*ElicitationRequestEvent
	for _, event := range foreground {
		if request, ok := event.(*ElicitationRequestEvent); ok {
			requests = append(requests, request)
		}
	}
	require.Eventually(t, func() bool { return attempts.Load() >= 2 }, 3*time.Second, time.Millisecond)
	select {
	case event := <-background:
		request, ok := event.(*ElicitationRequestEvent)
		require.True(t, ok, "got %T", event)
		requests = append(requests, request)
	default:
	}
	require.Len(t, requests, 1, "foreground/background copies must be delivered once")
	assert.Equal(t, "eid", requests[0].ElicitationID)
	assert.Empty(t, background)
	assert.Equal(t, int32(1), runs.Load())
	require.GreaterOrEqual(t, attempts.Load(), int32(2))
}

func TestRemoteRuntime_BackgroundGapReconcilesSavedTextWithoutReplay(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	foregroundDone := make(chan struct{})
	streamReady := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			w.Header().Set("Content-Type", "application/json")
			if snapshots.Add(1) == 1 {
				fmt.Fprint(w, `{"id":"s","last_event_seq":10,"messages":[{"agent_name":"root","message":{"role":"assistant","message_id":"old","content":"old answer"}}]}`)
			} else {
				fmt.Fprint(w, `{"id":"s","last_event_seq":30,"messages":[
					{"agent_name":"root","message":{"role":"assistant","message_id":"old","content":"old answer"}},
					{"agent_name":"root","message":{"role":"assistant","message_id":"foreground","content":"foreground answer"}},
					{"agent_name":"root","message":{"role":"assistant","message_id":"recall","content":"recall final","reasoning_content":"PRIVATE reasoning"}},
					{"message":{"role":"tool","message_id":"tool","content":"PRIVATE tool output"}},
					{"message":{"role":"assistant","message_id":"tool-call","content":"PRIVATE tool content","tool_calls":[{"id":"call","type":"function"}]}},
					{"implicit":true,"message":{"role":"assistant","message_id":"implicit","content":"PRIVATE implicit"}}
				]}`)
			}
		case strings.HasSuffix(req.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			if subscriptions.Add(1) == 1 {
				assert.Equal(t, "10", req.Header.Get("Last-Event-ID"))
				w.(http.Flusher).Flush()
				select {
				case <-foregroundDone:
				case <-req.Context().Done():
					return
				}
				fmt.Fprint(w, "id: 11\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall \"}\n\ndata: {\"type\":\"gap\"}\n\nid: 20\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"blind replay\"}\n\n")
				return
			}
			assert.Equal(t, "30", req.Header.Get("Last-Event-ID"))
			// Snapshot/log overlap is possible: the completed ID must not append twice.
			fmt.Fprint(w, "id: 31\ndata: {\"type\":\"agent_choice\",\"message_id\":\"recall\",\"content\":\"recall final\"}\n\nid: 32\ndata: {\"type\":\"agent_choice\",\"message_id\":\"later\",\"content\":\"later answer\"}\n\n")
			w.(http.Flusher).Flush()
			close(streamReady)
			<-req.Context().Done()
		case req.Method == http.MethodPost:
			runs.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"agent_choice\",\"message_id\":\"foreground\",\"content\":\"foreground answer\"}\n\ndata: {\"type\":\"stream_stopped\",\"session_id\":\"s\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan Event, 16)
	rt.OnBackgroundEvent(func(event Event) { background <- event })
	var foreground []Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		foreground = append(foreground, event)
	}
	require.Len(t, foreground, 2)
	close(foregroundDone)
	select {
	case <-streamReady:
	case <-time.After(3 * time.Second):
		t.Fatal("gap recovery did not reconnect")
	}
	var choices []*AgentChoiceEvent
	deadline := time.After(3 * time.Second)
	for len(choices) < 3 {
		select {
		case event := <-background:
			switch event := event.(type) {
			case *AgentChoiceEvent:
				choices = append(choices, event)
			case *WarningEvent:
				assert.Contains(t, event.Message, "gap")
			default:
				t.Fatalf("unexpected event %T", event)
			}
		case <-deadline:
			t.Fatal("saved final answer was not reconciled")
		}
	}
	assert.Equal(t, "recall ", choices[0].Content)
	assert.Equal(t, "final", choices[1].Content, "append only the missing suffix")
	assert.Equal(t, "recall", choices[1].MessageID)
	assert.Equal(t, "s", choices[1].SessionID)
	assert.Equal(t, "later answer", choices[2].Content)
	assert.Empty(t, background)
	assert.Equal(t, int32(1), runs.Load())
	assert.Equal(t, int32(2), snapshots.Load())
	assert.Equal(t, int32(2), subscriptions.Load())
}

func TestRemoteMessageHistoryRejectsUnsafeRecovery(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		delivered *AgentChoiceEvent
		savedID   string
		saved     string
	}{
		{name: "unidentified delivered message", delivered: AgentChoice("root", "s", "partial").(*AgentChoiceEvent), savedID: "id", saved: "partial final"},
		{name: "unidentified saved message", delivered: AgentChoice("root", "s", "partial", "id").(*AgentChoiceEvent), saved: "partial final"},
		{name: "changed prefix", delivered: AgentChoice("root", "s", "partial", "id").(*AgentChoiceEvent), savedID: "id", saved: "rewritten final"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			history := newRemoteMessageHistory(nil)
			require.True(t, history.deliver(tc.delivered, func(Event) bool { return true }))
			snapshot := &api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
				{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "new", Content: "must not partially replay"}},
				{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: tc.savedID, Content: tc.saved}},
			}}
			var got []Event
			err := history.reconcile(snapshot, func(event Event) { got = append(got, event) })
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestRemoteRuntime_ForegroundEOFHasErrorStop(t *testing.T) {
	t.Parallel()

	client := &runStreamRecordingClient{}
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	var got []Event
	for event := range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
		got = append(got, event)
	}
	require.Len(t, got, 2)
	assert.IsType(t, &ErrorEvent{}, got[0])
	stop, ok := got[1].(*StreamStoppedEvent)
	require.True(t, ok)
	assert.Equal(t, "error", stop.Reason)
	assert.Equal(t, "s", stop.SessionID)
	assert.Equal(t, 1, client.gotInvocation)
}

func TestRemoteRuntime_BackgroundGapWaitsForIdleAndCloseCancelsRecovery(t *testing.T) {
	t.Parallel()

	var runs, snapshots, subscriptions atomic.Int32
	recovering := make(chan struct{})
	recoveryStopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/snapshot"):
			switch snapshots.Add(1) {
			case 1:
				fmt.Fprint(w, `{"id":"s","last_event_seq":0}`)
			case 2:
				fmt.Fprint(w, `{"id":"s","last_event_seq":20,"streaming":true,"messages":[{"message":{"role":"assistant","message_id":"id","content":"not final"}}]}`)
			default:
				close(recovering)
				<-req.Context().Done()
				close(recoveryStopped)
			}
		case strings.HasSuffix(req.URL.Path, "/events"):
			subscriptions.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"gap\"}\n\n")
		case req.Method == http.MethodPost:
			runs.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL)
	require.NoError(t, err)
	rt, err := NewRemoteRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan Event, 4)
	rt.OnBackgroundEvent(func(event Event) { background <- event })
	for range rt.RunStream(t.Context(), &session.Session{ID: "s"}) {
	}
	select {
	case <-recovering:
	case <-time.After(3 * time.Second):
		t.Fatal("did not wait for an idle snapshot")
	}
	require.NoError(t, rt.Close())
	select {
	case <-recoveryStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel snapshot recovery")
	}
	assert.Equal(t, int32(1), runs.Load())
	assert.Equal(t, int32(1), subscriptions.Load(), "never advance the cursor from a running snapshot")
	for len(background) > 0 {
		assert.IsType(t, &WarningEvent{}, <-background, "partial saved text must not be emitted")
	}
}

func TestRemoteMessageHistoryPreservesKnownSubSessionScope(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	require.True(t, history.deliver(AgentChoice("worker", "child", "partial ", "id"), func(Event) bool { return true }))
	snapshot := &api.SessionSnapshotResponse{ID: "parent", Messages: []session.Message{
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "partial final"}},
	}}
	var got []Event
	require.NoError(t, history.reconcile(snapshot, func(event Event) { got = append(got, event) }))
	require.Len(t, got, 1)
	choice, ok := got[0].(*AgentChoiceEvent)
	require.True(t, ok)
	assert.Equal(t, "child", choice.SessionID)
	assert.Equal(t, "final", choice.Content)
}

func TestRemoteMessageHistoryDeduplicatesElicitationEitherDeliveryOrder(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	for _, id := range []string{"foreground-first", "background-first"} {
		request := &ElicitationRequestEvent{ElicitationID: id}
		var deliveries int
		for range 2 {
			require.True(t, history.deliver(request, func(Event) bool {
				deliveries++
				return true
			}))
		}
		assert.Equal(t, 1, deliveries)
	}
}

func TestRemoteMessageHistoryRejectsDuplicateSnapshotIDs(t *testing.T) {
	t.Parallel()

	history := newRemoteMessageHistory(nil)
	snapshot := &api.SessionSnapshotResponse{ID: "s", Messages: []session.Message{
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "first"}},
		{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "id", Content: "second"}},
	}}
	var got []Event
	require.Error(t, history.reconcile(snapshot, func(event Event) { got = append(got, event) }))
	assert.Empty(t, got)
}

func TestRemoteRuntimeRetiredBackgroundSubscriptionDoesNotEmit(t *testing.T) {
	t.Parallel()
	rt, err := NewRemoteRuntime(&stubRemoteClient{})
	require.NoError(t, err)
	canceled := make(chan struct{})
	sub := &remoteEventSubscription{cancel: func() { close(canceled) }, sessionID: "old", history: newRemoteMessageHistory(nil)}
	rt.background = sub
	var delivered []Event
	rt.OnBackgroundEvent(func(event Event) { delivered = append(delivered, event) })
	rt.RetireBackgroundEvents()
	<-canceled
	rt.emitBackgroundEvent(sub, AgentChoice("root", "old", "OLD-ANSWER", "old"))
	require.Empty(t, delivered)
	require.Nil(t, rt.background)
}
