package heuristics_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
)

func TestQueryAndTrace(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	testutil.TruncateEmbeddings(t, ctx, cfg)

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		t.Fatalf("connect neo4j: %v", err)
	}
	t.Cleanup(func() { repo.Close(ctx) })
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	p, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(p.Close)
	embeddings := store.NewEmbeddingStore(p)
	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)

	// Seed a full triplet and a Meta-Heuristic abstracted from the intervention.
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	mustCreate(t, repo.CreateState(ctx, domain.State{ID: stateID, Properties: map[string]any{"latency_ms": 210.0}}))
	mustCreate(t, repo.CreateIntervention(ctx, domain.Intervention{ID: interventionID, Type: domain.InterventionQuery}))
	mustCreate(t, repo.CreateOutcome(ctx, domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified}))
	mustCreate(t, repo.CreatePreConditionFor(ctx, stateID, interventionID))
	mustCreate(t, repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: -12, Confidence: 1.0}))

	mhID := testutil.NewID(t)
	definition := "reducing the alert threshold restores latency without degrading recall"
	mustCreate(t, repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: definition}, []string{interventionID}))

	// Embed the stored definition (document side) and clear the pending flag.
	docVec, err := provider.EmbedDocument(ctx, definition)
	if err != nil {
		t.Fatalf("embed document: %v", err)
	}
	if err := embeddings.Upsert(ctx, mhID, docVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}
	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}

	svc := heuristics.NewService(provider, embeddings, repo)

	matches, err := svc.Query(ctx, "latency climbing above the threshold", 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 1 || matches[0].MetaHeuristic.ID != mhID {
		t.Fatalf("expected seeded meta-heuristic, got %+v", matches)
	}

	triplets, err := svc.Trace(ctx, mhID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(triplets) != 1 || triplets[0].Intervention.ID != interventionID {
		t.Fatalf("expected one triplet for the intervention, got %+v", triplets)
	}
}

func mustCreate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// The drift tests below run on fakes: the retire contract turns on faults the
// live stores cannot be made to produce on cue -- a delete that fails for one
// id, a graph that is unreachable rather than empty, and a cancel landing
// mid-batch.

type fakeProvider struct {
	embedErr error
}

func (f fakeProvider) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return []float32{0.1, 0.2}, nil
}
func (f fakeProvider) EmbedDocument(_ context.Context, _ string) ([]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return []float32{0.1, 0.2}, nil
}
func (fakeProvider) Dimensions() int { return 2 }

// fakeEmbeddings records attempted deletes separately from successful ones: a
// delete that fails still proves the retire was reached, which is what pins the
// corroboration threshold, and the two lists diverging is what pins that one
// failure does not abandon the rest of the batch.
//
// Both methods honour the context they are handed. That is what makes the
// retire's detachment from the request observable -- without it, dropping the
// detachment entirely would leave every test still passing.
type fakeEmbeddings struct {
	hits         []string
	attempted    []string
	deleted      []string
	searchErr    error
	deleteErr    error
	deleteErrFor map[string]error
}

func (f *fakeEmbeddings) SimilaritySearch(ctx context.Context, _ []float32, _ int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.hits, nil
}

func (f *fakeEmbeddings) Delete(ctx context.Context, nodeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.attempted = append(f.attempted, nodeID)
	if err := f.deleteErrFor[nodeID]; err != nil {
		return err
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, nodeID)
	return nil
}

// fakeRepo resolves only the ids in present, omitting every other id from the
// result the way the batched Neo4j read does. lookups counts the calls, which is
// what pins hydration at one graph round trip per query.
//
// It always answers in reverse, because Neo4j's own batch order is arbitrary and
// returning the requested order would be the one arrangement in which a caller
// that forgot to re-key by id still looks correct. Every test here is therefore
// also an ordering test.
type fakeRepo struct {
	present  map[string]domain.MetaHeuristic
	failWith error
	lookups  int
	// onLookup runs before the answer, so a test can cancel the request context
	// the way a disconnecting client would.
	onLookup func()
}

func (f *fakeRepo) GetMetaHeuristics(_ context.Context, ids []string) ([]domain.MetaHeuristic, error) {
	f.lookups++
	if f.onLookup != nil {
		f.onLookup()
	}
	if f.failWith != nil {
		return nil, fmt.Errorf("get %d MetaHeuristics: %w", len(ids), f.failWith)
	}
	out := make([]domain.MetaHeuristic, 0, len(ids))
	for _, id := range ids {
		if mh, ok := f.present[id]; ok {
			out = append(out, mh)
		}
	}
	slices.Reverse(out)
	return out, nil
}

