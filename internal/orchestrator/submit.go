package orchestrator

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/store"
)

// maxUploadMemory bounds how much of a multipart upload is buffered in memory
// before spilling to a temp file during form parsing. maxUploadBytes caps the
// whole request body so an oversized upload is rejected before it spills to disk
// or is stored — mirroring the Sandbox's own object-size ceiling so the
// orchestrator never persists an object the sandbox would later reject.
const (
	maxUploadMemory = 32 << 20
	maxUploadBytes  = 512 << 20
)

// maxObjectiveRepairs bounds how many schema-aware repair attempts registration
// makes against the sandbox dry-run before declaring the goal unfittable (422).
const maxObjectiveRepairs = 3

// Ingestion-path sentinels, mapped to statuses by writeIngestErr.
var (
	errNoSource    = errors.New("a data source (file upload or import_path) is required")
	errBothSources = errors.New("provide either a file upload or import_path, not both")
	errPathEscape  = errors.New("import_path escapes the import directory")
	errNoImportDir = errors.New("on-disk import is not configured")
)

// handleSubmitGoal registers an analyst goal: it ingests the data source into the
// object store, introspects the source as a precondition, fits the Evaluation
// Matrix to that schema, validates the fitted objective by a dry-run against the
// sandbox (with one schema-aware repair on a compile failure), then persists the
// goal and audits the submission. It does not start Phase 1 — the hypothesis loop
// is a separate explicit trigger.
func (s *Server) handleSubmitGoal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid or oversized multipart form")
		return
	}
	goal := strings.TrimSpace(r.FormValue("goal"))
	if goal == "" {
		writeErr(w, http.StatusBadRequest, "goal is required")
		return
	}

	ref, err := s.ingest(r)
	if err != nil {
		s.writeIngestErr(w, err)
		return
	}

	// Introspect first, as a hard precondition: the schema fits the objective to
	// real columns, and an unreadable/unsupported/missing source is surfaced now
	// rather than at run time.
	introspect, err := s.sandbox.Introspect(ctx, IntrospectRequest{DataSourceRef: ref})
	if err != nil {
		s.writeIntakeErr(w, err)
		return
	}

	// A document source has no aggregation/direction to fit: its objective is a set
	// of fields to extract accurately. Branch here so the tabular objective-fitting
	// path below is never entered for a document goal.
	if introspect.Schema.Kind == string(datasource.KindDocument) {
		s.submitDocumentGoal(ctx, w, goal, ref, introspect.Sample)
		return
	}
	schema := toSandboxSchema(introspect.Schema)

	matrix, err := s.claude.GenerateEvaluationMatrix(ctx, goal, schema)
	if err != nil {
		log.Printf("orchestrator: generate evaluation matrix: %v", err)
		writeErr(w, http.StatusBadGateway, "evaluation matrix generation failed")
		return
	}

	// Validate the fitted objective by executing it against the sandbox with no
	// filters (the exact request the root baseline will run). A sandbox fault
	// (pre- or post-repair) is surfaced as-is, never labeled an unfixable objective.
	verr := s.dryRunObjective(ctx, ref, matrix)
	for attempts := 0; isObjectiveValidationFailure(verr) && attempts < maxObjectiveRepairs; attempts++ {
		repaired, rerr := s.claude.RepairEvaluationMatrix(ctx, goal, schema, matrix, verr.Error())
		if rerr != nil {
			log.Printf("orchestrator: repair evaluation matrix: %v", rerr)
			writeErr(w, http.StatusBadGateway, "evaluation matrix generation failed")
			return
		}
		matrix = repaired
		verr = s.dryRunObjective(ctx, ref, matrix)
	}
	if isObjectiveValidationFailure(verr) {
		writeErr(w, http.StatusUnprocessableEntity, "could not fit the goal to the data source: "+verr.Error())
		return
	}
	if verr != nil {
		s.writeIntakeErr(w, verr)
		return
	}

	optID := uuid.NewString()
	if err := s.goals.Insert(ctx, store.Goal{
		OptimizationFunctionID: optID,
		GoalText:               goal,
		EvaluationMatrix:       matrix,
		DataSourceRef:          ref,
	}); err != nil {
		log.Printf("orchestrator: insert goal: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.recordAudit(ctx, "goal_submit", "goal", map[string]any{
		"optimization_function_id": optID,
		"data_source_ref":          ref,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"optimization_function_id": optID})
}

// submitDocumentGoal completes registration for a document source: it derives the
// extractable fields via Claude (grounded in the goal and the sandbox's first-page
// sample), rejecting the goal with 422 if none fit, then persists the target
// fields in place of an Evaluation Matrix. The API response shape is stable with
// the tabular path (optimization_function_id), so the caller cannot tell the two
// intake flows apart. A Claude fault is a 502, matching the tabular generation
// failure mapping.
func (s *Server) submitDocumentGoal(ctx context.Context, w http.ResponseWriter, goal, ref, sample string) {
	fields, err := s.claude.IntrospectDocumentFields(ctx, goal, sample)
	if err != nil {
		log.Printf("orchestrator: introspect document fields: %v", err)
		writeErr(w, http.StatusBadGateway, "document field introspection failed")
		return
	}
	if len(fields) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "could not identify any extractable fields for the goal in this document")
		return
	}

	optID := uuid.NewString()
	if err := s.goals.Insert(ctx, store.Goal{
		OptimizationFunctionID: optID,
		GoalText:               goal,
		TargetFields:           fields,
		DataSourceRef:          ref,
	}); err != nil {
		log.Printf("orchestrator: insert goal: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.recordAudit(ctx, "goal_submit", "goal", map[string]any{
		"optimization_function_id": optID,
		"data_source_ref":          ref,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"optimization_function_id": optID})
}

