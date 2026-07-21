package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objectstore"
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

// Routes returns the mux for the two endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /introspect", s.handleIntrospect)
	mux.HandleFunc("POST /execute", s.handleExecute)
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

// IntrospectResponse returns the schema and the per-target column bindings.
type IntrospectResponse struct {
	Schema         schemaDTO       `json:"schema"`
	TargetBindings []TargetBinding `json:"target_bindings"`
}

// ExecuteRequest measures one aggregate over a data source under hard-constraint
// filters. Type discriminates the intervention shape; an empty Type defaults to
// query, and since this service handles only query, any other value is rejected.
type ExecuteRequest struct {
	DataSourceRef string                  `json:"data_source_ref"`
	Type          domain.InterventionType `json:"type"`
	Aggregation   string                  `json:"aggregation"`
	Target        domain.Target           `json:"target"`
	Filters       []domain.Constraint     `json:"filters"`
}

// ExecuteResponse carries the single measured aggregate shaped like an Outcome's
// Value: {"<target field>": <number>}, with a null number when a sum/avg/min/max
// filtered to an empty set (count over an empty set is 0).
type ExecuteResponse struct {
	Value map[string]any `json:"value"`
}

func (s *Server) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	var req IntrospectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		writeErr(w, http.StatusBadRequest, "data_source_ref is required")
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
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, IntrospectResponse{Schema: toSchemaDTO(schema), TargetBindings: bindings})
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DataSourceRef == "" {
		writeErr(w, http.StatusBadRequest, "data_source_ref is required")
		return
	}

	if req.Type != "" && req.Type != domain.InterventionQuery {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unsupported intervention type %q", req.Type))
		return
	}

	src := NewFileSource(s.objects, req.DataSourceRef, s.maxObjectBytes, s.maxTempDirSize)
	value, err := src.Execute(r.Context(), req.Aggregation, req.Target, req.Filters)
	if err != nil {
		writeStageErr(w, err)
		return
	}

	var measured any
	if value != nil {
		measured = *value
	}
	writeJSON(w, http.StatusOK, ExecuteResponse{Value: map[string]any{req.Target.Field: measured}})
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
		writeErr(w, http.StatusNotFound, "data source not found")
	case errors.Is(err, errUnknownField),
		errors.Is(err, errAmbiguousField),
		errors.Is(err, errNonNumeric),
		errors.Is(err, errUnknownAggregation),
		errors.Is(err, errUnknownOperator),
		errors.Is(err, errUnsupportedFormat),
		errors.Is(err, errObjectTooLarge):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		// The detail is masked from the client but logged so a 500-class defect
		// leaves a diagnostic trail.
		log.Printf("sandbox: internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("sandbox: encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
