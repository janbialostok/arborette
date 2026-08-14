package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DatasetStatus is a dataset's user-set lifecycle state. Objective-driven usage
// ("empty" vs "in use") is derived at read time, never stored.
type DatasetStatus string

const (
	DatasetActive   DatasetStatus = "active"
	DatasetArchived DatasetStatus = "archived"
)

// ErrNameConflict is returned when a dataset create/update would break the
// case-insensitive unique-name rule. The web UI surfaces it as the 409 conflict.
var ErrNameConflict = errors.New("dataset name already in use")

// Dataset is a first-class, user-managed data source container as persisted in
// the datasets table. ObjectiveCount is derived (a LEFT JOIN over
// goal_registry.dataset_id), never stored.
type Dataset struct {
	ID            string
	Name          string
	Description   string
	Status        DatasetStatus
	DataSourceRef string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// LastAccessedAt is when the dataset's detail view was last opened (the
	// access event that ranks the inventory). Nil means never accessed; the
	// column is NULL until the first open.
	LastAccessedAt *time.Time
	ObjectiveCount int
	// OwnerID is the owning account. Empty for a row created before ownership
	// existed (or by a background process) that has not yet been attributed to
	// the admin caretaker; such a dataset is visible to nobody until attributed.
	OwnerID string
}

// ShareGrant is one collaborator's working access to a dataset, as surfaced by
// ListShares. SharedBy is the account that granted it, empty for a grant made by
// the caretaker attribution backfill.
type ShareGrant struct {
	UserID    string
	Username  string
	CreatedAt time.Time
	SharedBy  string
}

// accessPredicate is the shared working-access test applied by every dataset and
// goal query that filters by user: the dataset's owner or an explicit share
// grant. The parameter index is $1 for the acting user's id.
const accessPredicate = `(d.owner_id = $1::uuid OR EXISTS (
	SELECT 1 FROM dataset_shares sh WHERE sh.dataset_id = d.id AND sh.user_id = $1::uuid))`

// DatasetStore is the datasets-table access package, backed by a runtime pool.
type DatasetStore struct {
	pool *Pool
}

// NewDatasetStore wires the dataset store to a pool.
func NewDatasetStore(pool *Pool) *DatasetStore {
	return &DatasetStore{pool: pool}
}

// datasetColumns is the read projection every dataset query shares, so the
// column order and scan order cannot drift apart.
const datasetColumns = `d.id, d.name, d.description, d.status, d.datasource_ref, ` +
	`d.created_at, d.updated_at, d.last_accessed_at, count(g.optimization_function_id)::int AS objective_count, ` +
	`coalesce(d.owner_id::text, '')`

// Create registers a new dataset and returns its minted id. The name uniqueness
// rule is case-insensitive (enforced by the lower(name) function index), so a
// conflicting name is reported as ErrNameConflict rather than a raw constraint
// error. The data source must already be ingested and its ref registered; the
// caller is responsible for that precondition -- this method only writes the
// metadata row.
func (s *DatasetStore) Create(ctx context.Context, dataset Dataset) (string, error) {
	if dataset.Status == "" {
		dataset.Status = DatasetActive
	}
	var id string
	err := s.pool.QueryRow(ctx,
		"INSERT INTO datasets (name, description, status, datasource_ref, owner_id) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		dataset.Name, dataset.Description, dataset.Status, dataset.DataSourceRef, nullableUUID(dataset.OwnerID),
	).Scan(&id)
	if err != nil {
		if isNameConflict(err) {
			return "", ErrNameConflict
		}
		return "", fmt.Errorf("insert dataset: %w", err)
	}
	return id, nil
}

// Get looks up a dataset by id, with its derived objective count.
func (s *DatasetStore) Get(ctx context.Context, id string) (Dataset, error) {
	var d Dataset
	err := s.pool.QueryRow(ctx,
		"SELECT "+datasetColumns+" FROM datasets d "+
			"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
			"WHERE d.id = $1 GROUP BY d.id",
		id,
	).Scan(&d.ID, &d.Name, &d.Description, &d.Status, &d.DataSourceRef,
		&d.CreatedAt, &d.UpdatedAt, &d.LastAccessedAt, &d.ObjectiveCount, &d.OwnerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dataset{}, pgx.ErrNoRows
		}
		return Dataset{}, fmt.Errorf("get dataset %q: %w", id, err)
	}
	return d, nil
}