func (f *fakeRepo) TraceCausalChain(_ context.Context, _ string) ([]graph.CausalTriplet, error) {
	return nil, nil
}

// TestQuerySkipsAndRetiresOrphanedEmbedding: a single embedding whose node the
// graph no longer has must not fail the search that happens to rank it, and must
// be retired so the stores reconverge.
func TestQuerySkipsAndRetiresOrphanedEmbedding(t *testing.T) {
	embeddings := &fakeEmbeddings{hits: []string{"live-a", "orphan", "live-b"}}
	repo := &fakeRepo{present: map[string]domain.MetaHeuristic{
		"live-a": {ID: "live-a"},
		"live-b": {ID: "live-b"},
	}}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	matches, err := svc.Query(context.Background(), "any state", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 2 || matches[0].MetaHeuristic.ID != "live-a" || matches[1].MetaHeuristic.ID != "live-b" {
		t.Fatalf("expected the two live heuristics in rank order, got %+v", matches)
	}
	if len(embeddings.deleted) != 1 || embeddings.deleted[0] != "orphan" {
		t.Fatalf("expected only the orphan retired, got %v", embeddings.deleted)
	}
}

// TestQueryHydratesTheBatchInRankOrder catches the two ways the batched read can
// go wrong: reverting to a per-id fetch, and re-assembling from the batch result
// instead of the ranked ids, which hands back a ranking that is not one.
func TestQueryHydratesTheBatchInRankOrder(t *testing.T) {
	embeddings := &fakeEmbeddings{hits: []string{"nearest", "middle", "farthest"}}
	repo := &fakeRepo{present: map[string]domain.MetaHeuristic{
		"nearest": {ID: "nearest"}, "middle": {ID: "middle"}, "farthest": {ID: "farthest"},
	}}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	matches, err := svc.Query(context.Background(), "any state", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got := make([]string, 0, len(matches))
	for _, m := range matches {
		got = append(got, m.MetaHeuristic.ID)
	}
	want := []string{"nearest", "middle", "farthest"}
	if !slices.Equal(got, want) {
		t.Fatalf("match order = %v, want the similarity order %v", got, want)
	}
	if repo.lookups != 1 {
		t.Fatalf("graph lookups = %d, want exactly 1 for the whole batch", repo.lookups)
	}
}

// TestQueryKeepsOrphansWhenNothingResolves is the guard against the cascade. An
// empty-but-healthy graph reports every id as not-found, and treating that as
// per-row drift would delete the whole table -- unrecoverably, since resume keys
// off embedding_pending, already false for anything embedded.
func TestQueryKeepsOrphansWhenNothingResolves(t *testing.T) {
	embeddings := &fakeEmbeddings{hits: []string{"orphan-a", "orphan-b"}}
	svc := heuristics.NewService(fakeProvider{}, embeddings, &fakeRepo{})

	matches, err := svc.Query(context.Background(), "any state", 2)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %+v", matches)
	}
	if len(embeddings.deleted) != 0 {
		t.Fatalf("expected no embedding retired without a live match, got %v", embeddings.deleted)
	}
}

// TestQueryPropagatesGraphFailure separates drift from breakage: an unreachable
// graph must fail the request, never be reported as an empty corpus and acted on
// by deleting the embeddings that were about to resolve.
func TestQueryPropagatesGraphFailure(t *testing.T) {
	embeddings := &fakeEmbeddings{hits: []string{"live-a"}}
	repo := &fakeRepo{failWith: errors.New("connection refused")}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	if _, err := svc.Query(context.Background(), "any state", 1); err == nil {
		t.Fatal("expected a transport failure to propagate")
	}
	if len(embeddings.deleted) != 0 {
		t.Fatalf("expected no embedding retired on a transport failure, got %v", embeddings.deleted)
	}
}

// TestQuerySurvivesRetireFailure keeps the cleanup subordinate to the answer: a
// search that found live heuristics is still a good search when the repair fails.
// Its batch holds exactly one live match, so the `attempted` assertion is also
// what pins a single live sibling as sufficient corroboration to retire at all.
func TestQuerySurvivesRetireFailure(t *testing.T) {
	embeddings := &fakeEmbeddings{hits: []string{"live-a", "orphan"}, deleteErr: errors.New("permission denied")}
	repo := &fakeRepo{present: map[string]domain.MetaHeuristic{"live-a": {ID: "live-a"}}}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	matches, err := svc.Query(context.Background(), "any state", 2)
	if err != nil {
		t.Fatalf("expected the search to survive a failed retire, got %v", err)
	}
	if len(embeddings.attempted) != 1 || embeddings.attempted[0] != "orphan" {
		t.Fatalf("expected the orphan retire to be attempted, got %v", embeddings.attempted)
	}
	if len(matches) != 1 || matches[0].MetaHeuristic.ID != "live-a" {
		t.Fatalf("expected the live heuristic, got %+v", matches)
	}
}

// TestQueryRetiresRestOfBatchAfterOneFailure pins that the retire loop treats
// each orphan independently. A row that cannot be deleted -- locked, or racing
// another reader -- must not strand the rest of the batch, or drift would only
// ever clear behind whichever id happens to sort first.
func TestQueryRetiresRestOfBatchAfterOneFailure(t *testing.T) {
	embeddings := &fakeEmbeddings{
		hits:         []string{"live-a", "orphan-a", "orphan-b"},
		deleteErrFor: map[string]error{"orphan-a": errors.New("row locked")},
	}
	repo := &fakeRepo{present: map[string]domain.MetaHeuristic{"live-a": {ID: "live-a"}}}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	matches, err := svc.Query(context.Background(), "any state", 3)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 1 || matches[0].MetaHeuristic.ID != "live-a" {
		t.Fatalf("expected the live heuristic, got %+v", matches)
	}
	if len(embeddings.attempted) != 2 || embeddings.attempted[0] != "orphan-a" || embeddings.attempted[1] != "orphan-b" {
		t.Fatalf("expected both orphans attempted, got %v", embeddings.attempted)
	}
	if len(embeddings.deleted) != 1 || embeddings.deleted[0] != "orphan-b" {
		t.Fatalf("expected only the non-failing orphan retired, got %v", embeddings.deleted)
	}
}

// TestQueryRetiresAfterRequestCancelled is the detachment contract: the repair is
// justified by a graph read that already happened, so a client hanging up
// mid-batch must not leave the drift in place for the next search to rediscover.
func TestQueryRetiresAfterRequestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	embeddings := &fakeEmbeddings{hits: []string{"live-a", "orphan"}}
	repo := &fakeRepo{
		present:  map[string]domain.MetaHeuristic{"live-a": {ID: "live-a"}},
		onLookup: cancel,
	}
	svc := heuristics.NewService(fakeProvider{}, embeddings, repo)

	if _, err := svc.Query(ctx, "any state", 2); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(embeddings.deleted) != 1 || embeddings.deleted[0] != "orphan" {
		t.Fatalf("expected the orphan retired despite cancellation, got %v", embeddings.deleted)
	}
}

