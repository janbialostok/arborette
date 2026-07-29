package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	setSSEHeaders(w)

	replay, ch, cancel := s.hub.Subscribe(r.PathValue("id"))
	defer cancel()

	for _, ev := range replay {
		if !writeSSEFrame(w, flusher, ev) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
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
