package sleepcycle

import (
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// leakyHarness scripts a winning run whose schema exposes a column name the
// abstraction can leak.
func leakyHarness(t *testing.T, definitions []string) *harness {
	t.Helper()
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
	h.sandbox.schema.Columns = append(h.sandbox.schema.Columns,
		sandboxColumn("HomePlanet"), sandboxColumn("Transported"))
	h.claude.definitions = definitions
	return h
}

// TestLeakedDefinitionTriggersExactlyOneRepair pins the bounded repair loop. The
// script carries two leaky definitions because a bounded loop needs one response
// per attempt plus the initial call — with only one, the repair call would read
// past the script, get a defaulted clean definition, and the skip assertion would
// pass for the wrong reason.
func TestLeakedDefinitionTriggersExactlyOneRepair(t *testing.T) {
	h := leakyHarness(t, []string{
		"segments where HomePlanet is high raise output",     // initial: leaks
		"[Primary Population Center] raises [System Output]", // repair: clean
	})

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.claude.abstractCalls != 1 || h.claude.repairCalls != 1 {
		t.Fatalf("expected one abstraction and exactly one repair, got %d and %d",
			h.claude.abstractCalls, h.claude.repairCalls)
	}
	if len(h.repo.heuristics) != 1 {
		t.Fatalf("the repaired definition should persist, got %d heuristics", len(h.repo.heuristics))
	}
}

// TestStillLeakingAfterRepairIsSkipped: after the bound, the segment is skipped
// rather than persisted — a leaky definition is a heuristic welded to one
// dataset, the opposite of what abstraction is for.
func TestStillLeakingAfterRepairIsSkipped(t *testing.T) {
	h := leakyHarness(t, []string{
		"HomePlanet drives Transported",
		"HomePlanet still drives Transported", // repair still leaks
	})

	if err := h.run(t); err != nil {
		t.Fatalf("an exhausted repair must be isolated, not terminal: %v", err)
	}
	if h.claude.repairCalls != maxAbstractionRepairs {
		t.Fatalf("repair calls = %d, want the bound %d", h.claude.repairCalls, maxAbstractionRepairs)
	}
	if len(h.repo.heuristics) != 0 {
		t.Fatalf("a still-leaky definition must not be persisted: %+v", h.repo.heuristics)
	}
	if got := h.audits.count("sleepcycle_abstraction_failure"); got != 1 {
		t.Fatalf("expected one abstraction-failure audit, got %d", got)
	}
}

// TestAbstractionFailureDoesNotAbortOtherSegments: per-macro-segment isolation.
func TestAbstractionFailureDoesNotAbortOtherSegments(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0, "a+c": 11.0}, "a", "b", "c")
	h.claude.abstractErr = errors.New("claude down")

	if err := h.run(t); err != nil {
		t.Fatalf("an abstraction error must never abort the run: %v", err)
	}
	if h.claude.abstractCalls != 2 {
		t.Fatalf("both segments must be attempted, got %d calls", h.claude.abstractCalls)
	}
	if got := h.audits.count("sleepcycle_abstraction_failure"); got != 2 {
		t.Fatalf("expected one failure audit per segment, got %d", got)
	}
}

// TestGetMetaHeuristicThreeWayBranch is the crash-safe progress rule. Progress is
// keyed off embedding_pending being false, not off node existence, so the three
// cases must behave differently.
func TestGetMetaHeuristicThreeWayBranch(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30

	t.Run("not found runs the abstraction", func(t *testing.T) {
		h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.claude.abstractCalls != 1 {
			t.Fatalf("a first-run segment must be abstracted, got %d calls", h.claude.abstractCalls)
		}
	})

	t.Run("a transport error skips without a Claude call", func(t *testing.T) {
		h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
		mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "b"))
		h.repo.getErr[mhID] = errors.New("neo4j unreachable")

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		// A wedged graph must not trigger a full re-run of every Claude call.
		if h.claude.abstractCalls != 0 {
			t.Fatalf("a transport error must skip the segment without abstracting, got %d calls", h.claude.abstractCalls)
		}
		if got := h.audits.count("sleepcycle_abstraction_failure"); got != 1 {
			t.Fatalf("expected one abstraction-failure audit, got %d", got)
		}
	})

	t.Run("an already-embedded node is fully skipped", func(t *testing.T) {
		h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
		mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "b"))
		h.repo.existing[mhID] = domain.MetaHeuristic{ID: mhID, Definition: "done", EmbeddingPending: false}

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.claude.abstractCalls != 0 || h.provider.calls != 0 {
			t.Fatalf("a finished segment must be skipped entirely, got %d abstractions and %d embeds",
				h.claude.abstractCalls, h.provider.calls)
		}
	})

	t.Run("a pending node re-runs only the embed tail", func(t *testing.T) {
		h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
		mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "b"))
		h.repo.existing[mhID] = domain.MetaHeuristic{ID: mhID, Definition: "written but unembedded", EmbeddingPending: true}

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.claude.abstractCalls != 0 {
			t.Fatalf("a written-but-unembedded node must not be re-abstracted, got %d calls", h.claude.abstractCalls)
		}
		if h.provider.calls == 0 || len(h.repo.cleared) == 0 {
			t.Fatal("the embed tail must be re-run and the pending flag cleared")
		}
	})
}

// TestResumePassSettlesPendingNodes: a node left flagged by a mid-write crash is
// finished on the next run, whichever goal triggered it.
func TestResumePassSettlesPendingNodes(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.pending = []domain.MetaHeuristic{{ID: "mh-orphan", Definition: "left behind", EmbeddingPending: true}}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.embeddings.upserted) != 1 || h.embeddings.upserted[0] != "mh-orphan" {
		t.Fatalf("the pending node must be re-embedded: %+v", h.embeddings.upserted)
	}
	if len(h.repo.cleared) != 1 || h.repo.cleared[0] != "mh-orphan" {
		t.Fatalf("the pending flag must be cleared after the upsert: %+v", h.repo.cleared)
	}
	if h.claude.abstractCalls != 0 {
		t.Fatal("the resume pass must not re-call Claude")
	}
}

// TestEmbedOrderSurvivesAFailedUpsert: the flag is cleared only after pgvector
// accepts the vector, so a failed upsert leaves the node retryable.
func TestEmbedOrderSurvivesAFailedUpsert(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.pending = []domain.MetaHeuristic{{ID: "mh-orphan", Definition: "left behind", EmbeddingPending: true}}
	h.embeddings.err = errors.New("pgvector down")

	if err := h.run(t); err != nil {
		t.Fatalf("a resume failure must be non-terminal: %v", err)
	}
	if len(h.repo.cleared) != 0 {
		t.Fatal("the pending flag must not be cleared when the vector write failed")
	}
	if _, ok := h.audits.find("sleepcycle_resume_failure"); !ok {
		t.Fatalf("expected a resume-failure audit: %+v", h.audits.records)
	}
}

func TestHeuristicInjectionIsAudited(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	rec, ok := h.audits.find("sleepcycle_heuristic_injection")
	if !ok {
		t.Fatalf("expected a heuristic-injection audit: %+v", h.audits.records)
	}
	if rec.eventType != "heuristic" {
		t.Fatalf("event type = %q, want the short categorical name", rec.eventType)
	}
	if rec.detail["meta_heuristic_id"] == nil || rec.detail["abstracted_from"] == nil {
		t.Fatalf("the audit must carry the node and its evidence: %+v", rec.detail)
	}
}

func sandboxColumn(name string) sandboxclient.Column {
	return sandboxclient.Column{Name: name, Type: "VARCHAR"}
}