// goalListItemDTO is the web-UI projection of a goal plus its latest run status
// (or a synthetic "no run").
type goalListItemDTO struct {
	OptimizationFunctionID string    `json:"optimization_function_id"`
	GoalText               string    `json:"goal_text"`
	CreatedAt              time.Time `json:"created_at"`
	Status                 string    `json:"status"`
	FailureReason          string    `json:"failure_reason,omitempty"`
}

// handleListGoals serves each goal's latest run status durably, after the fact —
// the counterpart to the ephemeral live stream. A goal never triggered gets a
// synthetic "no run".
func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goals, err := s.goals.List(ctx)
	if err != nil {
		log.Printf("orchestrator: list goals: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	ids := make([]string, 0, len(goals))
	for _, g := range goals {
		ids = append(ids, g.OptimizationFunctionID)
	}
	latest, err := s.runs.LatestByGoal(ctx, ids)
	if err != nil {
		log.Printf("orchestrator: latest runs by goal: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := make([]goalListItemDTO, 0, len(goals))
	for _, g := range goals {
		item := goalListItemDTO{
			OptimizationFunctionID: g.OptimizationFunctionID,
			GoalText:               g.GoalText,
			CreatedAt:              g.CreatedAt,
			Status:                 "no run",
		}
		if run, ok := latest[g.OptimizationFunctionID]; ok {
			item.Status = string(run.Status)
			if run.FailureReason != nil {
				item.FailureReason = *run.FailureReason
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// ingest resolves the request's data source to an object-store ref via one of
// two paths: an uploaded file part, or an on-disk path under the read-only
// import mount. Exactly one must be present.
func (s *Server) ingest(r *http.Request) (string, error) {
	file, header, ferr := r.FormFile("file")
	importPath := strings.TrimSpace(r.FormValue("import_path"))

	switch {
	case ferr == nil && importPath != "":
		file.Close()
		return "", errBothSources
	case ferr == nil:
		defer file.Close()
		contentType := header.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		key := s.objects.NewKey("datasources", uuid.NewString(), filepath.Base(header.Filename))
		if err := s.objects.Put(r.Context(), key, file, contentType); err != nil {
			return "", err
		}
		return key, nil
	case importPath != "":
		return s.ingestLocal(r.Context(), importPath)
	default:
		return "", errNoSource
	}
}

// ingestLocal copies a file from the read-only import mount into the object
// store. It rejects any path that escapes the mount: the request path is forced
// relative, joined under the mount, then symlink-resolved and verified to remain
// contained.
func (s *Server) ingestLocal(ctx context.Context, importPath string) (string, error) {
	if s.localImportDir == "" {
		return "", errNoImportDir
	}
	// Clean("/"+path) neutralizes "..", leading slashes, and absolute paths; Join
	// re-roots the result under the mount.
	rel := filepath.Clean("/" + importPath)
	full := filepath.Join(s.localImportDir, rel)

	rootReal, err := filepath.EvalSymlinks(s.localImportDir)
	if err != nil {
		return "", err
	}
	fullReal, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	if fullReal != rootReal && !strings.HasPrefix(fullReal, rootReal+string(os.PathSeparator)) {
		return "", errPathEscape
	}

	f, err := os.Open(fullReal)
	if err != nil {
		return "", err
	}
	defer f.Close()

	key := s.objects.NewKey("datasources", uuid.NewString(), filepath.Base(fullReal))
	if err := s.objects.Put(ctx, key, f, contentTypeForPath(fullReal)); err != nil {
		return "", err
	}
	return key, nil
}

func contentTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}

// writeIntakeErr maps a sandbox intake error (introspection precondition or a
// non-repairable dry-run outcome) to a status. A sandbox 400 is analyst-fixable
// (unreadable/unsupported source; message surfaced), a 404 is a missing source,
// and any 5xx or transport error is a sandbox fault surfaced as 502 — a masked
// sandbox 500 must never read as an analyst-fixable 4xx.
func (s *Server) writeIntakeErr(w http.ResponseWriter, err error) {
	var se *SandboxError
	if errors.As(err, &se) {
		switch {
		case se.Status == http.StatusBadRequest:
			writeErr(w, http.StatusBadRequest, se.Message)
		case se.Status == http.StatusNotFound:
			writeErr(w, http.StatusNotFound, se.Message)
		default:
			log.Printf("orchestrator: sandbox fault during intake: %v", err)
			writeErr(w, http.StatusBadGateway, "sandbox unavailable")
		}
		return
	}
	log.Printf("orchestrator: intake sandbox call: %v", err)
	writeErr(w, http.StatusBadGateway, "sandbox unavailable")
}

// writeIngestErr maps an ingestion error to a status: validation and escape
// errors are 400, a missing on-disk file is 404, anything else is a masked 500.
func (s *Server) writeIngestErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSource), errors.Is(err, errBothSources),
		errors.Is(err, errPathEscape), errors.Is(err, errNoImportDir):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, "data source not found")
	default:
		log.Printf("orchestrator: ingest data source: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
