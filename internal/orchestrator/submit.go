package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/service"
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

// reviewSettings are a goal's optional human-review overrides, carried from the
// registration form to whichever insert path the source kind selects. A nil
// threshold means the goal takes the service-wide default.
type reviewSettings struct {
	threshold *float64
	epochMode store.EpochMode
}

// parseReviewSettings reads the optional review overrides off the registration
// form. Both are validated here rather than at first use, so a typo is a
// rejected registration instead of a goal that silently runs under the defaults.
func parseReviewSettings(r *http.Request) (reviewSettings, error) {
	var settings reviewSettings
	if raw := strings.TrimSpace(r.FormValue("confidence_threshold")); raw != "" {
		threshold, err := strconv.ParseFloat(raw, 64)
		// NaN has to be rejected explicitly: ParseFloat accepts it, and every
		// comparison against it is false, so a range check alone lets it through --
		// after which no extraction ever clears the threshold and the settings
		// cannot be marshalled back to the analyst.
		if err != nil || math.IsNaN(threshold) || threshold <= 0 || threshold > 1 {
			return reviewSettings{}, errors.New("confidence_threshold must be a number greater than 0 and at most 1")
		}
		settings.threshold = &threshold
	}
	switch mode := store.EpochMode(strings.TrimSpace(r.FormValue("epoch_mode"))); mode {
	case "", store.EpochSpeculative, store.EpochBlocking:
		settings.epochMode = mode
	default:
		return reviewSettings{}, errors.New(`epoch_mode must be "speculative" or "blocking"`)
	}
	return settings, nil
}

// parseWindowBindings reads the optional entity-key/time-column window bindings off
// the registration form and validates them against the introspected schema: both or
// neither must be present, each must resolve to exactly one column
// (case-insensitive, mirroring the sandbox's own field resolution), and the time
// column must be orderable (temporal or numeric). It returns the resolved actual
// column names so they persist and compile against the exact schema spelling, or an
// analyst-fixable error the caller maps to 422.
func parseWindowBindings(r *http.Request, cols []columnDTO) (entityKey, timeColumn string, err error) {
	entity := strings.TrimSpace(r.FormValue("entity_key_column"))
	ts := strings.TrimSpace(r.FormValue("time_column"))
	if entity == "" && ts == "" {
		return "", "", nil
	}
	if entity == "" || ts == "" {
		return "", "", errors.New("entity_key_column and time_column must be provided together")
	}
	entityCol, ok := resolveSchemaColumn(cols, entity)
	if !ok {
		return "", "", fmt.Errorf("entity_key_column %q does not resolve to a unique column in the data source", entity)
	}
	timeCol, ok := resolveSchemaColumn(cols, ts)
	if !ok {
		return "", "", fmt.Errorf("time_column %q does not resolve to a unique column in the data source", ts)
	}
	if !isOrderableColumnType(timeCol.Type) {
		return "", "", fmt.Errorf("time_column %q must be a temporal or numeric column, not %s", ts, timeCol.Type)
	}
	return entityCol.Name, timeCol.Name, nil
}

// resolveSchemaColumn resolves a request field to its single introspected column
// under case-insensitive matching, reporting false when no column or more than one
// column matches (a case-colliding schema is ambiguous, not a silent pick) —
// mirroring the sandbox compiler's resolveColumn semantics.
func resolveSchemaColumn(cols []columnDTO, field string) (columnDTO, bool) {
	var match columnDTO
	found := 0
	for _, c := range cols {
		if strings.EqualFold(c.Name, field) {
			match = c
			found++
		}
	}
	if found != 1 {
		return columnDTO{}, false
	}
	return match, true
}

// isOrderableColumnType reports whether a DuckDB column type can back a window's
// ORDER BY: a numeric or temporal type. It mirrors the sandbox compiler's coarse
// type classification, which lives behind the CGO firewall and cannot be imported,
// so the orchestrator can reject a non-orderable time column at registration.
func isOrderableColumnType(t string) bool {
	u := strings.ToUpper(strings.TrimSpace(t))
	switch {
	case orderableNumericTypes[u],
		strings.HasPrefix(u, "DECIMAL"),
		strings.HasPrefix(u, "DATE"),
		strings.HasPrefix(u, "TIME"),
		strings.HasPrefix(u, "TIMESTAMP"):
		return true
	default:
		return false
	}
}

