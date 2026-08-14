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
	// LastAccessedAt is when the detail view was last opened, NULL (nil) until
	// the first open. The inventory orders by it, so the web UI surfaces it to
	// explain the ranking.
	LastAccessedAt *time.Time `json:"last_accessed_at"`
	// OwnerID is the owning account id; it is what the web UI keys "owner" vs
	// "shared" markers and owner-only controls off of.
	OwnerID string `json:"owner_id"`
	// Access is the acting user's relationship to this dataset: "owner" when
	// they own it, "shared" when it was granted to them. The web UI renders the
	// neutral "shared with you" marker from it and hides owner-only controls
	// from non-owners.
	Access string `json:"access"`
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

// accessFor classifies the acting user's relationship to a dataset the access
// predicate has already admitted. Rows admitted through ListAccessible are
// always owner-or-shared; "owner" when the row's owner is the acting user,
// "shared" otherwise. The distinction is what the inventory's "shared with
// you" marker and the owner-only control hiding render off of.
func accessFor(d store.Dataset, actingUser string) string {
	if d.OwnerID != "" && d.OwnerID == actingUser {
		return "owner"
	}
	return "shared"
}

func toDatasetDTO(d store.Dataset, actingUser string) datasetDTO {
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
		LastAccessedAt: d.LastAccessedAt,
		OwnerID:        d.OwnerID,
		Access:         accessFor(d, actingUser),
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
	// Stable partition: objectives with an active run rank first so the analyst
	// sees what is happening right now, the rest follow newest-created-first as
	// ListByDataset already returns them. The partition preserves the order
	// within each group (running, then the rest) — no sort, just two appends.
	running := make([]objectiveDTO, 0, len(out))
	rest := make([]objectiveDTO, 0, len(out))
	for _, o := range out {
		if o.Status == "running" {
			running = append(running, o)
		} else {
			rest = append(rest, o)
		}
	}
	return append(running, rest...), nil
}

// writeDatasetBindErr maps a dataset lookup failure during goal binding: a
// dataset the acting user cannot reach -- unknown or exists-but-not-theirs -- is
// refused as the uniform 404 "dataset not found", so binding never leaks a
// dataset's existence any more than viewing it does. Anything else is a server
// fault.
func (s *Server) writeDatasetBindErr(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		service.WriteErr(w, http.StatusNotFound, "dataset not found")
		return
	}
	log.Printf("orchestrator: bind goal to dataset: %v", err)
	service.WriteErr(w, http.StatusInternalServerError, "internal error")
}

// datasetForRef resolves the implicit dataset that wraps a legacy-ingested ref,
// reusing an existing row (a 0015 backfill or an earlier implicit registration)
// instead of minting a duplicate around the same ref. A freshly minted implicit
// dataset is owned by the acting user -- the submission that created it is its
// creator (US3.3) -- so an unbound goal's ref never leaks a dataset to others.
func (s *Server) datasetForRef(ctx context.Context, ref, owner string) (string, error) {
	return resolveDatasetForRef(ctx, s.datasets, ref, owner)
}

