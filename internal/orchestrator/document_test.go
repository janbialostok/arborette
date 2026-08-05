package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
)

// postDocumentGoal drives a valid PDF-upload goal submission through the mux.
func postDocumentGoal(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, map[string]string{"goal": "extract the key dates"}, "file", "contract.pdf", "%PDF-1.4 fake")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func documentIntrospect(sample string) IntrospectResponse {
	return IntrospectResponse{Schema: schemaDTO{Kind: string(datasource.KindDocument)}, Sample: sample}
}

func TestSubmitDocumentGoalPersistsTargetFields(t *testing.T) {
	goals := &fakeGoals{}
	fields := []domain.TargetField{
		{Name: "effective_date", Description: "the effective date"},
		{Name: "termination_date", Description: "the termination date"},
	}
	claude := &fakeClaude{fields: fields}
	sandbox := &fakeSandbox{introspect: documentIntrospect("EFFECTIVE DATE: 2024-01-01")}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postDocumentGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.gotSample != "EFFECTIVE DATE: 2024-01-01" {
		t.Fatalf("field introspection did not receive the sandbox sample: %q", claude.gotSample)
	}
	if goals.inserted == nil {
		t.Fatal("document goal was not persisted")
	}
	if !goals.inserted.IsDocument() || len(goals.inserted.TargetFields) != 2 {
		t.Fatalf("expected two target fields, got %+v", goals.inserted.TargetFields)
	}
	// A document goal must not carry a fitted tabular objective, and the tabular
	// generation path must never run.
	if len(goals.inserted.EvaluationMatrix.Targets) != 0 {
		t.Fatalf("document goal should carry no EvaluationMatrix targets: %+v", goals.inserted.EvaluationMatrix)
	}
	if claude.gotSchema.Columns != nil {
		t.Fatal("tabular matrix generation ran for a document goal")
	}
}

func TestSubmitDocumentGoalIntrospectionFaultIsBadGateway(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{fieldsErr: errors.New("claude down")}
	sandbox := &fakeSandbox{introspect: documentIntrospect("some text")}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postDocumentGoal(t, srv)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if goals.inserted != nil {
		t.Fatal("no goal should be persisted when field introspection faults")
	}
}

func TestSubmitDocumentGoalZeroFieldsIsUnprocessable(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{fields: nil}
	sandbox := &fakeSandbox{introspect: documentIntrospect("some text")}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postDocumentGoal(t, srv)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if goals.inserted != nil {
		t.Fatal("a zero-field document goal must not be persisted")
	}
}

func TestLocateProvenance(t *testing.T) {
	pages := []string{"first page text", "the effective date is January 1, 2024 here"}

	loc := locateProvenance(pages, "January 1, 2024")
	if loc == nil {
		t.Fatal("expected a locator for a value present in the text")
	}
	if loc.Page != 1 {
		t.Fatalf("page = %d, want 1", loc.Page)
	}
	if pages[loc.Page][loc.CharStart:loc.CharEnd] != "January 1, 2024" {
		t.Fatalf("offsets do not bracket the value: %q", pages[loc.Page][loc.CharStart:loc.CharEnd])
	}

	if locateProvenance(pages, "value not in the document") != nil {
		t.Fatal("expected nil locator for an absent value")
	}
	if locateProvenance(pages, "") != nil {
		t.Fatal("expected nil locator for an empty value")
	}
}

