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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	replay, ch, cancel := s.hub.Subscribe(r.PathValue("id"))
	defer cancel()

	for _, ev := range replay {
		if !writeEvent(w, flusher, ev) {
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
			if !writeEvent(w, flusher, ev) {
				return
			}
		}
	}
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, ev Event) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
