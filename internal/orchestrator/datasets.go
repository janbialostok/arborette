package orchestrator

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// datasetDTO selects and snake_cases the dataset fields the web UI renders. It
// is deliberately separate from store.Dataset (whose DatasetStatus and
// time.Time fields have no json tags) so the wire shape does not leak internals.
// Usage is the derived lifecycle state ("empty"/"in_use") the contract lists
// alongside the objective count, computed at read time.
type datasetDTO struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Status         string    `json:"status"`
	Usage          string    `json:"usage"`
	ObjectiveCount int       `json:"objective_count"`
	DataSourceRef  string    `json:"data_source_ref"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// datasetDetailDTO extends the summary with the dataset's objectives (its
// children), ascending by creation, for the detail view.
type datasetDetailDTO struct {
	datasetDTO
	Objectives []objectiveDTO `json:"objectives"`
}

// objectiveDTO is the named objective shape shared by the dataset detail and the
// non-empty dataset-delete 409: DatasetID is the parent binding, and the audit
// spelling optimization_function_id is kept so the wire and the audit detail
// cannot drift on the goal's primary name. Status is each goal's latest run
// status (or the synthetic "no run"), synthesized the same way the goals list
// does, so the contract's ObjectiveSummary carries it.
type objectiveDTO struct {
	OptimizationFunctionID string    `json:"optimization_function_id"`
	GoalText               string    `json:"goal_text"`
	DatasetID              string    `json:"dataset_id"`
	Status                 string    `json:"status"`
	CreatedAt              time.Time `json:"created_at"`
}

func toDatasetDTO(d store.Dataset) datasetDTO {
	usage := "empty"
	if d.ObjectiveCount > 0 {
		usage = "in_use"
	}
	return datasetDTO{
		ID:             d.ID,
		Name:           d.Name,
		Description:    d.Description,
		Status:         string(d.Status),
		Usage:          usage,
		ObjectiveCount: d.ObjectiveCount,
		DataSourceRef:  d.DataSourceRef,
		CreatedAt:      d.CreatedAt,
		UpdatedAt:      d.UpdatedAt,
	}
}

// objectivesWithStatus projects goals into the ObjectiveSummary shape, with each
// goal's latest run status (or the synthetic "no run") in place of the
// ephemeral live stream. It is shared by the dataset detail view and the
// non-empty delete 409 so both surfaces report the same status.
func (s *Server) objectivesWithStatus(ctx context.Context, datasetID string, goals []store.Goal) ([]objectiveDTO, error) {
	ids := make([]string, 0, len(goals))
	for _, g := range goals {
		ids = append(ids, g.OptimizationFunctionID)
	}
	latest, err := s.runs.LatestByGoal(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("latest runs by goal: %w", err)
	}
	out := make([]objectiveDTO, 0, len(goals))
	for _, g := range goals {
		o := objectiveDTO{
			OptimizationFunctionID: g.OptimizationFunctionID,
			GoalText:               g.GoalText,
			DatasetID:              datasetID,
			Status:                 "no run",
			CreatedAt:              g.CreatedAt,
		}
		if run, ok := latest[g.OptimizationFunctionID]; ok {
			o.Status = string(run.Status)
		}
		out = append(out, o)
	}
	return out, nil
}

// writeDatasetBindErr maps a dataset lookup failure during goal binding: an
// unknown dataset is refused as the conflict the contract names (the goal cannot
// be parented), anything else is a server fault.
func (s *Server) writeDatasetBindErr(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		service.WriteErr(w, http.StatusConflict, "dataset not found")
		return
	}
	log.Printf("orchestrator: bind goal to dataset: %v", err)
	service.WriteErr(w, http.StatusInternalServerError, "internal error")
}

// datasetForRef resolves the implicit dataset that wraps a legacy-ingested ref,
// reusing an existing row (a 0015 backfill or an earlier implicit registration)
// instead of minting a duplicate around the same ref.
func (s *Server) datasetForRef(ctx context.Context, ref string) (string, error) {
	return resolveDatasetForRef(ctx, s.datasets, ref)
}

// resolveDatasetForRef is the shared implicit-dataset resolution: find the
// dataset bound to the ref, or create it with the deterministic auto-name. A
// create race on that name falls back to reading the winner's row, so two
// concurrent legacy registrations of the same ref converge on one parent.
func resolveDatasetForRef(ctx context.Context, targets datasetStore, ref string) (string, error) {
	existing, err := targets.GetByRef(ctx, ref)
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up dataset for ref: %w", err)
	}
	id, err := targets.Create(ctx, store.Dataset{
		Name:          implicitDatasetName(ref),
		Description:   "",
		Status:        store.DatasetActive,
		DataSourceRef: ref,
	})
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, store.ErrNameConflict) {
		return "", fmt.Errorf("create implicit dataset: %w", err)
	}
	// A concurrent legacy registration minted the same implicit name; take the
	// dataset that won the race.
	winner, werr := targets.GetByRef(ctx, ref)
	if werr != nil {
		return "", fmt.Errorf("resolve raced implicit dataset: %w", werr)
	}
	return winner.ID, nil
}

// ReconcileDatasets is the boot-time safety net for the objective->dataset
// hierarchy: a goal that reached the registry without a parent (a row written
// before the 0015 migration on a partially migrated stack) is bound to the
// dataset for its ref -- reused or created implicitly -- and the reassignment is
// audited as dataset_reconcile so legacy reconciliation is visible for review.
// Failures are logged, never fatal: one unreachable ref must not stop serving.
func ReconcileDatasets(ctx context.Context, goals goalStore, targets datasetStore, audits auditStore, identity Identity) {
	all, err := goals.List(ctx)
	if err != nil {
		log.Printf("orchestrator: reconcile datasets: list goals: %v", err)
		return
	}
	reconciled := 0
	for _, g := range all {
		if g.DatasetID != "" {
			continue
		}
		datasetID, derr := resolveDatasetForRef(ctx, targets, g.DataSourceRef)
		if derr != nil {
			log.Printf("orchestrator: reconcile datasets: bind goal %q: %v", g.OptimizationFunctionID, derr)
			continue
		}
		if serr := goals.SetDatasetID(ctx, g.OptimizationFunctionID, datasetID); serr != nil {
			log.Printf("orchestrator: reconcile datasets: reassign goal %q: %v", g.OptimizationFunctionID, serr)
			continue
		}
		analyst, aerr := identity.Current(ctx)
		if aerr != nil {
			log.Printf("orchestrator: reconcile datasets: identity: %v", aerr)
			continue
		}
		if err := audits.Append(ctx, store.AuditRecord{
			Actor:     analyst.ID,
			Action:    "dataset_reconcile",
			EventType: "goal",
			Detail: map[string]any{
				"optimization_function_id": g.OptimizationFunctionID,
				"data_source_ref":          g.DataSourceRef,
				"dataset_id":               datasetID,
			},
		}); err != nil {
			log.Printf("orchestrator: reconcile datasets: audit goal %q: %v", g.OptimizationFunctionID, err)
		}
		reconciled++
	}
	if reconciled > 0 {
		log.Printf("orchestrator: reconciled %d goal(s) without a dataset into the hierarchy", reconciled)
	}
}

// implicitDatasetName derives a deterministic, ref-specific dataset name for the
// legacy submission path, mirroring the 0015 backfill's scheme (lowercased base
// filename plus a short md5 of the ref) so a ref never generates two colliding
// auto-names.
func implicitDatasetName(ref string) string {
	parts := strings.Split(strings.TrimSuffix(ref, "/"), "/")
	base := parts[len(parts)-1]
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, base)
	digest := fmt.Sprintf("%x", md5.Sum([]byte(ref)))
	return strings.ToLower(cleaned) + "-" + digest[:6]
}

// handleListDatasets serves the inventory the header Datasets link lands on,
// optionally narrowed by a case-insensitive name substring (?q).
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	datasets, err := s.datasets.List(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		log.Printf("orchestrator: list datasets: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]datasetDTO, 0, len(datasets))
	for _, d := range datasets {
		out = append(out, toDatasetDTO(d))
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// handleCreateDataset registers a new dataset around a data source, mirroring
// POST /goals ingest: a multipart form carrying name (+ description) and exactly
// one of file / import_path, whose source is put in the object store under
// datasources/<uuid>/<filename> and bound to the dataset. An ingest failure is
// surfaced through the shared ingest-error mapping (502 for infrastructure, 400
// for a missing source); a duplicate (case-insensitive) name is the 409 the
// inventory form surfaces.
func (s *Server) handleCreateDataset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid or oversized multipart form")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		service.WriteErr(w, http.StatusBadRequest, "name is required")
		return
	}
	description := r.FormValue("description")

	ref, err := s.ingest(r)
	if err != nil {
		s.writeIngestErr(w, err)
		return
	}
	if err := s.goals.RegisterDataSourceRef(ctx, ref); err != nil {
		log.Printf("orchestrator: register data source ref: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	id, err := s.datasets.Create(ctx, store.Dataset{
		Name:          name,
		Description:   description,
		DataSourceRef: ref,
	})
	if err != nil {
		if errors.Is(err, store.ErrNameConflict) {
			service.WriteErr(w, http.StatusConflict, "dataset name already in use")
			return
		}
		log.Printf("orchestrator: create dataset: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	d, err := s.datasets.Get(ctx, id)
	if err != nil {
		log.Printf("orchestrator: read back dataset %s: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.recordAudit(ctx, "dataset_create", "dataset", map[string]any{
		"dataset_id":      id,
		"dataset_name":    name,
		"data_source_ref": ref,
	}); err != nil {
		log.Printf("orchestrator: audit dataset create: %v", err)
	}
	service.WriteJSON(w, http.StatusCreated, toDatasetDTO(d))
}

type updateDatasetRequest struct {
	Name          *string              `json:"name"`
	Description   *string              `json:"description"`
	Status        *store.DatasetStatus `json:"status"`
	DataSourceRef *string              `json:"datasource_ref"`
}

// handleUpdateDataset edits a dataset's metadata only -- the data source
// binding is immutable once a dataset exists, so a datasource_ref in the body
// is rejected outright rather than silently ignored. Absent fields keep their
// current value (PATCH semantics); a rename onto an existing name is the 409
// conflict, matching the create path.
func (s *Server) handleUpdateDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.lookupDataset(w, r, id)
	if !ok {
		return
	}
	var req updateDatasetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef != nil {
		service.WriteErr(w, http.StatusBadRequest, "datasource_ref is immutable and cannot be changed")
		return
	}
	name := existing.Name
	if req.Name != nil {
		if *req.Name == "" {
			service.WriteErr(w, http.StatusBadRequest, "name is required")
			return
		}
		name = *req.Name
	}
	description := existing.Description
	if req.Description != nil {
		description = *req.Description
	}
	status := existing.Status
	if req.Status != nil {
		switch *req.Status {
		case store.DatasetActive, store.DatasetArchived:
			status = *req.Status
		default:
			service.WriteErr(w, http.StatusBadRequest, "status must be active or archived")
			return
		}
	}
	if err := s.datasets.Update(r.Context(), id, name, description, status); err != nil {
		switch {
		case errors.Is(err, store.ErrNameConflict):
			service.WriteErr(w, http.StatusConflict, "dataset name already in use")
		case errors.Is(err, pgx.ErrNoRows):
			service.WriteErr(w, http.StatusNotFound, "dataset not found")
		default:
			log.Printf("orchestrator: update dataset %q: %v", id, err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if err := s.recordAudit(r.Context(), "dataset_update", "dataset", map[string]any{
		"dataset_id": id,
	}); err != nil {
		log.Printf("orchestrator: audit dataset update: %v", err)
	}
	updated, err := s.datasets.Get(r.Context(), id)
	if err != nil {
		log.Printf("orchestrator: read back dataset %s: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	service.WriteJSON(w, http.StatusOK, toDatasetDTO(updated))
}

// handleGetDataset serves one dataset's detail row with its objectives, the
// children the detail view and the delete confirmation render.
func (s *Server) handleGetDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.lookupDataset(w, r, id)
	if !ok {
		return
	}
	objectives, err := s.datasets.ListObjectives(r.Context(), id)
	if err != nil {
		log.Printf("orchestrator: list objectives for dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := datasetDetailDTO{datasetDTO: toDatasetDTO(d)}
	out.Objectives, err = s.objectivesWithStatus(r.Context(), id, objectives)
	if err != nil {
		log.Printf("orchestrator: synthesize objective status for dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// handleDeleteDataset removes a dataset. A dataset that still holds objectives
// is refused with 409 and a named list of whatever stands in the way; after the
// row is gone, a ref no longer referenced by any dataset or goal has its object
// and registry row retired too (refcount-aware cleanup).
func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.lookupDataset(w, r, id)
	if !ok {
		return
	}
	objectives, err := s.datasets.ListObjectives(r.Context(), id)
	if err != nil {
		log.Printf("orchestrator: list objectives for dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(objectives) > 0 {
		blocking, err := s.objectivesWithStatus(r.Context(), id, objectives)
		if err != nil {
			log.Printf("orchestrator: synthesize objective status for dataset %q: %v", id, err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		service.WriteJSON(w, http.StatusConflict, map[string]any{
			"error":      "dataset has objectives",
			"objectives": blocking,
		})
		return
	}
	if err := s.datasets.Delete(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			service.WriteErr(w, http.StatusNotFound, "dataset not found")
		default:
			log.Printf("orchestrator: delete dataset %q: %v", id, err)
			service.WriteErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	s.retireDataSourceRef(r, existing.DataSourceRef)
	if err := s.recordAudit(r.Context(), "dataset_delete", "dataset", map[string]any{
		"dataset_id":      id,
		"data_source_ref": existing.DataSourceRef,
	}); err != nil {
		log.Printf("orchestrator: audit dataset delete: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// retireDataSourceRef removes a data source's object and registry row once no
// dataset or goal references the ref anymore. Object deletion is best-effort
// (a straggler object is inert residue); a failed registry delete is logged,
// since the row is equally harmless while unreferenced.
func (s *Server) retireDataSourceRef(r *http.Request, ref string) {
	if ref == "" {
		return
	}
	datasets, goals, err := s.datasets.DataSourceRefUsage(r.Context(), ref)
	if err != nil {
		log.Printf("orchestrator: count ref %q usage: %v", ref, err)
		return
	}
	if datasets > 0 || goals > 0 {
		return
	}
	if err := s.objects.Delete(r.Context(), ref); err != nil {
		log.Printf("orchestrator: delete object for ref %q: %v", ref, err)
	}
	if err := s.datasets.DeleteDataSourceRef(r.Context(), ref); err != nil {
		log.Printf("orchestrator: delete data source ref %q: %v", ref, err)
	}
}

// lookupDataset fetches a dataset, writing a 404/500 and returning ok=false
// when nothing can be served.
func (s *Server) lookupDataset(w http.ResponseWriter, r *http.Request, id string) (store.Dataset, bool) {
	d, err := s.datasets.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			service.WriteErr(w, http.StatusNotFound, "dataset not found")
			return store.Dataset{}, false
		}
		log.Printf("orchestrator: get dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return store.Dataset{}, false
	}
	return d, true
}
