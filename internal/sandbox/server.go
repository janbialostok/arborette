package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/service"
)

// refValidator reports whether a data source ref is one this system minted at
// ingest, scoping the sandbox to the registry rather than arbitrary bucket paths.
// It is a narrow local interface so internal/sandbox does not import internal/store;
// cmd/sandbox adapts the store method into it. A nil validator is a test seam only
// -- the wired binary always validates.
type refValidator interface {
	Validate(ctx context.Context, ref string) (bool, error)
}

// Server is the HTTP surface of the Sandbox Execution service. It is stateless in
// semantics -- every request builds a fresh reader for the ref it carries, so the
// sandbox owns no per-goal state -- while the optional cache adds a pure performance
// layer over immutable content-addressed refs. The limiter bounds per-class
// concurrency, and the ref validator scopes requests to registered data sources.
type Server struct {
	objects           *objectstore.Client
	validator         refValidator
	cache             *StageCache
	limiter           *classLimiter
	maxObjectBytes    int64
	maxBodyBytes      int64
	maxTempDirSize    string
	distinctValueCap  int
	analyzeMaxColumns int
	analyzeMaxBins    int
}

// NewServer wires the handlers to the object store, the ref validator, the staging
// cache, the concurrency limiter, and the staging limits. It takes primitives and
// narrow types only (infra-constructor convention). A nil validator and a nil cache
// are documented test seams; the wired binary passes real ones. analyzeMaxColumns
// and analyzeMaxBins bound the /analyze request shape (per-kind column count and a
// binned column's bin count), derived from config.
func NewServer(objects *objectstore.Client, validator refValidator, cache *StageCache, limiter *classLimiter, maxObjectBytes, maxBodyBytes int64, maxTempDirSize string, distinctValueCap, analyzeMaxColumns, analyzeMaxBins int) *Server {
	return &Server{
		objects:           objects,
		validator:         validator,
		cache:             cache,
		limiter:           limiter,
		maxObjectBytes:    maxObjectBytes,
		maxBodyBytes:      maxBodyBytes,
		maxTempDirSize:    maxTempDirSize,
		distinctValueCap:  distinctValueCap,
		analyzeMaxColumns: analyzeMaxColumns,
		analyzeMaxBins:    analyzeMaxBins,
	}
}

// Routes returns the mux for the endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /introspect", s.handleIntrospect)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /analyze", s.handleAnalyze)
	mux.HandleFunc("POST /document/text", s.handleDocumentText)
	return mux
}

// columnDTO and schemaDTO carry explicit snake_case tags for the JSON contract;
// the shared datasource types are tag-less by design, so the sandbox maps to
// these local DTOs rather than tagging the shared package.
type columnDTO struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	DistinctValues []string `json:"distinct_values,omitempty"`
}

type schemaDTO struct {
	Kind    string      `json:"kind"`
	Columns []columnDTO `json:"columns"`
}

// TargetBinding tells the caller which introspected column backs an optimization
// target: Matched is false with an empty Column when no column matched.
type TargetBinding struct {
	Target  string `json:"target"`
	Column  string `json:"column"`
	Matched bool   `json:"matched"`
}

// IntrospectRequest asks for the schema of a data source plus a binding of each
// optimization target to a column.
type IntrospectRequest struct {
	DataSourceRef string          `json:"data_source_ref"`
	Targets       []domain.Target `json:"targets"`
}

// IntrospectResponse returns the schema and the per-target column bindings. For a
// document source Schema.Kind is "document", TargetBindings is empty (a document
// has no columns to bind), and Sample carries a bounded plain-text excerpt of the
// document's first page that grounds the orchestrator's field-introspection call.
type IntrospectResponse struct {
	Schema         schemaDTO       `json:"schema"`
	TargetBindings []TargetBinding `json:"target_bindings"`
	Sample         string          `json:"sample,omitempty"`
}

// DocumentTextRequest asks for a document's ordered per-page plain text.
type DocumentTextRequest struct {
	DataSourceRef string `json:"data_source_ref"`
}

// DocumentTextResponse returns the document's per-page plain text, page index to
// text, the run-invariant substrate the orchestrator caches for provenance.
type DocumentTextResponse struct {
	Pages []string `json:"pages"`
}

