package orchestrator

import (
	"errors"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// handleDeleteGoal removes an objective and everything it owns: the store
// transaction deletes its runs and verifications before the goal row (guarding
// on in-flight work with the 409 the wire contract prescribes), then the goal's
// graph nodes and embedding rows are retired. The graph and embedding cleanup
// run after the durable delete commits and are best-effort: the authoritative
// deletion is the transaction, and a straggler node is inert residue, not a
// live objective. The detail keys follow the codebase-wide audit vocabulary
// (optimization_function_id / data_source_ref / dataset_id).
func (s *Server) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	goal, ok := s.lookupGoal(r.Context(), w, id, s.actingUser(r))
	if !ok {
		return
	}
	datasetID := s.datasetIDForRef(r, goal.DataSourceRef)

	if err := s.goals.Delete(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrGoalRunning), errors.Is(err, store.ErrVerificationPending):
			service.WriteErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, pgx.ErrNoRows):
			service.WriteErr(w, http.StatusNotFound, "goal not found")
		default:
			log.Printf("orchestrator: delete goal %q: %v", id, err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if err := s.repo.DeleteGoalGraph(r.Context(), id); err != nil {
		log.Printf("orchestrator: delete goal graph %q: %v", id, err)
	}
	if err := s.embeddings.DeleteByGoal(r.Context(), id); err != nil {
		log.Printf("orchestrator: delete goal embeddings %q: %v", id, err)
	}
	detail := map[string]any{
		"optimization_function_id": id,
		"data_source_ref":          goal.DataSourceRef,
	}
	if datasetID != "" {
		detail["dataset_id"] = datasetID
	}
	if err := s.recordAudit(r.Context(), "objective_delete", "goal", detail); err != nil {
		log.Printf("orchestrator: audit objective delete: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// datasetIDForRef resolves the dataset bound to a data source ref, for the
// hierarchical audit detail. Best-effort: a goal whose ref has no dataset (a
// legacy pre-backfill row cannot exist post-migration, but a ref is shared only
// within one dataset) yields empty and the detail omits dataset_id.
func (s *Server) datasetIDForRef(r *http.Request, ref string) string {
	datasets, err := s.datasets.List(r.Context(), "")
	if err != nil {
		log.Printf("orchestrator: list datasets for ref %q: %v", ref, err)
		return ""
	}
	for _, d := range datasets {
		if d.DataSourceRef == ref {
			return d.ID
		}
	}
	return ""
}