func TestRunDocumentLoopPerFieldSubTrees(t *testing.T) {
	repo := &fakeRepo{}
	goal := documentGoal([]domain.TargetField{
		{Name: "effective_date", Description: "the effective date"},
		{Name: "termination_date", Description: "the termination date"},
	})
	// A confident extraction whose value appears verbatim on page 1, so provenance
	// resolves; a confidence above the field-root baseline of 0 lets each root
	// method expand one level (and no further, since children tie the parent).
	claude := &fakeClaude{
		extractValue:      "January 1, 2024",
		extractConfidence: 0.8,
	}
	sandbox := &fakeSandbox{docPages: []string{"the effective date is January 1, 2024"}}
	objects := &fakeObjects{getData: []byte("%PDF-1.4 fake")}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)

	srv.runLoop(context.Background(), goal, "run-doc")

	if len(repo.outcomes) == 0 {
		t.Fatal("no extraction outcomes written")
	}
	// Every extraction outcome is unverified, carries the located provenance (the
	// value is on the page), is keyed by its field name, and its PRODUCED edge uses
	// the model's confidence (not the query path's fixed 1.0), observational.
	for _, o := range repo.outcomes {
		if o.VerificationStatus != domain.VerificationUnverified {
			t.Fatalf("outcome status = %q, want unverified", o.VerificationStatus)
		}
		if o.Provenance == nil {
			t.Fatalf("outcome missing provenance for a value present in the text: %+v", o)
		}
	}
	for _, e := range repo.produced {
		if e.Confidence != 0.8 {
			t.Fatalf("produced confidence = %v, want the model's 0.8 (not 1.0)", e.Confidence)
		}
		if e.EpistemicSource != domain.EpistemicObservational {
			t.Fatalf("epistemic source = %q, want observational", e.EpistemicSource)
		}
	}

	// Two fields, breadth-3 root methods each, and one level of expansion (children
	// tie the parent confidence and prune): 2 fields * 3 root * (1 + 3 children) = 24.
	if claude.extractCalls != 24 {
		t.Fatalf("extract calls = %d, want 24 (2 fields x 3 methods x depth-2 with pruning)", claude.extractCalls)
	}
	// The breadth axis exercises each competing extraction method.
	seen := map[string]bool{}
	for _, m := range claude.gotMethods {
		seen[m] = true
	}
	for _, m := range extractionMethods {
		if !seen[m] {
			t.Fatalf("expected method %q to be exercised as a breadth sibling", m)
		}
	}
}

func TestRunDocumentLoopOversizedPDFIsTerminal(t *testing.T) {
	runs := &fakeRuns{}
	repo := &fakeRepo{}
	// A PDF over the inline base64 limit cannot be extracted via the MVP path, so
	// the run fails once up front rather than 413-ing on every extraction node.
	claude := &fakeClaude{extractConfidence: 0.9}
	sandbox := &fakeSandbox{docPages: []string{"text"}}
	objects := &fakeObjects{getData: make([]byte, maxInlinePDFBytes+1)}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)
	srv.runs = runs

	srv.runLoop(context.Background(), documentGoal([]domain.TargetField{{Name: "effective_date"}}), "run-doc")

	if claude.extractCalls != 0 {
		t.Fatalf("no extraction should run for an oversized PDF, got %d calls", claude.extractCalls)
	}
	if len(runs.statusCalls) != 1 || runs.statusCalls[0].status != store.RunFailed {
		t.Fatalf("an oversized PDF should fail the run: %+v", runs.statusCalls)
	}
}

func TestRunDocumentLoopDocTextFailureIsTerminal(t *testing.T) {
	runs := &fakeRuns{}
	repo := &fakeRepo{}
	// The per-page text fetch fails: a run-fatal setup error, so the run settles
	// failed and no extraction is attempted.
	claude := &fakeClaude{extractConfidence: 0.9}
	sandbox := &fakeSandbox{docTextErr: &SandboxError{Status: http.StatusBadGateway, Message: "sandbox down"}}
	objects := &fakeObjects{getData: []byte("%PDF-1.4 fake")}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)
	srv.runs = runs

	srv.runLoop(context.Background(), documentGoal([]domain.TargetField{{Name: "effective_date"}}), "run-doc")

	if claude.extractCalls != 0 {
		t.Fatalf("no extraction should run when the text fetch fails, got %d calls", claude.extractCalls)
	}
	if len(repo.outcomes) != 0 {
		t.Fatalf("no outcomes should be written on a terminal setup failure, got %d", len(repo.outcomes))
	}
	assertOneStatus(t, runs, store.RunFailed, "sandbox down")
}

