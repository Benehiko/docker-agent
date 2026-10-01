package app

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// eventSubscriber buffers reliable deliveries independently of slow consumers.
// Best-effort subscribers keep their bounded channel and overflow policy.
type eventSubscriber struct {
	ch                chan tea.Msg
	queue             *eventQueue
	prepare           func(tea.Msg) tea.Msg
	registered        func()
	prepareGeneration func(tea.Msg, uint64) tea.Msg
}

// SubscribeOption configures reliable event delivery.
type SubscribeOption func(*eventSubscriber)

// WithEventMapper stamps routing metadata before an event is queued.
// The mapper runs on the fan-out goroutine and must not block; nil skips delivery.
func WithEventMapper(mapper func(tea.Msg) tea.Msg) SubscribeOption {
	return func(sub *eventSubscriber) {
		sub.prepare = mapper
	}
}

// WithGenerationEventMapper preserves producer identity through routing queues.
func WithGenerationEventMapper(mapper func(tea.Msg, uint64) tea.Msg) SubscribeOption {
	return func(sub *eventSubscriber) { sub.prepareGeneration = mapper }
}

// WithSubscriptionReady signals after registration, before any events are delivered.
func WithSubscriptionReady(ready func()) SubscribeOption {
	return func(sub *eventSubscriber) { sub.registered = ready }
}

type eventQueue struct {
	mu      sync.Mutex
	pending []tea.Msg
	ready   chan struct{}
	closed  bool
}

func newEventQueue() *eventQueue {
	return &eventQueue{ready: make(chan struct{}, 1)}
}

func (q *eventQueue) push(msg tea.Msg) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.pending = append(q.pending, msg)
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *eventQueue) next(ctx context.Context, done <-chan struct{}) (tea.Msg, bool) {
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		default:
		}

		q.mu.Lock()
		if len(q.pending) > 0 {
			msg := q.pending[0]
			q.pending[0] = nil
			q.pending = q.pending[1:]
			if len(q.pending) == 0 {
				q.pending = nil
			}
			q.mu.Unlock()
			return msg, true
		}
		q.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false
		case <-done:
			return nil, false
		case <-q.ready:
		}
	}
}

func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.pending = nil
}
