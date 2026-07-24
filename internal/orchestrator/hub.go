package orchestrator

import "sync"

// replayBufferSize bounds the per-run event history the hub retains for replay,
// capping memory while covering the subscribe-after-trigger race. Sized above the
// depth-4 worst-case event count (≈ breadth+breadth²+breadth³+breadth⁴ triplets plus
// branch-failure/loop-complete frames) so a mid-run SSE reconnect replays the whole
// run rather than dropping the earliest triplets from the feed.
const replayBufferSize = 256

// Event is one progress item published to a run's subscribers and serialized as
// an SSE data frame.
type Event struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Hub is an in-memory pub/sub keyed by optimization_function_id. Because the
// Phase-1 trigger (202) and the SSE subscribe are independent HTTP calls, a hub
// with no history would lose every event published before a client subscribes;
// each run keeps a bounded replay ring buffer so a subscribe-after-trigger
// client still sees the run's start, then switches to live. Runs are evicted on
// completion to cap memory.
type Hub struct {
	mu   sync.Mutex
	runs map[string]*runState
}

type runState struct {
	buffer      []Event
	subscribers map[chan Event]struct{}
}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{runs: map[string]*runState{}}
}

func (h *Hub) run(id string) *runState {
	rs := h.runs[id]
	if rs == nil {
		rs = &runState{subscribers: map[chan Event]struct{}{}}
		h.runs[id] = rs
	}
	return rs
}

// Subscribe registers a live channel for a run and returns the buffered history
// to replay first, plus a cancel that unsubscribes. A dropped SSE connection
// calls cancel; it never cancels the loop, which runs on its own context.
func (h *Hub) Subscribe(id string) (replay []Event, ch chan Event, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.run(id)
	replay = append([]Event(nil), rs.buffer...)
	ch = make(chan Event, replayBufferSize)
	rs.subscribers[ch] = struct{}{}
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := rs.subscribers[ch]; ok {
			delete(rs.subscribers, ch)
			close(ch)
		}
		// Evict a run that no live loop is feeding: streaming an arbitrary or
		// already-finished id lazily creates a runState, and only Complete (at a
		// real loop's end) otherwise removes it. Without this, a network client
		// streaming distinct ids would leak map entries unboundedly. A run with a
		// non-empty buffer belongs to an in-flight or recent loop and is left for
		// Complete to evict.
		if len(rs.subscribers) == 0 && len(rs.buffer) == 0 {
			delete(h.runs, id)
		}
	}
	return replay, ch, cancel
}

// Publish appends the event to the run's replay buffer and fans it out to live
// subscribers. A slow subscriber's send is dropped rather than blocking the
// loop.
func (h *Hub) Publish(id string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.run(id)
	rs.buffer = append(rs.buffer, ev)
	if len(rs.buffer) > replayBufferSize {
		rs.buffer = rs.buffer[len(rs.buffer)-replayBufferSize:]
	}
	for ch := range rs.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Complete evicts the run: it closes and drops every subscriber channel (ending
// their streams) and discards the buffer. Publish a terminal event before
// calling this so subscribers see the run's end.
func (h *Hub) Complete(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.runs[id]
	if rs == nil {
		return
	}
	for ch := range rs.subscribers {
		delete(rs.subscribers, ch)
		close(ch)
	}
	delete(h.runs, id)
}
