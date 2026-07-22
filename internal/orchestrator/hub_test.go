package orchestrator

import "testing"

func TestHubReplaysAcrossSubscribers(t *testing.T) {
	h := NewHub()
	_, ch, cancel := h.Subscribe("run")

	h.Publish("run", Event{Type: "a"})
	if ev := <-ch; ev.Type != "a" {
		t.Fatalf("live event = %q, want a", ev.Type)
	}

	// A dropped SSE connection unsubscribes; it must not stop the run. Publishing
	// after the disconnect still buffers, and a fresh subscriber replays both.
	cancel()
	h.Publish("run", Event{Type: "b"})

	replay, _, _ := h.Subscribe("run")
	if len(replay) != 2 || replay[0].Type != "a" || replay[1].Type != "b" {
		t.Fatalf("expected both events replayed to a late subscriber, got %+v", replay)
	}
}

func TestHubCompleteClosesSubscribers(t *testing.T) {
	h := NewHub()
	_, ch, cancel := h.Subscribe("run")
	defer cancel() // must be a safe no-op after Complete already removed the channel

	h.Complete("run")
	if _, ok := <-ch; ok {
		t.Fatalf("expected the subscriber channel to be closed on Complete")
	}
	// The run is evicted; a new subscriber starts with an empty buffer.
	replay, _, _ := h.Subscribe("run")
	if len(replay) != 0 {
		t.Fatalf("expected an empty buffer after eviction, got %+v", replay)
	}
}
