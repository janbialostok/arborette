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

// A subscriber watching a goal whose run is long finished creates the run state
// itself, and the verification frames it is there to receive land in that state's
// buffer. Complete is only ever called at a loop's end, so nothing would reap it
// afterwards -- the subscriber that brought it into being has to.
//
// The eviction tests assert on h.runs directly: the leak they guard IS the map entry,
// and an empty replay buffer would not prove the key is gone.
func TestHubEvictsSubscriberOnlyStateOnLastCancel(t *testing.T) {
	h := NewHub()
	_, ch, cancel := h.Subscribe("finished-goal")

	h.PublishLive("finished-goal", Event{Type: "causal_verification_dispatched"})
	if ev := <-ch; ev.Type != "causal_verification_dispatched" {
		t.Fatalf("live event = %q, want causal_verification_dispatched", ev.Type)
	}

	cancel()

	if _, ok := h.runs["finished-goal"]; ok {
		t.Fatal("state a subscriber created and only PublishLive fed was retained after the last unsubscribe")
	}
}

// A run's own state must survive a dropped connection: the loop is still writing to
// it, and a reconnect replays from it.
func TestHubKeepsLoopStateUntilComplete(t *testing.T) {
	h := NewHub()
	_, _, cancel := h.Subscribe("live-goal")
	h.Publish("live-goal", Event{Type: "triplet"})

	cancel()

	if _, ok := h.runs["live-goal"]; !ok {
		t.Fatal("a running loop's state was evicted when its subscriber dropped")
	}
	h.Complete("live-goal")
	if _, ok := h.runs["live-goal"]; ok {
		t.Fatal("Complete left the run's state behind")
	}
}

// A subscriber that arrives during a run and leaves after it must not resurrect
// what Complete evicted: PublishLive drops the frame, so no state comes back.
func TestHubDropsLiveEventAfterComplete(t *testing.T) {
	h := NewHub()
	h.Publish("goal", Event{Type: "triplet"})
	h.Complete("goal")

	h.PublishLive("goal", Event{Type: "verification"})

	if _, ok := h.runs["goal"]; ok {
		t.Fatal("PublishLive resurrected state Complete had evicted")
	}
}

// The eviction rule is a conjunction, and a goal with two open streams is the
// ordinary case. Evicting on the first cancel would leave the sibling attached to
// state the hub no longer knows about, after which PublishLive's liveness check
// drops every frame published to it.
func TestHubKeepsStateWhileAnotherSubscriberHoldsIt(t *testing.T) {
	h := NewHub()
	_, _, cancelFirst := h.Subscribe("goal")
	_, _, cancelSecond := h.Subscribe("goal")

	cancelFirst()

	if _, ok := h.runs["goal"]; !ok {
		t.Fatal("state was evicted while a second subscriber still held it")
	}

	cancelSecond()

	if _, ok := h.runs["goal"]; ok {
		t.Fatal("state outlived its last subscriber")
	}
}

// cancel captures its own run state but deletes by key, so a subscriber that outlives
// a Complete must not reap whatever took its place: a mis-eviction strands a live
// subscriber on state the hub no longer knows, silently dropping every frame
// published to it afterwards.
func TestHubCancelDoesNotEvictASuccessorState(t *testing.T) {
	h := NewHub()
	_, _, cancelStale := h.Subscribe("goal")
	h.Complete("goal")

	_, ch, _ := h.Subscribe("goal")
	cancelStale()

	h.PublishLive("goal", Event{Type: "verification"})
	select {
	case ev := <-ch:
		if ev.Type != "verification" {
			t.Fatalf("live event = %q, want verification", ev.Type)
		}
	default:
		t.Fatal("the stale cancel evicted the successor state, so the live subscriber got nothing")
	}
}