// ExecuteRequest measures one aggregate over a data source under hard-constraint
// filters. Type discriminates the intervention shape; an empty Type defaults to
// query, and since this service handles only query, any other value is rejected.
// ValueExpression, when set, is the objective value expression measured in place
// of the bare Target; ObjectiveLabel is the key the measured value is returned
// under (a compiled expression has no single column name); IncludeRowCount adds
// the matched row count to the response, measured in the same scan. All three are
// omitempty so the legacy field-keyed request is unchanged.
type ExecuteRequest struct {
	DataSourceRef   string                  `json:"data_source_ref"`
	Type            domain.InterventionType `json:"type"`
	Aggregation     string                  `json:"aggregation"`
	Target          domain.Target           `json:"target"`
	ValueExpression *domain.Expression      `json:"value_expression,omitempty"`
	ObjectiveLabel  string                  `json:"objective_label,omitempty"`
	EntityKeyColumn string                  `json:"entity_key_column,omitempty"`
	TimeColumn      string                  `json:"time_column,omitempty"`
	IncludeRowCount bool                    `json:"include_row_count,omitempty"`
	Filters         []domain.Constraint     `json:"filters"`
}

// rowCountKey is the reserved Value key carrying the matched row count when
// IncludeRowCount is set. It cannot collide with a measured value's own key: that
// key is either a target field name or a RenderObjectiveLabel agg(expr) string.
const rowCountKey = "row_count"

// ExecuteResponse carries the single measured aggregate shaped like an Outcome's
// Value: {"<key>": <number>}, keyed by the request's objective label when set and
// falling back to the target field for the legacy path, with a null number when a
// sum/avg/min/max filtered to an empty set (count over an empty set is 0). When
// the request set IncludeRowCount the map additionally carries rowCountKey.
type ExecuteResponse struct {
	Value map[string]any `json:"value"`
}

// Analyze-kind discriminators. contingency returns GROUP BY counts; moments
// returns sum/sum-of-squares/cross-product aggregates for correlation estimation.
const (
	AnalyzeContingency = "contingency"
	AnalyzeMoments     = "moments"
)

// AnalyzeColumn is one contingency-table column. Bins 0 groups on the raw
// (categorical) value; a positive Bins quantile-buckets a numeric column into that
// many bins. The numeric endpoints of a mixed pair are binned this way by the
// caller; the moments kind never bins.
type AnalyzeColumn struct {
	Name string `json:"name"`
	Bins int    `json:"bins,omitempty"`
}

// AnalyzeRequest asks for one contingency or moments aggregation over a data
// source under optional hard-constraint filters. Columns (contingency) or Variables
// + GroupBy (moments) are populated by kind. The discriminator is additive-friendly:
// an unknown kind is rejected so later kinds slot in without breaking the contract.
type AnalyzeRequest struct {
	DataSourceRef string              `json:"data_source_ref"`
	Kind          string              `json:"kind"`
	Filters       []domain.Constraint `json:"filters,omitempty"`
	Columns       []AnalyzeColumn     `json:"columns,omitempty"`
	Variables     []string            `json:"variables,omitempty"`
	GroupBy       []string            `json:"group_by,omitempty"`
}

// ContingencyCell is one row of a contingency table: the group-key values (in the
// requested column order, numeric-binned rendered as bucket labels) and the matched
// row count.
type ContingencyCell struct {
	Values []string `json:"values"`
	Count  int64    `json:"count"`
}

// MomentsRow is one stratum's moment aggregates. Group is the categorical group-key
// values (empty for the ungrouped form); N is the complete-case row count; Sum and
// SumSq are per-variable in request order; Cross is the pairwise cross-product sums
// for pairs (i,j) with i<j in request order, so the caller can form the covariance
// (and thus correlation) matrix.
type MomentsRow struct {
	Group []string  `json:"group,omitempty"`
	N     int64     `json:"n"`
	Sum   []float64 `json:"sum"`
	SumSq []float64 `json:"sum_sq"`
	Cross []float64 `json:"cross"`
}

// AnalyzeResponse carries the aggregation result keyed by kind: Cells for
// contingency, Moments for moments.
type AnalyzeResponse struct {
	Kind    string            `json:"kind"`
	Cells   []ContingencyCell `json:"cells,omitempty"`
	Moments []MomentsRow      `json:"moments,omitempty"`
}

