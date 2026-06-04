// Package runbus is an in-process pub/sub for run-scoped events. It
// backs the /api/runs/:run_id/events SSE stream so the UI can react to
// step-lifecycle transitions and plugin-emitted progress payloads with
// no polling latency.
//
// The bus is intentionally thin: publishers hand it opaque JSON payloads,
// subscribers get them back verbatim. Nothing here parses plugin bytes.
// Events sent when there are no subscribers are dropped - late joiners
// bootstrap themselves from the stored run summary + run_steps snapshot
// before the SSE handler attaches to the live stream.
package runbus

import (
	"encoding/json"
	"sync"
)

// Bus fans out run-scoped events to any number of subscribers per run.
// One Bus per orchestrator process; the Engine owns it.
type Bus struct {
	mu     sync.RWMutex
	byRun  map[string]map[chan Event]struct{}
	buffer int
}

// Event is one message on the bus. Type identifies the payload shape
// ("step-start", "step-progress", "step-finish", "run-status"). Data is
// a JSON-encoded object the SSE writer forwards verbatim into the
// `data:` line of the outgoing frame.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// New allocates a Bus. buffer is the per-subscriber channel depth; when
// a subscriber can't keep up, the oldest queued event is dropped so
// publishers never block on a slow browser.
func New(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 32
	}
	return &Bus{
		byRun:  map[string]map[chan Event]struct{}{},
		buffer: buffer,
	}
}

// Subscribe returns a channel that receives every event published for
// the given runID until unsubscribe is called. Callers must drain the
// channel promptly or accept dropped events (see New).
func (b *Bus) Subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, b.buffer)
	b.mu.Lock()
	subs, ok := b.byRun[runID]
	if !ok {
		subs = map[chan Event]struct{}{}
		b.byRun[runID] = subs
	}
	subs[ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if subs, ok := b.byRun[runID]; ok {
			if _, present := subs[ch]; present {
				delete(subs, ch)
				close(ch)
			}
			if len(subs) == 0 {
				delete(b.byRun, runID)
			}
		}
		b.mu.Unlock()
	}
	return ch, unsubscribe
}

// Publish sends an event to every current subscriber of runID. Runs
// with no subscribers cost nothing. If a subscriber's channel is full,
// the oldest queued event is dropped to keep publishers non-blocking.
func (b *Bus) Publish(runID string, evt Event) {
	// Send while holding the read lock: unsubscribe closes the channel
	// under the write lock, so releasing first would let it close between
	// the copy and the send, and a send on a closed channel panics. Every
	// send below is non-blocking, so holding the lock can't stall anyone.
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.byRun[runID] {
		select {
		case ch <- evt:
		default:
			// Full: drop the oldest and try once more. Non-blocking; if
			// the second send also fails the event is dropped.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- evt:
			default:
			}
		}
	}
}
