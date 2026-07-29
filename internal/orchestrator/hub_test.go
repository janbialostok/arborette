package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// The replay buffer is sized to hold a whole run's triplets. A cumulative event
// published per triplet would crowd them out, so it replaces its predecessor
// instead of accumulating -- and stays where it was, keeping the replay in the
// order events actually happened.
func TestHubCoalescesSupersedingEvents(t *testing.T) {
	h := NewHub()
	h.Publish("run", Event{Type: "confidence_distribution", Coalesce: true, Payload: map[string]any{"total": 1}})
	// Two same-type frames that do NOT coalesce: the buffer exists to replay
	// every one of these, so only the flagged type may collapse.
	h.Publish("run", Event{Type: "triplet", Payload: map[string]any{"n": 1}})
	h.Publish("run", Event{Type: "triplet", Payload: map[string]any{"n": 2}})
	h.Publish("run", Event{Type: "confidence_distribution", Coalesce: true, Payload: map[string]any{"total": 2}})
	h.Publish("run", Event{Type: "confidence_distribution", Coalesce: true, Payload: map[string]any{"total": 3}})

	replay, _, _ := h.Subscribe("run")
	if len(replay) != 3 {
		t.Fatalf("expected both triplets plus one coalesced frame, got %+v", replay)
	}
	if replay[0].Type != "confidence_distribution" || replay[1].Type != "triplet" || replay[2].Type != "triplet" {
		t.Fatalf("coalescing must not reorder the replay or collapse unflagged frames, got %+v", replay)
	}
	if replay[1].Payload["n"] != 1 || replay[2].Payload["n"] != 2 {
		t.Fatalf("every unflagged frame must survive: %+v", replay)
	}
	if replay[0].Payload["total"] != 3 {
		t.Fatalf("the retained frame must be the newest, got %+v", replay[0].Payload)
	}
}

// Coalesce routes the hub; it is not part of the contract clients parse, so it
// must never appear in a frame on the wire.
func TestEventCoalesceStaysOffTheWire(t *testing.T) {
	frame, err := json.Marshal(Event{Type: "confidence_distribution", Coalesce: true})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if strings.Contains(strings.ToLower(string(frame)), "coalesce") {
		t.Fatalf("the hub's routing hint leaked into the wire frame: %s", frame)
	}
}

// A live subscriber sees every update as it happens; coalescing bounds the
// replay buffer, not the feed.
func TestHubCoalescingStillFansOutEveryUpdate(t *testing.T) {
	h := NewHub()
	_, ch, cancel := h.Subscribe("run")
	defer cancel()

	h.Publish("run", Event{Type: "confidence_distribution", Coalesce: true, Payload: map[string]any{"total": 1}})
	h.Publish("run", Event{Type: "confidence_distribution", Coalesce: true, Payload: map[string]any{"total": 2}})

	for _, want := range []int{1, 2} {
		if ev := <-ch; ev.Payload["total"] != want {
			t.Fatalf("live update = %+v, want total %d", ev.Payload, want)
		}
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
