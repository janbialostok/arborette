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
//
// Coalesce marks an event whose latest value supersedes its predecessors (a
// cumulative distribution, not an append-only fact), so the replay buffer keeps
// only the newest frame of that type instead of one per update -- without it a
// per-triplet cumulative frame would roughly double a run's frame count and
// evict the earliest triplets the buffer is sized to replay. It is a hub-internal
// routing hint and is deliberately untagged for the wire: frames are marshalled
// whole, so an exported field would appear in every event a client parses.
type Event struct {
	Type     string         `json:"type"`
	Payload  map[string]any `json:"payload,omitempty"`
	Coalesce bool           `json:"-"`
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
	// loop records that a run is feeding this state, which decides who evicts it: a
	// loop's state is Complete's to reap and must outlive a dropped connection so a
	// reconnect can replay it, while state a subscriber alone brought into being has
	// no such owner and belongs to whoever unsubscribes last. Only Publish sets it.
	loop bool
}

func (rs *runState) replaceBuffered(ev Event) bool {
	for i := range rs.buffer {
		if rs.buffer[i].Type == ev.Type {
			rs.buffer[i] = ev
			return true
		}
	}
	return false
}

// deliver buffers the event for replay and fans it out. A slow subscriber's send is
// dropped rather than blocking the loop, and a coalescing event replaces its
// predecessor in place, so a replay keeps the order events happened in rather than
// jumping the newest value to the end.
func (rs *runState) deliver(ev Event) {
	if !ev.Coalesce || !rs.replaceBuffered(ev) {
		rs.buffer = append(rs.buffer, ev)
	}
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
		// Evict a run no loop owns: streaming an arbitrary or already-finished id
		// lazily creates a runState, and only Complete (at a real loop's end)
		// otherwise removes it, so without this a client streaming distinct ids
		// would leak map entries unboundedly.
		//
		// The identity check is what keeps this cancel from reaching past its own
		// state: a run that Complete evicted while this subscriber still held it has
		// since been replaced under the same key, and deleting by key alone would
		// take the successor -- and its live subscribers' delivery -- with it.
		if h.runs[id] == rs && len(rs.subscribers) == 0 && !rs.loop {
			delete(h.runs, id)
		}
	}
	return replay, ch, cancel
}

// Publish records a run's own event and claims the run's state for the loop.
func (h *Hub) Publish(id string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.run(id)
	rs.loop = true
	rs.deliver(ev)
}

// PublishLive is Publish for an event that only makes sense while a run is
// streaming. It drops the event when no run state exists rather than creating it,
// which plain Publish does -- a late frame would otherwise resurrect state Complete
// just evicted, and nothing would clean it up again. The check and the delivery
// share one critical section for that reason: releasing the lock between them is all
// a concurrent Complete needs to slip through. Concurrent runs for one goal share
// this key, so a still-running run can publish after a sibling completed; that is
// exactly the case this guards.
func (h *Hub) PublishLive(id string, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.runs[id]
	if rs == nil {
		return
	}
	rs.deliver(ev)
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