func (s *Server) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	release, ok := s.acquireSlot(r)
	if !ok {
		return
	}
	defer release()

	var req IntrospectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}
	if !s.validateRef(w, r, req.DataSourceRef) {
		return
	}

	// A document source has no columns to describe or bind: report its kind plus a
	// first-page text sample and let the orchestrator derive extractable fields via
	// Claude. Kind detection is by extension, mirroring the tabular reader's own
	// extension dispatch, so the two readers own non-overlapping formats.
	if isDocumentRef(req.DataSourceRef) {
		src := NewDocumentSource(s.objects, s.cache, req.DataSourceRef, s.maxObjectBytes)
		pages, err := src.Pages(r.Context())
		if err != nil {
			writeStageErr(w, err)
			return
		}
		service.WriteJSON(w, http.StatusOK, IntrospectResponse{
			Schema: schemaDTO{Kind: string(datasource.KindDocument)},
			Sample: documentSample(pages),
		})
		return
	}

	src := NewFileSource(s.objects, s.cache, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize, s.distinctValueCap)
	schema, err := src.Introspect(r.Context())
	if err != nil {
		writeStageErr(w, err)
		return
	}

	bindings, err := bindTargets(req.Targets, schema.Columns)
	if err != nil {
		service.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	service.WriteJSON(w, http.StatusOK, IntrospectResponse{Schema: toSchemaDTO(schema), TargetBindings: bindings})
}

// acquireSlot takes a concurrency slot for the default request class, blocking
// until one frees. A non-nil error means the request context was cancelled while
// waiting -- the client is gone, so no response is written.
func (s *Server) acquireSlot(r *http.Request) (func(), bool) {
	return s.acquireSlotClass(r, ClassDefault)
}

// acquireSlotClass takes a concurrency slot for the named class, so /analyze can
// hold its own class independent of the default surface's slots.
func (s *Server) acquireSlotClass(r *http.Request, class string) (func(), bool) {
	release, err := s.limiter.Acquire(r.Context(), class)
	if err != nil {
		return nil, false
	}
	return release, true
}

// validateRef scopes a request to a registered data source. Unknown ref -> the same
// 404 shape an object-store miss produces, so the boundary leaks no
// registered-vs-missing oracle. A nil validator (test seam) skips the check; the
// wired binary always validates.
func (s *Server) validateRef(w http.ResponseWriter, r *http.Request, ref string) bool {
	if s.validator == nil {
		return true
	}
	ok, err := s.validator.Validate(r.Context(), ref)
	if err != nil {
		log.Printf("sandbox: validate data source ref: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if !ok {
		service.WriteErr(w, http.StatusNotFound, "data source not found")
		return false
	}
	return true
}

// handleDocumentText serves a document's ordered per-page plain text. It mirrors
// handleIntrospect's stage-and-read shape and is deterministic (no LLM): the
// orchestrator fetches this once per run as the substrate for provenance search.
func (s *Server) handleDocumentText(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	release, ok := s.acquireSlot(r)
	if !ok {
		return
	}
	defer release()

	var req DocumentTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}
	if !s.validateRef(w, r, req.DataSourceRef) {
		return
	}

	src := NewDocumentSource(s.objects, s.cache, req.DataSourceRef, s.maxObjectBytes)
	pages, err := src.Pages(r.Context())
	if err != nil {
		writeStageErr(w, err)
		return
	}
	service.WriteJSON(w, http.StatusOK, DocumentTextResponse{Pages: pages})
}

// isDocumentRef reports whether a ref is a document this service reads (a PDF
// today), so introspection routes it to the document reader rather than the
// tabular one. The two extension sets are non-overlapping.
func isDocumentRef(ref string) bool {
	return strings.HasSuffix(strings.ToLower(ref), ".pdf")
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	release, ok := s.acquireSlot(r)
	if !ok {
		return
	}
	defer release()

	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}
	if !s.validateRef(w, r, req.DataSourceRef) {
		return
	}

	if req.Type != "" && req.Type != domain.InterventionQuery {
		service.WriteErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported intervention type %q", req.Type))
		return
	}

	// Reject a statically-invalid request (bad aggregation/operator/cast/literal)
	// before constructing the reader, so no object-store Get is paid for a request
	// the compiler would refuse anyway. Column/type checks need the schema and stay
	// in the compiler.
	if err := staticValidate(req.Aggregation, req.ValueExpression, req.Filters); err != nil {
		writeStageErr(w, err)
		return
	}
	// A windowed value expression needs entity/time bindings to compile; reject one
	// without them here, pre-staging, since the check is schema-independent.
	if req.ValueExpression != nil && domain.HasWindowKind(*req.ValueExpression) &&
		(req.EntityKeyColumn == "" || req.TimeColumn == "") {
		service.WriteErr(w, http.StatusBadRequest, "windowed objective requires entity_key_column and time_column")
		return
	}

	src := NewFileSource(s.objects, s.cache, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize, s.distinctValueCap)
	m, err := src.ExecuteCounted(r.Context(), req.Aggregation, req.Target, req.ValueExpression, req.Filters, req.IncludeRowCount, req.EntityKeyColumn, req.TimeColumn)
	if err != nil {
		writeStageErr(w, err)
		return
	}

	var measured any
	if m.Value != nil {
		measured = *m.Value
	}
	// A compiled expression has no single column name, so the value is keyed by the
	// objective label; the legacy field-only request carries no label and falls
	// back to the target field, keeping its response shape unchanged.
	key := req.ObjectiveLabel
	if key == "" {
		key = req.Target.Field
	}
	value := map[string]any{key: measured}
	if m.Counted {
		value[rowCountKey] = m.RowCount
	}
	service.WriteJSON(w, http.StatusOK, ExecuteResponse{Value: value})
}

