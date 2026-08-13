package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// EpochMode is how a dataset's hypothesis loop treats pending human verifications.
// speculative (the default) never waits: the loop prunes on unverified
// confidence while verifications resolve in the background. blocking trades loop
// speed for pruning decisions taken only on verified values.
type EpochMode string

const (
	EpochSpeculative EpochMode = "speculative"
	EpochBlocking    EpochMode = "blocking"
)

// Dataset tracks, aliased from the domain: the classifier produces these values and
// this package persists them, so one declaration owns the spelling.
const (
	TrackExplore = domain.TrackExplore
	TrackVerify  = domain.TrackVerify
)

// Goal is an alias for the Dataset type for backward compatibility.
//
// Deprecated: Use Dataset instead.
type Goal = Dataset

// Dataset is a registered dataset as persisted in dataset_registry (formerly
// goal_registry). A dataset carries exactly one question form: a tabular dataset
// has an EvaluationMatrix (TargetFields empty); a document dataset has TargetFields
// (a zero-value matrix). ConfidenceThreshold is nil when it takes the
// service-wide HITL default.
// EntityKeyColumn and TimeColumn are the window bindings a windowed/entity-relative
// question compiles against; both empty for a plain aggregate question.
// They are always both set or both empty (validated at registration).
//
// Track is the routing track intake assigned. Claim is the raw jsonb of the
// validated domain.ClaimSpec on the verify track, and nil on the explore track or
// when the claim could not be constructed -- in which case ClaimError carries the
// validation reason. Claim stays a raw []byte rather than a decoded struct so a
// reader that only routes on Track never pays to decode it, and so the stored
// document is handed to the Verifier byte-identical to what intake validated.
type Dataset struct {
	OptimizationFunctionID string
	GoalText               string
	EvaluationMatrix       domain.EvaluationMatrix
	TargetFields           []domain.TargetField
	DataSourceRef          string
	ConfidenceThreshold    *float64
	EpochMode              EpochMode
	EntityKeyColumn        string
	TimeColumn             string
	Track                  string
	Claim                  []byte
	ClaimError             string
	CreatedAt              time.Time
}

// IsDocument reports whether this is a document dataset (its question is a set of
// fields to extract) rather than a tabular one (an EvaluationMatrix to query).
func (d Dataset) IsDocument() bool { return len(d.TargetFields) > 0 }

// DatasetRegistry is the dataset-registry access package, backed by a runtime pool
// (table: dataset_registry, formerly goal_registry).
type DatasetRegistry struct {
	pool *Pool
}

// GoalRegistry is a deprecated alias for DatasetRegistry.
//
// Deprecated: Use DatasetRegistry instead.
type GoalRegistry = DatasetRegistry

// NewGoalRegistry wires the registry to a pool.
//
// Deprecated: Use NewDatasetRegistry instead.
func NewGoalRegistry(pool *Pool) *GoalRegistry {
	return NewDatasetRegistry(pool)
}

// NewDatasetRegistry wires the registry to a pool.
func NewDatasetRegistry(pool *Pool) *DatasetRegistry {
	return &DatasetRegistry{pool: pool}
}

// datasetColumns is the read projection Get and List share, so their column order
// and scan order cannot drift apart. The nullable window-binding columns are
// COALESCEd to ” so they scan into plain string fields (empty ⇒ unbound), per the
// store's NULL-scan convention.
const datasetColumns = "optimization_function_id, goal_text, evaluation_matrix, datasource_ref, " +
	"target_fields, confidence_threshold, epoch_mode, " +
	"coalesce(entity_key_column, ''), coalesce(time_column, ''), " +
	"track, claim, coalesce(claim_error, ''), created_at"