// orderableNumericTypes is the DuckDB numeric set (the fixed-point DECIMAL(p,s) is
// matched by prefix in isOrderableColumnType), mirroring the sandbox compiler's
// numericTypes across the CGO firewall.
var orderableNumericTypes = map[string]bool{
	"TINYINT":   true,
	"SMALLINT":  true,
	"INTEGER":   true,
	"BIGINT":    true,
	"HUGEINT":   true,
	"UTINYINT":  true,
	"USMALLINT": true,
	"UINTEGER":  true,
	"UBIGINT":   true,
	"UHUGEINT":  true,
	"FLOAT":     true,
	"DOUBLE":    true,
}

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
		service.WriteErr(w, http.StatusBadRequest, "invalid or oversized multipart form")
		return
	}
	goal := strings.TrimSpace(r.FormValue("goal"))
	if goal == "" {
		service.WriteErr(w, http.StatusBadRequest, "goal is required")
		return
	}
	review, err := parseReviewSettings(r)
	if err != nil {
		service.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ref, err := s.ingest(r)
	if err != nil {
		s.writeIngestErr(w, err)
		return
	}

	// Register the minted ref immediately -- this is the single choke point covering
	// upload, local-import, and document goals -- and before the intake introspect
	// and dry-run below, which the Sandbox validates against the registry. Skipping
	// it would 404 every intake, since those sandbox calls precede goals.Insert. A
	// row orphaned by a later intake failure is harmless: it names an object the
	// orchestrator itself staged.
	if err := s.goals.RegisterDataSourceRef(ctx, ref); err != nil {
		log.Printf("orchestrator: register data source ref: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
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
		s.submitDocumentGoal(ctx, w, goal, ref, introspect.Sample, review)
		return
	}
	schema := toSandboxSchema(introspect.Schema)

	// Window bindings are validated against the introspected schema before fitting:
	// both-or-neither, each resolving to a unique column, and an orderable time
	// column. A binding failure is analyst-fixable, so it is a 422 like an unfittable
	// objective. The resolved actual names persist so they compile against the exact
	// schema spelling.
	entityKey, timeColumn, err := parseWindowBindings(r, introspect.Schema.Columns)
	if err != nil {
		service.WriteErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Window kinds are reachable only when the goal bound its entity/time columns, so
	// availability rides that binding into the fitting prompt and the dry-run.
	windowed := entityKey != ""

	matrix, err := s.claude.GenerateEvaluationMatrix(ctx, goal, schema, windowed)
	if err != nil {
		log.Printf("orchestrator: generate evaluation matrix: %v", err)
		service.WriteErr(w, http.StatusBadGateway, "evaluation matrix generation failed")
		return
	}

	// Validate the fitted objective by executing it against the sandbox with no
	// filters (the exact request the root baseline will run). A sandbox fault
	// (pre- or post-repair) is surfaced as-is, never labeled an unfixable objective.
	verr := s.dryRunObjective(ctx, ref, matrix, entityKey, timeColumn)
	for attempts := 0; isObjectiveValidationFailure(verr) && attempts < maxObjectiveRepairs; attempts++ {
		repaired, rerr := s.claude.RepairEvaluationMatrix(ctx, goal, schema, matrix, verr.Error(), windowed)
		if rerr != nil {
			log.Printf("orchestrator: repair evaluation matrix: %v", rerr)
			service.WriteErr(w, http.StatusBadGateway, "evaluation matrix generation failed")
			return
		}
		matrix = repaired
		verr = s.dryRunObjective(ctx, ref, matrix, entityKey, timeColumn)
	}
	if isObjectiveValidationFailure(verr) {
		service.WriteErr(w, http.StatusUnprocessableEntity, "could not fit the goal to the data source: "+verr.Error())
		return
	}
	if verr != nil {
		s.writeIntakeErr(w, verr)
		return
	}

	intent := s.classifyIntent(ctx, goal, schema)

	optID := uuid.NewString()
	if err := s.goals.Insert(ctx, store.Goal{
		OptimizationFunctionID: optID,
		GoalText:               goal,
		EvaluationMatrix:       matrix,
		DataSourceRef:          ref,
		ConfidenceThreshold:    review.threshold,
		EpochMode:              review.epochMode,
		EntityKeyColumn:        entityKey,
		TimeColumn:             timeColumn,
		Track:                  intent.track,
		Claim:                  intent.claim,
		ClaimError:             intent.claimError,
	}); err != nil {
		log.Printf("orchestrator: insert goal: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.recordAudit(ctx, "goal_submit", "goal", map[string]any{
		"optimization_function_id": optID,
		"data_source_ref":          ref,
		"track":                    intent.track,
		"rationale":                intent.rationale,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.reportIntent(ctx, optID, intent)

	service.WriteJSON(w, http.StatusCreated, map[string]any{"optimization_function_id": optID})
}

// goalIntent is the classified routing decision as the goal row records it: the
// track, the canonical JSON of the validated claim (nil unless the verify track
// produced a claim that grounds in the schema), and the cannot-construct reason when
// it did not.
//
// rationale is the model's stated reasoning and failure is why the classifier could
// not answer at all; they are separate fields rather than one because they read
// differently to an analyst -- a rationale explains a routing decision, a failure
// explains its absence -- and a single field would put a parse error under a key
// labelled "rationale".
type goalIntent struct {
	track      string
	claim      []byte
	claimError string
	rationale  string
	failure    string
	auditEvent string
}

// classifyIntent asks Claude which track the goal belongs to and, on the verify
// track, validates the extracted claim against the introspected schema with the same
// deterministic column and value checks a Phase-1 proposal passes.
//
// It never fails registration. A classification fault falls open to the explore
// track: the classifier is an accelerator, and a goal that cannot be classified is
// still a goal worth running observationally. A claim that fails validation is the
// distinct cannot-construct outcome — the goal registers on the verify track with no
// claim and the validation reason recorded, which says the claim could not be built
// rather than that it was tested and found unsupported.
func (s *Server) classifyIntent(ctx context.Context, goal string, schema llm.SandboxSchema) goalIntent {
	result, err := s.claude.ClassifyGoalIntent(ctx, llm.GoalIntentInput{GoalText: goal, Schema: schema})
	if err != nil {
		log.Printf("orchestrator: classify goal intent: %v", err)
		return goalIntent{track: store.TrackExplore, auditEvent: "goal_intent_classification_failed", failure: err.Error()}
	}
	if result.Track != store.TrackVerify || result.Claim == nil {
		return goalIntent{track: store.TrackExplore, rationale: result.Rationale}
	}

	if reason := domain.ClaimGroundingError(result.Claim.Filters, columnNames(schema), columnValues(schema)); reason != "" {
		return goalIntent{
			track:      store.TrackVerify,
			claimError: reason,
			rationale:  result.Rationale,
			auditEvent: "goal_claim_construction_failed",
		}
	}
	encoded, err := json.Marshal(result.Claim)
	if err != nil {
		log.Printf("orchestrator: encode goal claim: %v", err)
		return goalIntent{track: store.TrackExplore, auditEvent: "goal_intent_classification_failed", failure: err.Error()}
	}
	return goalIntent{track: store.TrackVerify, claim: encoded, rationale: result.Rationale}
}

// reportIntent records the classification outcomes worth a trail: a fallen-open
// classification and a claim that could not be constructed. The SSE frame is
// published live-only -- a goal has no stream at registration, and creating hub
// state for a run that does not exist would leak it.
func (s *Server) reportIntent(ctx context.Context, optID string, intent goalIntent) {
	if intent.auditEvent == "" {
		return
	}
	// A cannot-construct goal reports the validation reason; a fallen-open
	// classification reports why the classifier could not answer.
	reason := intent.claimError
	if reason == "" {
		reason = intent.failure
	}
	detail := map[string]any{
		"optimization_function_id": optID,
		"track":                    intent.track,
		"reason":                   reason,
	}
	if err := s.recordAudit(ctx, intent.auditEvent, "goal", detail); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	if intent.claimError != "" {
		s.hub.PublishLive(optID, Event{Type: domain.ClaimConstructionFailed, Payload: map[string]any{"reason": intent.claimError}})
	}
}

// submitDocumentGoal completes registration for a document source: it derives the
// extractable fields via Claude (grounded in the goal and the sandbox's first-page
// sample), rejecting the goal with 422 if none fit, then persists the target
// fields in place of an Evaluation Matrix. The API response shape is stable with
// the tabular path (optimization_function_id), so the caller cannot tell the two
// intake flows apart. A Claude fault is a 502, matching the tabular generation
// failure mapping.
func (s *Server) submitDocumentGoal(ctx context.Context, w http.ResponseWriter, goal, ref, sample string, review reviewSettings) {
	fields, err := s.claude.IntrospectDocumentFields(ctx, goal, sample)
	if err != nil {
		log.Printf("orchestrator: introspect document fields: %v", err)
		service.WriteErr(w, http.StatusBadGateway, "document field introspection failed")
		return
	}
	if len(fields) == 0 {
		service.WriteErr(w, http.StatusUnprocessableEntity, "could not identify any extractable fields for the goal in this document")
		return
	}

	optID := uuid.NewString()
	if err := s.goals.Insert(ctx, store.Goal{
		OptimizationFunctionID: optID,
		GoalText:               goal,
		TargetFields:           fields,
		DataSourceRef:          ref,
		ConfidenceThreshold:    review.threshold,
		EpochMode:              review.epochMode,
	}); err != nil {
		log.Printf("orchestrator: insert goal: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := s.recordAudit(ctx, "goal_submit", "goal", map[string]any{
		"optimization_function_id": optID,
		"data_source_ref":          ref,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	service.WriteJSON(w, http.StatusCreated, map[string]any{"optimization_function_id": optID})
}

// goalListItemDTO is the web-UI projection of a goal plus its latest run status
// (or a synthetic "no run").
//
// Track and ClaimError are the goal's routing outcome, not its run's. They are on the
// read path because otherwise nothing can read them: a verify-track goal whose claim
// could not be built records the reason at registration, and a reason no client can
// fetch tells the analyst nothing. ClaimError is omitempty, so an ordinary goal's
// projection is unchanged.
type goalListItemDTO struct {
	OptimizationFunctionID string    `json:"optimization_function_id"`
	GoalText               string    `json:"goal_text"`
	CreatedAt              time.Time `json:"created_at"`
	Track                  string    `json:"track"`
	ClaimError             string    `json:"claim_error,omitempty"`
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
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	ids := make([]string, 0, len(goals))
	for _, g := range goals {
		ids = append(ids, g.OptimizationFunctionID)
	}
	latest, err := s.runs.LatestByGoal(ctx, ids)
	if err != nil {
		log.Printf("orchestrator: latest runs by goal: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := make([]goalListItemDTO, 0, len(goals))
	for _, g := range goals {
		item := goalListItemDTO{
			OptimizationFunctionID: g.OptimizationFunctionID,
			GoalText:               g.GoalText,
			CreatedAt:              g.CreatedAt,
			Track:                  g.Track,
			ClaimError:             g.ClaimError,
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
	service.WriteJSON(w, http.StatusOK, out)
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
		writeSandboxStatus(w, se, "intake")
		return
	}
	log.Printf("orchestrator: intake sandbox call: %v", err)
	service.WriteErr(w, http.StatusBadGateway, "sandbox unavailable")
}

// writeSandboxStatus maps a sandbox fault to a status for whichever phase hit
// it. The phase reaches the log rather than the response so an operator grepping
// a fault knows whether it came from registering a goal or from reviewing one,
// without the analyst's error text differing between the two.
func writeSandboxStatus(w http.ResponseWriter, se *SandboxError, phase string) {
	switch se.Status {
	case http.StatusBadRequest:
		service.WriteErr(w, http.StatusBadRequest, se.Message)
	case http.StatusNotFound:
		service.WriteErr(w, http.StatusNotFound, se.Message)
	default:
		log.Printf("orchestrator: sandbox fault during %s: %v", phase, se)
		service.WriteErr(w, http.StatusBadGateway, "sandbox unavailable")
	}
}

// writeIngestErr maps an ingestion error to a status: validation and escape
// errors are 400, a missing on-disk file is 404, anything else is a masked 500.
func (s *Server) writeIngestErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSource), errors.Is(err, errBothSources),
		errors.Is(err, errPathEscape), errors.Is(err, errNoImportDir):
		service.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, os.ErrNotExist):
		service.WriteErr(w, http.StatusNotFound, "data source not found")
	default:
		log.Printf("orchestrator: ingest data source: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
