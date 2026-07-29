package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleStream streams a run's progress as Server-Sent Events. It subscribes to
// the hub, replays the buffered history first (covering the subscribe-after-
// trigger race), then writes live events until the request context is done or
// the run completes. A dropped connection unsubscribes but must not cancel the
// loop, which runs on its own context.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// Resolve the goal before subscribing, as the other /goals/{id} handlers do.
	// Subscribing lazily creates run state, and the keepalive below holds an
	// otherwise-silent stream open indefinitely, so an unknown id would pin a
	// goroutine and a buffer that nothing reaps.
	goal, ok := s.lookupGoal(r.Context(), w, r.PathValue("id"))
	if !ok {
		return
	}
	setSSEHeaders(w)
	// Flush the head before waiting on anything: a blocking-mode run can be
	// silent for a long time, and until the first frame lands a client cannot
	// even see its status code -- an open connection is indistinguishable from a
	// hang.
	flusher.Flush()

	replay, ch, cancel := s.hub.Subscribe(goal.OptimizationFunctionID)
	defer cancel()

	for _, ev := range replay {
		if !writeSSEFrame(w, flusher, ev) {
			return
		}
	}
	keepalive := time.NewTicker(s.keepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if !writeSSEKeepalive(w, flusher) {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !writeSSEFrame(w, flusher, ev) {
				return
			}
		}
	}
}

// defaultKeepaliveInterval paces the comment frames that hold an idle stream
// open. A run in blocking epoch mode emits nothing while it waits on a human,
// and intermediaries reap connections that go quiet for minutes, so silence has
// to be filled to keep the view attached across a review.
const defaultKeepaliveInterval = 30 * time.Second

// setSSEHeaders declares a response an event stream.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// writeSSEFrame writes one frame in the service's wire format -- `data: <json>`
// with no `event:` line, leaving clients to discriminate on a top-level type
// field -- and reports whether the write reached the client. The flush is
// load-bearing: without it net/http buffers small writes and both feeds degrade
// into a single response at the end of the turn.
func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, frame any) bool {
	b, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// writeSSEKeepalive writes an SSE comment frame, which the protocol defines as
// ignorable padding -- clients never surface it as an event -- and reports
// whether the write reached the client.
func writeSSEKeepalive(w http.ResponseWriter, flusher http.Flusher) bool {
	if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