// handleAnalyze answers a contingency or moments aggregation, following
// handleExecute's hardening order: MaxBytesReader -> acquire an analyze-class slot
// -> decode -> validateRef -> static request-shape validation (a 422, pre-staging)
// -> compile+run over the staged engine -> writeStageErr. It is the read-only
// surface the causal-discovery sweep drives.
func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	release, ok := s.acquireSlotClass(r, ClassAnalyze)
	if !ok {
		return
	}
	defer release()

	var req AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}
	if !s.validateRef(w, r, req.DataSourceRef) {
		return
	}

	// Request-shape validation is schema-independent, so it runs before staging and
	// answers 422 (unprocessable) rather than the 400/404 writeStageErr maps: an
	// unknown kind or over-cap column count is a caller contract error, not a
	// staging fault. Column existence and type checks need the schema and stay in
	// the compile step below.
	if err := staticValidateAnalyze(req.Kind, req.Columns, req.Variables, req.GroupBy, s.analyzeMaxColumns, s.analyzeMaxBins); err != nil {
		service.WriteErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	src := NewFileSource(s.objects, s.cache, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize, s.distinctValueCap)
	resp, err := src.Analyze(r.Context(), req)
	if err != nil {
		writeStageErr(w, err)
		return
	}
	service.WriteJSON(w, http.StatusOK, resp)
}

// bindTargets binds each target to a column using the shared case-insensitive
// match, so a target binds here exactly as it resolves at execute. No match
// yields Matched:false; a target matching more than one column is an ambiguous-
// schema error surfaced as a 400.
func bindTargets(targets []domain.Target, cols []datasource.Column) ([]TargetBinding, error) {
	bindings := make([]TargetBinding, 0, len(targets))
	for _, t := range targets {
		matches := matchColumns(cols, t.Field)
		switch len(matches) {
		case 0:
			bindings = append(bindings, TargetBinding{Target: t.Field, Matched: false})
		case 1:
			bindings = append(bindings, TargetBinding{Target: t.Field, Column: matches[0].Name, Matched: true})
		default:
			return nil, fmt.Errorf("%w: %q", errAmbiguousField, t.Field)
		}
	}
	return bindings, nil
}

func toSchemaDTO(schema *datasource.Schema) schemaDTO {
	cols := make([]columnDTO, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		cols = append(cols, columnDTO{Name: c.Name, Type: c.Type, DistinctValues: c.DistinctValues})
	}
	return schemaDTO{Kind: string(schema.Kind), Columns: cols}
}

// writeStageErr maps a staging/introspection/execution error to a status: a
// missing object-store key is 404, the compiler/format/size validation errors are
// 400, anything else is a masked 500.
func writeStageErr(w http.ResponseWriter, err error) {
	switch {
	case objectstore.IsNotFound(err):
		service.WriteErr(w, http.StatusNotFound, "data source not found")
	case errors.Is(err, errUnknownField),
		errors.Is(err, errAmbiguousField),
		errors.Is(err, errNonNumeric),
		errors.Is(err, errUnknownAggregation),
		errors.Is(err, errUnknownOperator),
		errors.Is(err, errTypeIncompatible),
		errors.Is(err, errUnknownCast),
		errors.Is(err, errNonFiniteValue),
		errors.Is(err, errUnsupportedFormat),
		errors.Is(err, errUnsupportedDocument),
		errors.Is(err, errObjectTooLarge):
		service.WriteErr(w, http.StatusBadRequest, err.Error())
	default:
		// The detail is masked from the client but logged so a 500-class defect
		// leaves a diagnostic trail.
		log.Printf("sandbox: internal error: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