func TestRunDocumentLoopReadObjectFailureIsTerminal(t *testing.T) {
	runs := &fakeRuns{}
	repo := &fakeRepo{}
	// The raw-PDF fetch fails after the text fetch succeeds: still a run-fatal
	// setup error before any extraction.
	claude := &fakeClaude{extractConfidence: 0.9}
	sandbox := &fakeSandbox{docPages: []string{"some text"}}
	objects := &fakeObjects{getErr: errors.New("object gone")}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)
	srv.runs = runs

	srv.runLoop(context.Background(), documentGoal([]domain.TargetField{{Name: "effective_date"}}), "run-doc")

	if claude.extractCalls != 0 {
		t.Fatalf("no extraction should run when the PDF fetch fails, got %d calls", claude.extractCalls)
	}
	assertOneStatus(t, runs, store.RunFailed, "object gone")
}

func TestRunDocumentLoopExtractErrorIsNonTerminal(t *testing.T) {
	runs := &fakeRuns{}
	audits := &fakeAudits{}
	repo := &fakeRepo{}
	// Every extraction fails: each field-root method records a branch failure and
	// does not expand, but the run still completes (per-node failures are
	// non-terminal, mirroring the tabular per-candidate contract).
	claude := &fakeClaude{extractErr: errors.New("claude down")}
	sandbox := &fakeSandbox{docPages: []string{"some text"}}
	objects := &fakeObjects{getData: []byte("%PDF-1.4 fake")}
	srv := newTestServerRepo(repo, &fakeGoals{}, audits, objects, &fakeHeur{}, claude, sandbox)
	srv.runs = runs

	srv.runLoop(context.Background(), documentGoal([]domain.TargetField{{Name: "effective_date"}}), "run-doc")

	if len(repo.outcomes) != 0 {
		t.Fatalf("a failed extraction writes no triplet, got %d outcomes", len(repo.outcomes))
	}
	// One field, breadth-3 root methods, each failing with no expansion.
	if claude.extractCalls != len(extractionMethods) {
		t.Fatalf("extract calls = %d, want %d (one per root method, no expansion)", claude.extractCalls, len(extractionMethods))
	}
	found := false
	for _, r := range audits.records() {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hypothesis_branch_failure audit for the failed extraction: %+v", audits.records())
	}
	assertOneStatus(t, runs, store.RunCompleted, "")
}

func TestRunDocumentLoopPrunesOnNoImprovement(t *testing.T) {
	repo := &fakeRepo{}
	goal := documentGoal([]domain.TargetField{{Name: "effective_date", Description: "the effective date"}})
	// Confidence 0 never beats the field-root baseline of 0, so each root method
	// writes exactly one triplet and never expands. The extracted value is empty,
	// so no provenance locator resolves (the nil-provenance path through the write).
	claude := &fakeClaude{extractConfidence: 0.0}
	sandbox := &fakeSandbox{docPages: []string{"unrelated text"}}
	objects := &fakeObjects{getData: []byte("%PDF-1.4 fake")}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)

	srv.runLoop(context.Background(), goal, "run-doc")

	if claude.extractCalls != len(extractionMethods) {
		t.Fatalf("extract calls = %d, want %d (one per root method, no expansion)", claude.extractCalls, len(extractionMethods))
	}
	for _, o := range repo.outcomes {
		if o.Provenance != nil {
			t.Fatalf("an empty extracted value must yield no locator, got %+v", o.Provenance)
		}
	}
}

func documentGoal(fields []domain.TargetField) store.Goal {
	return store.Goal{OptimizationFunctionID: "doc-goal", GoalText: "extract dates", TargetFields: fields, DataSourceRef: "contract.pdf"}
}