// Insert persists a registered dataset. A document dataset writes a NULL
// evaluation_matrix and populated target_fields; a tabular dataset does the reverse.
// A nil []byte binds as SQL NULL, so exactly one jsonb column is populated.
func (g *DatasetRegistry) Insert(ctx context.Context, dataset Dataset) error {
	var matrix, targetFields []byte
	var err error
	if dataset.IsDocument() {
		targetFields, err = json.Marshal(dataset.TargetFields)
		if err != nil {
			return fmt.Errorf("marshal target fields: %w", err)
		}
	} else {
		matrix, err = json.Marshal(dataset.EvaluationMatrix)
		if err != nil {
			return fmt.Errorf("marshal evaluation matrix: %w", err)
		}
	}
	epochMode := dataset.EpochMode
	if epochMode == "" {
		epochMode = EpochSpeculative
	}
	// The column list is explicit, so the column's SQL DEFAULT never fires: an unset
	// track has to be defaulted here or a dataset registered by a path that runs no
	// classification (a document dataset) would persist an empty track.
	track := dataset.Track
	if track == "" {
		track = TrackExplore
	}
	// An unbound window binding writes SQL NULL (not an empty string), so a windowed
	// dataset is cheaply distinguishable by its non-NULL columns.
	entityKey := nullableText(dataset.EntityKeyColumn)
	timeColumn := nullableText(dataset.TimeColumn)
	_, err = g.pool.Exec(ctx,
		"INSERT INTO dataset_registry "+
			"(optimization_function_id, goal_text, evaluation_matrix, datasource_ref, target_fields, confidence_threshold, epoch_mode, entity_key_column, time_column, track, claim, claim_error) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)",
		dataset.OptimizationFunctionID, dataset.GoalText, matrix, dataset.DataSourceRef, targetFields,
		dataset.ConfidenceThreshold, epochMode, entityKey, timeColumn,
		track, dataset.Claim, nullableText(dataset.ClaimError),
	)
	if err != nil {
		return fmt.Errorf("insert dataset: %w", err)
	}
	return nil
}

// SetClaimError records why a verify-track goal's claim could not be constructed.
// It is the write behind the Verifier's re-validation failure report: the claim is
// re-checked against the introspected schema at dispatch time, and a failure there
// has no run to fail, so the goal row is where the analyst learns the reason.
// A goal id that matches no row is reported as an error rather than a silent
// success: the caller's only handling is to log, and an unrecorded reason that looks
// like a recorded one leaves the analyst with a claimless verify goal and no
// explanation anywhere.
func (g *DatasetRegistry) SetClaimError(ctx context.Context, optimizationFunctionID, reason string) error {
	tag, err := g.pool.Exec(ctx,
		"UPDATE dataset_registry SET claim_error = $1 WHERE optimization_function_id = $2",
		nullableText(reason), optimizationFunctionID,
	)
	if err != nil {
		return fmt.Errorf("set claim error for %q: %w", optimizationFunctionID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("set claim error for %q: no such dataset", optimizationFunctionID)
	}
	return nil
}

// RegisterDataSourceRef records a minted data source ref so the Sandbox can scope
// requests to it. It is idempotent (ON CONFLICT DO NOTHING) because the same object
// underpins one dataset registration and every later run against it.
func (g *DatasetRegistry) RegisterDataSourceRef(ctx context.Context, ref string) error {
	_, err := g.pool.Exec(ctx,
		"INSERT INTO data_source_registry (ref) VALUES ($1) ON CONFLICT DO NOTHING", ref)
	if err != nil {
		return fmt.Errorf("register data source ref: %w", err)
	}
	return nil
}

// DataSourceRefExists reports whether a ref is registered. It uses QueryRow, never a
// bare Query: pgx defers a permission error past Query, and an unread Rows would
// deadlock pool.Close, so a boolean existence check reads its single row eagerly.
func (g *DatasetRegistry) DataSourceRefExists(ctx context.Context, ref string) (bool, error) {
	var exists bool
	err := g.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM data_source_registry WHERE ref = $1)", ref).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check data source ref %q: %w", ref, err)
	}
	return exists, nil
}

