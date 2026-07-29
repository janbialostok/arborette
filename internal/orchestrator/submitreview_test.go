package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
)

// submitWithReview registers a goal through the real multipart form, which is
// the only way an analyst ever sets the review settings.
func submitWithReview(t *testing.T, srv *Server, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	fields["goal"] = "extract the invoice fields"
	body, contentType := multipartBody(t, fields, "file", "invoice.pdf", "%PDF-1.4 fake")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// documentIntakeServer registers document goals, the only kind the review
// settings govern -- and the path a tabular-only test would miss entirely.
func documentIntakeServer(goals *fakeGoals) *Server {
	claude := &fakeClaude{fields: []domain.TargetField{{Name: "invoice_total"}}}
	sandbox := &fakeSandbox{introspect: IntrospectResponse{
		Schema: schemaDTO{Kind: "document"},
		Sample: "Invoice Total: 1296.00",
	}}
	return newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
}

func TestSubmitGoalPersistsReviewSettings(t *testing.T) {
	t.Run("a document goal carries the analyst's overrides", func(t *testing.T) {
		goals := &fakeGoals{}
		rec := submitWithReview(t, documentIntakeServer(goals), map[string]string{
			"confidence_threshold": "0.55",
			"epoch_mode":           "blocking",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		if goals.inserted == nil {
			t.Fatal("no goal was persisted")
		}
		if goals.inserted.ConfidenceThreshold == nil || *goals.inserted.ConfidenceThreshold != 0.55 {
			t.Fatalf("threshold = %v, want 0.55 persisted on the document goal", goals.inserted.ConfidenceThreshold)
		}
		if goals.inserted.EpochMode != store.EpochBlocking {
			t.Fatalf("epoch mode = %q, want blocking", goals.inserted.EpochMode)
		}
	})

	t.Run("a tabular goal carries them too", func(t *testing.T) {
		goals := &fakeGoals{}
		claude := &fakeClaude{matrix: fittedMatrix()}
		sandbox := &fakeSandbox{
			introspect: revenueSchema(),
			execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
		}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

		body, contentType := multipartBody(t, map[string]string{
			"goal":                 "grow revenue",
			"confidence_threshold": "0.7",
			"epoch_mode":           "blocking",
		}, "file", "data.csv", "revenue\n10\n")
		req := httptest.NewRequest(http.MethodPost, "/goals", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		if goals.inserted.ConfidenceThreshold == nil || *goals.inserted.ConfidenceThreshold != 0.7 ||
			goals.inserted.EpochMode != store.EpochBlocking {
			t.Fatalf("tabular goal did not carry the review settings: %+v", goals.inserted)
		}
	})

	t.Run("omitting them leaves the goal on the service defaults", func(t *testing.T) {
		goals := &fakeGoals{}
		rec := submitWithReview(t, documentIntakeServer(goals), map[string]string{})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		if goals.inserted.ConfidenceThreshold != nil || goals.inserted.EpochMode != "" {
			t.Fatalf("expected no overrides persisted: %+v", goals.inserted)
		}
	})
}

func TestSubmitGoalRejectsInvalidReviewSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"threshold above 1":  {"confidence_threshold": "1.5"},
		"threshold at zero":  {"confidence_threshold": "0"},
		"negative threshold": {"confidence_threshold": "-0.5"},
		"non-numeric":        {"confidence_threshold": "high"},
		"unknown epoch mode": {"epoch_mode": "eventually"},
		"infinite threshold": {"confidence_threshold": "Inf"},
		"negative infinity":  {"confidence_threshold": "-Inf"},
		// NaN is the one bad value a range check alone cannot catch: every
		// comparison against it is false, so it would pass straight through and
		// then make no extraction ever clear the threshold.
		"not-a-number literal": {"confidence_threshold": "NaN"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			goals := &fakeGoals{}
			rec := submitWithReview(t, documentIntakeServer(goals), fields)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if goals.inserted != nil {
				t.Fatalf("a rejected registration must persist nothing: %+v", goals.inserted)
			}
		})
	}
}

// effectiveThreshold is what the routing decision reads, so a goal's override
// has to win over the service default there and not only in the list response.
func TestEffectiveThresholdPrefersTheGoalOverride(t *testing.T) {
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	if got := srv.effectiveThreshold(store.Goal{}); got != testHITLThreshold {
		t.Fatalf("threshold = %v, want the service default %v", got, testHITLThreshold)
	}
	override := 0.25
	if got := srv.effectiveThreshold(store.Goal{ConfidenceThreshold: &override}); got != override {
		t.Fatalf("threshold = %v, want the goal's %v", got, override)
	}
}