// TestQueryEmptyCorpus is the healthy-but-empty case: no hits is an empty answer,
// not an error, and there is nothing to retire.
func TestQueryEmptyCorpus(t *testing.T) {
	embeddings := &fakeEmbeddings{}
	svc := heuristics.NewService(fakeProvider{}, embeddings, &fakeRepo{})

	matches, err := svc.Query(context.Background(), "any state", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %+v", matches)
	}
	if len(embeddings.attempted) != 0 {
		t.Fatalf("expected no retire attempt on an empty corpus, got %v", embeddings.attempted)
	}
}

// TestQueryPropagatesPreSearchFailures covers the two paths that fail before any
// hit is resolved, so neither can be mistaken for an empty corpus.
func TestQueryPropagatesPreSearchFailures(t *testing.T) {
	t.Run("embed", func(t *testing.T) {
		embeddings := &fakeEmbeddings{hits: []string{"live-a"}}
		svc := heuristics.NewService(fakeProvider{embedErr: errors.New("ollama down")}, embeddings, &fakeRepo{})
		if _, err := svc.Query(context.Background(), "any state", 1); err == nil {
			t.Fatal("expected an embed failure to propagate")
		}
		if len(embeddings.attempted) != 0 {
			t.Fatalf("expected no retire attempt, got %v", embeddings.attempted)
		}
	})
	t.Run("similarity search", func(t *testing.T) {
		embeddings := &fakeEmbeddings{searchErr: errors.New("pgvector down")}
		svc := heuristics.NewService(fakeProvider{}, embeddings, &fakeRepo{})
		if _, err := svc.Query(context.Background(), "any state", 1); err == nil {
			t.Fatal("expected a similarity-search failure to propagate")
		}
		if len(embeddings.attempted) != 0 {
			t.Fatalf("expected no retire attempt, got %v", embeddings.attempted)
		}
	})
}