// GetByRef looks up the dataset bound to a data source ref. It is the read the
// legacy submission path uses to reuse an existing dataset (created by the 0015
// backfill or an earlier implicit registration) instead of minting a duplicate
// around the same ref.
func (s *DatasetStore) GetByRef(ctx context.Context, ref string) (Dataset, error) {
	var d Dataset
	err := s.pool.QueryRow(ctx,
		"SELECT "+datasetColumns+" FROM datasets d "+
			"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
			"WHERE d.datasource_ref = $1 GROUP BY d.id LIMIT 1",
		ref,
	).Scan(&d.ID, &d.Name, &d.Description, &d.Status, &d.DataSourceRef,
		&d.CreatedAt, &d.UpdatedAt, &d.LastAccessedAt, &d.ObjectiveCount, &d.OwnerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dataset{}, pgx.ErrNoRows
		}
		return Dataset{}, fmt.Errorf("get dataset by ref %q: %w", ref, err)
	}
	return d, nil
}

// List returns every dataset with its derived objective count, ordered by last
// access (most recently opened first), with never-accessed datasets falling back
// to newest-created-first and a final id tie-break so the order is deterministic
// across reloads. A non-empty q filters by case-insensitive name substring.
func (s *DatasetStore) List(ctx context.Context, query string) ([]Dataset, error) {
	var rows pgx.Rows
	var err error
	const orderBy = " ORDER BY d.last_accessed_at DESC NULLS LAST, d.created_at DESC, d.id"
	if strings.TrimSpace(query) == "" {
		rows, err = s.pool.Query(ctx,
			"SELECT "+datasetColumns+" FROM datasets d "+
				"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
				"GROUP BY d.id"+orderBy,
		)
	} else {
		rows, err = s.pool.Query(ctx,
			"SELECT "+datasetColumns+" FROM datasets d "+
				"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
				"WHERE position(lower($1) in lower(d.name)) > 0 "+
				"GROUP BY d.id"+orderBy,
			query,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	defer rows.Close()

	var datasets []Dataset
	for rows.Next() {
		var d Dataset
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.Status, &d.DataSourceRef,
			&d.CreatedAt, &d.UpdatedAt, &d.LastAccessedAt, &d.ObjectiveCount, &d.OwnerID); err != nil {
			return nil, fmt.Errorf("scan dataset row: %w", err)
		}
		datasets = append(datasets, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dataset rows: %w", err)
	}
	return datasets, nil
}

// ListAccessible returns the datasets the acting user may work with — those
// they own or hold a share grant for — under the same ordering and name filter
// as List. Datasets with no owner (unattributed pre-ownership rows) surface for
// nobody here, even the caretaker: attribution is an explicit admin act, not an
// implicit read grant.
func (s *DatasetStore) ListAccessible(ctx context.Context, query, userID string) ([]Dataset, error) {
	var rows pgx.Rows
	var err error
	const orderBy = " ORDER BY d.last_accessed_at DESC NULLS LAST, d.created_at DESC, d.id"
	if strings.TrimSpace(query) == "" {
		rows, err = s.pool.Query(ctx,
			"SELECT "+datasetColumns+" FROM datasets d "+
				"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
				"WHERE "+accessPredicate+" GROUP BY d.id"+orderBy,
			userID,
		)
	} else {
		rows, err = s.pool.Query(ctx,
			"SELECT "+datasetColumns+" FROM datasets d "+
				"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
				"WHERE "+accessPredicate+" AND position(lower($2) in lower(d.name)) > 0 "+
				"GROUP BY d.id"+orderBy,
			userID, query,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list accessible datasets: %w", err)
	}
	defer rows.Close()

	var datasets []Dataset
	for rows.Next() {
		var d Dataset
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.Status, &d.DataSourceRef,
			&d.CreatedAt, &d.UpdatedAt, &d.LastAccessedAt, &d.ObjectiveCount, &d.OwnerID); err != nil {
			return nil, fmt.Errorf("scan accessible dataset row: %w", err)
		}
		datasets = append(datasets, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accessible dataset rows: %w", err)
	}
	return datasets, nil
}

// GetAccessible fetches a dataset only if the acting user holds working access
// to it. The caller relies on pgx.ErrNoRows to answer 404 for both "missing"
// and "exists but not yours" uniformly.
func (s *DatasetStore) GetAccessible(ctx context.Context, id, userID string) (Dataset, error) {
	var d Dataset
	err := s.pool.QueryRow(ctx,
		"SELECT "+datasetColumns+" FROM datasets d "+
			"LEFT JOIN goal_registry g ON g.dataset_id = d.id "+
			"WHERE d.id = $2 AND "+accessPredicate+" GROUP BY d.id",
		userID, id,
	).Scan(&d.ID, &d.Name, &d.Description, &d.Status, &d.DataSourceRef,
		&d.CreatedAt, &d.UpdatedAt, &d.LastAccessedAt, &d.ObjectiveCount, &d.OwnerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Dataset{}, pgx.ErrNoRows
		}
		return Dataset{}, fmt.Errorf("get accessible dataset %q: %w", id, err)
	}
	return d, nil
}

// CanAccess reports whether the acting user holds working access to a dataset.
// The no-row case (unknown id) reads as no access, which is the right answer for
// both "missing" and "not yours" callers.
func (s *DatasetStore) CanAccess(ctx context.Context, id, userID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM datasets d WHERE d.id = $2 AND "+accessPredicate+")",
		userID, id,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check dataset access %q: %w", id, err)
	}
	return ok, nil
}

