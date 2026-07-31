package sandbox

import (
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

// Server is the stateless HTTP surface of the Sandbox Execution service. It holds
// only the object-store client and the staging limits; every request builds a
// fresh FileSource for the ref it carries, so the sandbox owns no per-goal state.
type Server struct {
	objects        *objectstore.Client
	maxObjectBytes int64
	maxTempDirSize string
}

// NewServer wires the handlers to the object store and staging limits.
func NewServer(objects *objectstore.Client, maxObjectBytes int64, maxTempDirSize string) *Server {
	return &Server{objects: objects, maxObjectBytes: maxObjectBytes, maxTempDirSize: maxTempDirSize}
}

// Routes returns the mux for the endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /introspect", s.handleIntrospect)
	mux.HandleFunc("POST /execute", s.handleExecute)
	mux.HandleFunc("POST /document/text", s.handleDocumentText)
	return mux
}

// columnDTO and schemaDTO carry explicit snake_case tags for the JSON contract;
// the shared datasource types are tag-less by design, so the sandbox maps to
// these local DTOs rather than tagging the shared package.
type columnDTO struct {
	Name string `json:"name"`
	Type string `json:"type"`
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

func (s *Server) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	var req IntrospectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}

	// A document source has no columns to describe or bind: report its kind plus a
	// first-page text sample and let the orchestrator derive extractable fields via
	// Claude. Kind detection is by extension, mirroring the tabular reader's own
	// extension dispatch, so the two readers own non-overlapping formats.
	if isDocumentRef(req.DataSourceRef) {
		src := NewDocumentSource(s.objects, req.DataSourceRef, s.maxObjectBytes)
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

	src := NewFileSource(s.objects, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize)
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

// handleDocumentText serves a document's ordered per-page plain text. It mirrors
// handleIntrospect's stage-and-read shape and is deterministic (no LLM): the
// orchestrator fetches this once per run as the substrate for provenance search.
func (s *Server) handleDocumentText(w http.ResponseWriter, r *http.Request) {
	var req DocumentTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}

	src := NewDocumentSource(s.objects, req.DataSourceRef, s.maxObjectBytes)
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
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}

	if req.Type != "" && req.Type != domain.InterventionQuery {
		service.WriteErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported intervention type %q", req.Type))
		return
	}

	src := NewFileSource(s.objects, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize)
	m, err := src.ExecuteCounted(r.Context(), req.Aggregation, req.Target, req.ValueExpression, req.Filters, req.IncludeRowCount)
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
		cols = append(cols, columnDTO{Name: c.Name, Type: c.Type})
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
