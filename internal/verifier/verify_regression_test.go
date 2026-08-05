package verifier

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// TestVerifyRegression exercises Engine B end to end at default config against the
// planted ground-truth structure: the true A→Y effect (a root cause, no confounders)
// survives adjustment and the refutation battery as causally_verified; the spurious
// X–Y association (confounded by Z) collapses under adjustment to confounded, naming
// Z; and the placebo on the true effect shows ~zero (placebo ≈ 1). It pins that the
// settled knobs do not spuriously kill the true effect on the fixture.
func TestVerifyRegression(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())

	t.Run("true effect is causally verified", func(t *testing.T) {
		g := &fakeGraph{found: true, graphValue: groundGraph(),
			intervention: finding("a-finding", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
		v := &fakeVerifications{accepted: true, completeHeld: true}
		audit := &fakeAudit{}
		w := newVerifyWorker(newEffectAnalyzer(ds), g, maximizeYGoal(), v, audit)

		if err := w.VerifyOne(context.Background(), "goal", "a-finding", "ref", true); err != nil {
			t.Fatalf("VerifyOne: %v", err)
		}
		if v.completeStatus != string(outcomeCausallyVerified) {
			t.Fatalf("true A→Y effect status = %q, want causally_verified", v.completeStatus)
		}
		if v.refScore == nil || *v.refScore < 0.8 {
			t.Fatalf("refutation score = %v, want >= 0.8", v.refScore)
		}
		if g.causalWrites != 1 {
			t.Fatalf("a verified effect must write one causal edge, got %d", g.causalWrites)
		}
		if g.writtenEdge.EpistemicSource != domain.EpistemicCausalInferred {
			t.Fatalf("written edge epistemic source = %q, want causal_inferred", g.writtenEdge.EpistemicSource)
		}
		if placebo := placeboFromEvents(audit); placebo < 0.8 {
			t.Fatalf("placebo on the true effect = %.3f, want ~1 (>= 0.8)", placebo)
		}
	})

	t.Run("spurious effect is confounded", func(t *testing.T) {
		g := &fakeGraph{found: true, graphValue: groundGraph(),
			intervention: finding("x-finding", thresholdSegment(groundtruth.ColX, domain.GreaterThanOrEqual, 0))}
		v := &fakeVerifications{accepted: true, completeHeld: true}
		w := newVerifyWorker(newEffectAnalyzer(ds), g, maximizeYGoal(), v, &fakeAudit{})

		if err := w.VerifyOne(context.Background(), "goal", "x-finding", "ref", true); err != nil {
			t.Fatalf("VerifyOne: %v", err)
		}
		if v.completeStatus != string(outcomeConfounded) {
			t.Fatalf("spurious X–Y effect status = %q, want confounded", v.completeStatus)
		}
		if !contains(v.adjustmentSet, groundtruth.ColZ) {
			t.Fatalf("confounded verdict must name Z in the adjustment set, got %v", v.adjustmentSet)
		}
		if g.causalWrites != 0 || g.supersedes != 1 {
			t.Fatalf("a confounded effect must supersede, not write (writes=%d supersedes=%d)", g.causalWrites, g.supersedes)
		}
	})
}

// placeboFromEvents pulls the placebo score out of the published refutation-detail
// event.
func placeboFromEvents(audit *fakeAudit) float64 {
	for _, ev := range audit.events {
		if ev["type"] == "verification_refutation" {
			if p, ok := ev["placebo"].(float64); ok {
				return p
			}
		}
	}
	return 0
}