// Touch records that the dataset's detail view was opened, stamping the
// last-access time the inventory orders by. It is the access event the web
// detail GET fires; the caller treats a failure as best effort because an access
// mark must never turn a read into an error.
func (s *DatasetStore) Touch(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE datasets SET last_accessed_at = now() WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("touch dataset %q: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// Update edits a dataset's metadata only (name, description, status) and touches
// updated_at. The data source binding is immutable and never changed. A rename
// onto an existing name reports ErrNameConflict; an unknown id reports
// pgx.ErrNoRows.
func (s *DatasetStore) Update(ctx context.Context, id, name, description string, status DatasetStatus) error {
	if status == "" {
		status = DatasetActive
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE datasets SET name = $2, description = $3, status = $4, updated_at = now() WHERE id = $1",
		id, name, description, status,
	)
	if err != nil {
		if isNameConflict(err) {
			return ErrNameConflict
		}
		return fmt.Errorf("update dataset %q: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountObjectives returns how many goals currently reference the dataset.
func (s *DatasetStore) CountObjectives(ctx context.Context, id string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*)::int FROM goal_registry WHERE dataset_id = $1", id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count objectives for dataset %q: %w", id, err)
	}
	return n, nil
}

// ListObjectives returns the goals bound to a dataset, for the non-empty delete
// response. It reuses the goal-registry projection and decode path, so the 409
// payload names the offending objectives rather than only their ids.
func (s *DatasetStore) ListObjectives(ctx context.Context, datasetID string) ([]Goal, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+goalColumns+" FROM goal_registry WHERE dataset_id = $1 ORDER BY created_at ASC",
		datasetID,
	)
	if err != nil {
		return nil, fmt.Errorf("list objectives for dataset %q: %w", datasetID, err)
	}
	defer rows.Close()

	var goals []Goal
	for rows.Next() {
		var goal Goal
		var matrix, targetFields []byte
		if err := rows.Scan(&goal.OptimizationFunctionID, &goal.GoalText, &matrix,
			&goal.DataSourceRef, &goal.DatasetID, &targetFields,
			&goal.ConfidenceThreshold, &goal.EpochMode,
			&goal.EntityKeyColumn, &goal.TimeColumn,
			&goal.Track, &goal.Claim, &goal.ClaimError, &goal.CreatedAt, &goal.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan objective row for dataset %q: %w", datasetID, err)
		}
		if err := decodeGoalObjective(&goal, matrix, targetFields); err != nil {
			return nil, err
		}
		goals = append(goals, goal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate objective rows for dataset %q: %w", datasetID, err)
	}
	return goals, nil
}

// Delete removes the dataset row. The caller decides emptiness (CountObjectives
// in the same transaction or a pre-check) and whether to retire the ref.
func (s *DatasetStore) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM datasets WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete dataset %q: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// Share grants a user working access to a dataset (or is a no-op when the grant
// already exists). The caller has already verified the acting user may grant:
// only the owner grants access, the caretaker attribution path is backfill.
func (s *DatasetStore) Share(ctx context.Context, datasetID, userID, sharedBy string) error {
	_, err := s.pool.Exec(ctx,
		"INSERT INTO dataset_shares (dataset_id, user_id, created_by) VALUES ($1, $2, $3) "+
			"ON CONFLICT (dataset_id, user_id) DO NOTHING",
		datasetID, userID, nullableUUID(sharedBy),
	)
	if err != nil {
		return fmt.Errorf("share dataset %q with user %q: %w", datasetID, userID, err)
	}
	return nil
}

// RevokeShare removes a user's working access to a dataset. Revoking a grant
// that does not exist is a no-op, so the caller treats a completed revoke as
// success either way.
func (s *DatasetStore) RevokeShare(ctx context.Context, datasetID, userID string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM dataset_shares WHERE dataset_id = $1 AND user_id = $2",
		datasetID, userID,
	)
	if err != nil {
		return fmt.Errorf("revoke dataset %q share for user %q: %w", datasetID, userID, err)
	}
	return nil
}

// ListShares returns the collaborators currently granted access to a dataset,
// oldest grant first for a stable UI order.
func (s *DatasetStore) ListShares(ctx context.Context, datasetID string) ([]ShareGrant, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT sh.user_id, u.username, sh.created_at, coalesce(sh.created_by::text, '') "+
			"FROM dataset_shares sh JOIN users u ON u.id = sh.user_id "+
			"WHERE sh.dataset_id = $1 ORDER BY sh.created_at ASC, sh.user_id ASC",
		datasetID,
	)
	if err != nil {
		return nil, fmt.Errorf("list shares for dataset %q: %w", datasetID, err)
	}
	defer rows.Close()

	var grants []ShareGrant
	for rows.Next() {
		var g ShareGrant
		if err := rows.Scan(&g.UserID, &g.Username, &g.CreatedAt, &g.SharedBy); err != nil {
			return nil, fmt.Errorf("scan share grant row: %w", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share grant rows: %w", err)
	}
	return grants, nil
}

// DataSourceRefUsage returner how many datasets and goals still reference a ref.
// It drives the refcount-aware cleanup: a ref's registry row and object are
// retired only when this returns zero for both tables.
func (s *DatasetStore) DataSourceRefUsage(ctx context.Context, ref string) (datasets, goals int, err error) {
	err = s.pool.QueryRow(ctx,
		"SELECT "+
			"(SELECT count(*)::int FROM datasets WHERE datasource_ref = $1), "+
			"(SELECT count(*)::int FROM goal_registry WHERE datasource_ref = $1)",
		ref,
	).Scan(&datasets, &goals)
	if err != nil {
		return 0, 0, fmt.Errorf("count ref usage for %q: %w", ref, err)
	}
	return datasets, goals, nil
}

// DeleteDataSourceRef removes a ref from the registry once nothing references it
// anymore. No error when the ref is already gone (the object store may have been
// cleaned up independently).
func (s *DatasetStore) DeleteDataSourceRef(ctx context.Context, ref string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM data_source_registry WHERE ref = $1", ref)
	if err != nil {
		return fmt.Errorf("delete data source ref %q: %w", ref, err)
	}
	return nil
}

// isNameConflict classifies a constraint violation from the lower(name) unique
// index as the case-insensitive name conflict it is.
func isNameConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "datasets_name_lower")
	}
	return false
}