// resolveDatasetForRef is the shared implicit-dataset resolution: find the
// dataset bound to the ref, or create it with the deterministic auto-name,
// owned by owner (the caretaker on a system/boot path). A create race on that
// name falls back to reading the winner's row, so two concurrent legacy
// registrations of the same ref converge on one parent.
func resolveDatasetForRef(ctx context.Context, targets datasetStore, ref, owner string) (string, error) {
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
		OwnerID:       owner,
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
// A dataset this sweep mints is owned by the admin caretaker (system-created
// objects follow the same ownership rules as user-created data, FR-003), never
// left ownerless. Failures are logged, never fatal: one unreachable ref must not
// stop serving.
func ReconcileDatasets(ctx context.Context, goals goalStore, targets datasetStore, users userStore, audits auditStore, identity Identity) {
	all, err := goals.List(ctx)
	if err != nil {
		log.Printf("orchestrator: reconcile datasets: list goals: %v", err)
		return
	}
	// The caretaker is the owner every boot-minted dataset is attributed to. A
	// stack with no admin yet (a fresh deployment) has nothing to attribute to,
	// so the mint is deferred to the caretaker self-heal rather than guessed.
	caretakerOwner := ""
	if users != nil {
		if caretaker, cerr := users.EarliestActiveAdmin(ctx); cerr == nil {
			caretakerOwner = caretaker.ID
		}
	}
	reconciled := 0
	for _, g := range all {
		if g.DatasetID != "" {
			continue
		}
		datasetID, derr := resolveDatasetForRef(ctx, targets, g.DataSourceRef, caretakerOwner)
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
// optionally narrowed by a case-insensitive name substring (?q). Only the
// acting user's own and shared-with datasets are returned: rows with no owner
// (unattributed pre-ownership data, visible to nobody) are never listed, even
// to the caretaker -- attribution is an explicit admin act, not an implicit
// read grant. Each row classifies the acting user's access ("owner"/"shared").
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	userID := s.actingUser(r)
	datasets, err := s.datasets.ListAccessible(r.Context(), r.URL.Query().Get("q"), userID)
	if err != nil {
		log.Printf("orchestrator: list datasets: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]datasetDTO, 0, len(datasets))
	for _, d := range datasets {
		out = append(out, toDatasetDTO(d, userID))
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

	// The acting signed-in account becomes the dataset's owner immediately --
	// the ownership stamping at the heart of this feature. A system/boot path
	// with no account stamps NULL, the ownerless-yet-unattributed state the
	// caretaker self-heal attributes later.
	id, err := s.datasets.Create(ctx, store.Dataset{
		Name:          name,
		Description:   description,
		DataSourceRef: ref,
		OwnerID:       s.actingUser(r),
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
		"owner_id":        s.actingUser(r),
	}); err != nil {
		log.Printf("orchestrator: audit dataset create: %v", err)
	}
	service.WriteJSON(w, http.StatusCreated, toDatasetDTO(d, s.actingUser(r)))
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
	service.WriteJSON(w, http.StatusOK, toDatasetDTO(updated, s.actingUser(r)))
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
	// Opening the detail view is the access event the inventory ranks by; stamp
	// it best-effort so an access-mark failure never turns a read into an error.
	if err := s.datasets.Touch(r.Context(), id); err != nil {
		log.Printf("orchestrator: touch dataset %q: %v", id, err)
	}
	out := datasetDetailDTO{datasetDTO: toDatasetDTO(d, s.actingUser(r))}
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
// when nothing can be served. It is the uniform single-object gate every
// dataset-keyed route passes through: a dataset the acting user cannot access
// (missing, unowned, or unshared) reads exactly as "not found", so a direct
// link to a stranger's dataset never reveals it exists.
func (s *Server) lookupDataset(w http.ResponseWriter, r *http.Request, id string) (store.Dataset, bool) {
	d, err := s.datasets.GetAccessible(r.Context(), id, s.actingUser(r))
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

// lookupOwnedDataset fetches a dataset only when the acting user is its owner,
// the gate every dataset-administering route (sharing management, and later
// metadata edit/delete) goes through. A dataset the acting user can read but not
// administer is refused with 403; an unknown id stays the uniform 404. A
// dataset with no owner (an unattributed legacy row) is owned by nobody, so it
// too answers 403 to everyone until the caretaker self-heal attributes it.
func (s *Server) lookupOwnedDataset(w http.ResponseWriter, r *http.Request, id string) (store.Dataset, bool) {
	actingUser := s.actingUser(r)
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
	if d.OwnerID == "" || d.OwnerID != actingUser {
		service.WriteErr(w, http.StatusForbidden, "only the dataset owner can administer sharing")
		return store.Dataset{}, false
	}
	return d, true
}

// shareGrantDTO is the wire shape of one collaborator's working access, as
// returned by GET /datasets/{id}/shares and PUT /datasets/{id}/shares/{username}.
// SharedBy is omitted from the wire: it names the account that granted the
// access, which is an audit concern, not something the collaborator list needs.
type shareGrantDTO struct {
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

func toShareGrantDTO(g store.ShareGrant) shareGrantDTO {
	return shareGrantDTO{UserID: g.UserID, Username: g.Username, CreatedAt: g.CreatedAt}
}

// handleListShares serves the collaborator list for the owner's sharing panel.
// Sharing management is owner-only (FR-011): a collaborator is refused with 403,
// so the grant list never leaks to the very accounts it names.
func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, ok := s.lookupOwnedDataset(w, r, id); !ok {
		return
	}
	grants, err := s.datasets.ListShares(ctx, id)
	if err != nil {
		log.Printf("orchestrator: list shares for dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]shareGrantDTO, 0, len(grants))
	for _, g := range grants {
		out = append(out, toShareGrantDTO(g))
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// handleShareDataset grants one account working access to a dataset owned by the
// acting user. The recipient is named by username (case-insensitive, matching
// the sign-in lookup); the owner cannot be shared with (400), a duplicate grant
// is refused (409), an unknown username is 404, and a non-owner acting user is
// refused with 403 before any of that is resolved. The grant takes effect
// immediately: it is one row write, and the next access-scoped query admits the
// recipient.
func (s *Server) handleShareDataset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	username := strings.TrimSpace(r.PathValue("username"))
	ds, ok := s.lookupOwnedDataset(w, r, id)
	if !ok {
		return
	}
	if username == "" {
		service.WriteErr(w, http.StatusBadRequest, "username is required")
		return
	}
	target, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			service.WriteErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("orchestrator: look up share recipient %q: %v", username, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target.ID == ds.OwnerID {
		service.WriteErr(w, http.StatusBadRequest, "you already own this dataset")
		return
	}
	existing, err := s.datasets.ListShares(ctx, id)
	if err != nil {
		log.Printf("orchestrator: list shares for dataset %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, g := range existing {
		if g.UserID == target.ID {
			service.WriteErr(w, http.StatusConflict, "dataset already shared with this user")
			return
		}
	}
	actor := s.actingUser(r)
	if err := s.datasets.Share(ctx, id, target.ID, actor); err != nil {
		log.Printf("orchestrator: share dataset %q with %q: %v", id, target.ID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.recordAudit(ctx, "dataset_share", "dataset", map[string]any{
		"dataset_id":  id,
		"user_id":     target.ID,
		"shared_with": target.Username,
	}); err != nil {
		log.Printf("orchestrator: audit dataset share: %v", err)
	}
	service.WriteJSON(w, http.StatusOK, toShareGrantDTO(store.ShareGrant{
		UserID:    target.ID,
		Username:  target.Username,
		CreatedAt: time.Now().UTC(),
	}))
}

// handleRevokeShare removes one collaborator's working access. Owner-only like
// every sharing-management route; revoking an already-absent grant is an
// idempotent 204 (the store's DELETE is a no-op), and an unknown username is
// resolved first so the response names the offender's status honestly.
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	username := strings.TrimSpace(r.PathValue("username"))
	if _, ok := s.lookupOwnedDataset(w, r, id); !ok {
		return
	}
	target, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			service.WriteErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("orchestrator: look up revoke recipient %q: %v", username, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.datasets.RevokeShare(ctx, id, target.ID); err != nil {
		log.Printf("orchestrator: revoke share for dataset %q user %q: %v", id, target.ID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.recordAudit(ctx, "dataset_unshare", "dataset", map[string]any{
		"dataset_id": id,
		"user_id":    target.ID,
	}); err != nil {
		log.Printf("orchestrator: audit dataset unshare: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