// Get looks up a registered dataset by its optimization_function_id. Both jsonb
// columns are null-guarded: a document dataset has a NULL evaluation_matrix and a
// tabular dataset a NULL target_fields, and a NULL scans as a nil []byte that must
// not be unmarshalled (an empty input is not valid JSON) -- decoding both
// unconditionally would break every read once one document dataset exists.
func (g *DatasetRegistry) Get(ctx context.Context, optimizationFunctionID string) (Dataset, error) {
	var ds Dataset
	var matrix, targetFields []byte
	err := g.pool.QueryRow(ctx,
		"SELECT "+datasetColumns+" FROM dataset_registry WHERE optimization_function_id = $1",
		optimizationFunctionID,
	).Scan(&ds.OptimizationFunctionID, &ds.GoalText, &matrix, &ds.DataSourceRef, &targetFields,
		&ds.ConfidenceThreshold, &ds.EpochMode, &ds.EntityKeyColumn, &ds.TimeColumn,
		&ds.Track, &ds.Claim, &ds.ClaimError, &ds.CreatedAt)
	if err != nil {
		return Dataset{}, fmt.Errorf("get dataset %q: %w", optimizationFunctionID, err)
	}
	if err := decodeDatasetQuestion(&ds, matrix, targetFields); err != nil {
		return Dataset{}, err
	}
	return ds, nil
}

// List returns every registered dataset, newest first.
func (g *DatasetRegistry) List(ctx context.Context) ([]Dataset, error) {
	rows, err := g.pool.Query(ctx,
		"SELECT "+datasetColumns+" FROM dataset_registry ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	defer rows.Close()

	var datasets []Dataset
	for rows.Next() {
		var ds Dataset
		var matrix, targetFields []byte
		if err := rows.Scan(&ds.OptimizationFunctionID, &ds.GoalText, &matrix,
			&ds.DataSourceRef, &targetFields, &ds.ConfidenceThreshold, &ds.EpochMode,
			&ds.EntityKeyColumn, &ds.TimeColumn,
			&ds.Track, &ds.Claim, &ds.ClaimError, &ds.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan dataset row: %w", err)
		}
		if err := decodeDatasetQuestion(&ds, matrix, targetFields); err != nil {
			return nil, err
		}
		datasets = append(datasets, ds)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dataset rows: %w", err)
	}
	return datasets, nil
}

// decodeDatasetQuestion decodes a dataset's question from its two jsonb columns,
// skipping a NULL (nil []byte) column so a document dataset's NULL matrix and a
// tabular dataset's NULL target_fields each decode to a zero value rather than a
// json.Unmarshal error on empty input.
func decodeDatasetQuestion(ds *Dataset, matrix, targetFields []byte) error {
	if len(matrix) > 0 {
		if err := json.Unmarshal(matrix, &ds.EvaluationMatrix); err != nil {
			return fmt.Errorf("unmarshal evaluation matrix: %w", err)
		}
	}
	if len(targetFields) > 0 {
		if err := json.Unmarshal(targetFields, &ds.TargetFields); err != nil {
			return fmt.Errorf("unmarshal target fields: %w", err)
		}
	}
	return nil
}

// decodeGoalObjective is a deprecated alias for decodeDatasetQuestion.
//
// Deprecated: Use decodeDatasetQuestion instead.
func decodeGoalObjective(ds *Dataset, matrix, targetFields []byte) error {
	return decodeDatasetQuestion(ds, matrix, targetFields)
}

// Delete removes a dataset with the given optimizationFunctionID.
// It uses the table-level cascade (ON DELETE CASCADE on child tables) to clean
// up associated runs, verification queue entries, and causal verifications.
func (g *DatasetRegistry) Delete(ctx context.Context, optimizationFunctionID string) error {
	tag, err := g.pool.Exec(ctx,
		"DELETE FROM dataset_registry WHERE optimization_function_id = $1",
		optimizationFunctionID,
	)
	if err != nil {
		return fmt.Errorf("delete dataset %q: %w", optimizationFunctionID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete dataset %q: not found", optimizationFunctionID)
	}
	return nil
}
