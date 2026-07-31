// Package sandbox is the Sandbox Execution service's own layer: the file-backed
// DataSource reader, the deterministic intervention-query compiler, and the HTTP
// handlers. It deliberately owns the only DuckDB-over-object-store access path in
// the system. DuckDB lives here, behind this service's boundary, rather than in
// the shared internal/datasource interface package: the mainstream driver is
// CGO-based, and putting it on the shared seam would force CGO on every importer
// of the interface (orchestrator, mcpserver, sleepcycle). internal/datasource
// stays the pure, dependency-free contract it is today; this package implements it.
package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objectstore"

	_ "github.com/marcboeker/go-duckdb/v2"
)

// Sentinel errors let the HTTP layer map staging/validation failures to status
// codes without string matching.
var (
	errObjectTooLarge    = errors.New("data source exceeds size limit")
	errUnsupportedFormat = errors.New("unsupported data source format")
)

// FileSource reads a single tabular object (CSV or Parquet) out of the object
// store and answers introspection and aggregate queries over it via a transient,
// in-memory DuckDB engine. It is constructed with primitive args (per the infra-
// constructor convention). It is stateless in semantics: every call reads a fresh
// engine over the ref it carries. The optional cache is a pure performance layer
// keyed on the immutable content-addressed ref -- it changes only whether the
// download and parse are re-paid, never what a read returns. A nil cache preserves
// the original per-request staging.
type FileSource struct {
	objects        *objectstore.Client
	cache          *StageCache
	dataSourceRef  string
	maxObjectBytes int64
	maxTempDirSize string
}

// NewFileSource builds a FileSource for one object-store ref. cache is optional
// (nil ⇒ per-request staging). maxObjectBytes caps the staged copy; maxTempDirSize
// is a DuckDB size string (with a unit) bounding query spill.
func NewFileSource(objects *objectstore.Client, cache *StageCache, dataSourceRef string, maxObjectBytes int64, maxTempDirSize string) *FileSource {
	return &FileSource{
		objects:        objects,
		cache:          cache,
		dataSourceRef:  dataSourceRef,
		maxObjectBytes: maxObjectBytes,
		maxTempDirSize: maxTempDirSize,
	}
}

var _ datasource.DataSource = (*FileSource)(nil)

// Kind reports the source shape. This reader handles only tabular objects.
func (s *FileSource) Kind() datasource.SourceKind { return datasource.KindTabular }

// Introspect stages the object once and reports its columns. Read-only-ness is
// guaranteed by emitting only DESCRIBE/SELECT over the ephemeral connection, not
// by any access_mode.
func (s *FileSource) Introspect(ctx context.Context) (*datasource.Schema, error) {
	db, tableFn, cleanup, err := s.stage(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cols, err := columns(ctx, db, tableFn)
	if err != nil {
		return nil, err
	}
	return &datasource.Schema{Kind: datasource.KindTabular, Columns: cols}, nil
}

// Execute measures one aggregate over the object under the given filters. It
// stages once (sharing the download + engine with introspection's path), reads
// the schema to validate the compiled query against, runs the single SELECT, and
// returns the scalar. When expr is non-nil the aggregate measures that compiled
// objective value expression; otherwise it measures the bare target column (the
// legacy path). A nil result means the aggregate filtered to an empty set (SQL
// NULL) for avg/sum/min/max; count over an empty set returns 0, not nil.
func (s *FileSource) Execute(ctx context.Context, agg string, target domain.Target, expr *domain.Expression, filters []domain.Constraint) (*float64, error) {
	m, err := s.ExecuteCounted(ctx, agg, target, expr, filters, false)
	if err != nil {
		return nil, err
	}
	return m.Value, nil
}

// Measurement is one execute result: the aggregate Value (nil when the filters
// matched nothing and the aggregate scanned as SQL NULL) plus, when the caller
// asked for it, the matched RowCount. Counted reports whether RowCount was
// measured, distinguishing a real zero from an unrequested one.
type Measurement struct {
	Value    *float64
	RowCount int64
	Counted  bool
}

// ExecuteCounted is Execute with an opt-in matched-row count measured in the
// same scan, so a caller that needs support alongside the objective pays one
// scan rather than a second round-trip. The count is populated even when the
// objective scans as SQL NULL: an empty-filtered aggregate with support 0 is
// exactly the case a support floor must see. count(*) never returns NULL, so it
// scans into a plain int64.
func (s *FileSource) ExecuteCounted(ctx context.Context, agg string, target domain.Target, expr *domain.Expression, filters []domain.Constraint, withCount bool) (Measurement, error) {
	db, tableFn, cleanup, err := s.stage(ctx)
	if err != nil {
		return Measurement{}, err
	}
	defer cleanup()

	cols, err := columns(ctx, db, tableFn)
	if err != nil {
		return Measurement{}, err
	}

	var query string
	var args []any
	if expr != nil {
		query, args, err = compileObjectiveCounted(tableFn, cols, agg, *expr, filters, withCount)
	} else {
		query, args, err = compileQueryCounted(tableFn, cols, agg, target, filters, withCount)
	}
	if err != nil {
		return Measurement{}, err
	}

	var value sql.NullFloat64
	measurement := Measurement{Counted: withCount}
	row := db.QueryRowContext(ctx, query, args...)
	if withCount {
		err = row.Scan(&value, &measurement.RowCount)
	} else {
		err = row.Scan(&value)
	}
	if err != nil {
		return Measurement{}, fmt.Errorf("execute aggregate: %w", err)
	}
	if !value.Valid {
		return measurement, nil
	}
	// A non-finite DOUBLE scans as a valid float64 but json.Marshal cannot encode
	// it, so reject it here rather than emit a truncated 200 body: an expression
	// division by zero (+Inf/NaN) or a source column literally containing inf both
	// reach this guard regardless of which compiler produced the query.
	if math.IsInf(value.Float64, 0) || math.IsNaN(value.Float64) {
		return Measurement{}, errNonFiniteValue
	}
	measurement.Value = &value.Float64
	return measurement, nil
}

// stage resolves the source file (from the cache when configured, else a fresh
// per-request download) and opens a configured in-memory DuckDB over it, returning
// the engine, the table-function expression both Introspect and Execute build
// their SQL around, and a cleanup that closes the *sql.DB (an unclosed one leaks
// native handles under load), releases any cache reference, and removes the temp
// dir (DuckDB spill; the source file lives in the cache, not here, when cached).
// cleanup must be deferred on every path, including a failure after sql.Open.
func (s *FileSource) stage(ctx context.Context) (db *sql.DB, tableFn string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "arborette-sandbox-")
	if err != nil {
		return nil, "", nil, fmt.Errorf("create staging dir: %w", err)
	}
	var release func()
	cleanup = func() {
		if db != nil {
			db.Close()
		}
		if release != nil {
			release()
		}
		os.RemoveAll(dir)
	}

	sourcePath, fnName, rel, err := s.stageSource(ctx, dir)
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	release = rel

	db, err = sql.Open("duckdb", "")
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("open duckdb: %w", err)
	}
	// Spill goes to the per-request temp dir (the source file lives in the cache when
	// cached, so this dir holds only spill).
	if err := configureSpill(ctx, db, dir, s.maxTempDirSize); err != nil {
		cleanup()
		return nil, "", nil, err
	}

	// DuckDB resolves the table-function file argument at bind time and rejects a
	// bound ? there, so the staged path is embedded as a literal. It is safe: the
	// path is a server-generated name (a temp dir or the cache's sha256 artifact),
	// never request-derived.
	tableFn = fnName + "('" + escapeLiteral(sourcePath) + "')"
	return db, tableFn, cleanup, nil
}

// stageSource resolves the on-disk source file, the DuckDB reader that opens it, and
// a release the caller's cleanup must call (a no-op for the non-cache paths). The
// reader dispatch depends on which path staged the file, not on the ref's extension:
// a cache artifact is Parquet at an extension-less path (CSV is converted once at
// fill time; Parquet caches as-is), read with read_parquet, so tableFunction's
// extension keying cannot serve it. A nil cache or an errCacheBypass falls back to a
// per-request download into dir under the raw extension-keyed reader.
func (s *FileSource) stageSource(ctx context.Context, dir string) (path, fnName string, release func(), err error) {
	rawFn, ext, err := tableFunction(s.dataSourceRef)
	if err != nil {
		return "", "", nil, err
	}

	// perRequest stages the raw object into the request's own temp dir under the
	// extension-keyed reader -- the fallback when the cache is disabled or bypasses.
	perRequest := func() (string, string, func(), error) {
		staged := filepath.Join(dir, "source"+ext)
		if err := s.download(ctx, staged); err != nil {
			return "", "", nil, err
		}
		return staged, rawFn, func() {}, nil
	}

	if s.cache == nil {
		return perRequest()
	}

	// A CSV fill transiently needs the raw download plus the Parquet artifact, so it
	// reserves twice maxObjectBytes; a Parquet fill caches as-is.
	isCSV := ext == ".csv"
	reserve := s.maxObjectBytes
	if isCSV {
		reserve = 2 * s.maxObjectBytes
	}
	cachePath, rel, err := s.cache.Acquire(ctx, s.dataSourceRef, reserve, s.fill(isCSV))
	if errors.Is(err, errCacheBypass) {
		return perRequest()
	}
	if err != nil {
		return "", "", nil, err
	}
	return cachePath, "read_parquet", rel, nil
}

// fill returns the cache FillFunc for this ref. A Parquet ref caches the staged
// bytes unchanged (parse is ~free on read-back); a CSV ref downloads into the
// fill's scratch dir and converts once to Parquet at dest, so every later read
// amortizes both the download and the parse.
func (s *FileSource) fill(isCSV bool) FillFunc {
	return func(ctx context.Context, scratch, dest string) error {
		if !isCSV {
			return s.download(ctx, dest)
		}
		raw := filepath.Join(scratch, "source.csv")
		if err := s.download(ctx, raw); err != nil {
			return err
		}
		return convertCSVToParquet(ctx, raw, dest, s.maxTempDirSize)
	}
}

// download streams the object into dest via the shared bounded-staging helper.
func (s *FileSource) download(ctx context.Context, dest string) error {
	return stageBoundedObject(ctx, s.objects, s.dataSourceRef, dest, s.maxObjectBytes)
}

// convertCSVToParquet converts a staged CSV into a Parquet artifact at dest using a
// throwaway in-memory DuckDB. The COPY must name FORMAT PARQUET explicitly (a bare
// COPY ... TO defaults to CSV output) and write to exactly dest (the extension-less
// path the cache stats, serves, and evicts). Conversion spill is pointed at a
// per-fill temp dir *outside* the cache root -- an in-memory DuckDB otherwise
// spills to ./.tmp, unwritable on the distroless image, and directing it into the
// accounted cache root would let a conversion exceed the cache's disk budget by up
// to the spill bound. Spill is bounded by maxTempDirSize, the same exposure the
// query path already has.
func convertCSVToParquet(ctx context.Context, csvPath, dest, maxTempDirSize string) error {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open duckdb for conversion: %w", err)
	}
	defer db.Close()

	spill, err := os.MkdirTemp("", "arborette-convert-")
	if err != nil {
		return fmt.Errorf("create conversion spill dir: %w", err)
	}
	defer os.RemoveAll(spill)
	if err := configureSpill(ctx, db, spill, maxTempDirSize); err != nil {
		return err
	}

	copyQuery := "COPY (SELECT * FROM read_csv_auto('" + escapeLiteral(csvPath) + "')) TO '" +
		escapeLiteral(dest) + "' (FORMAT PARQUET)"
	if _, err := db.ExecContext(ctx, copyQuery); err != nil {
		return fmt.Errorf("convert csv to parquet: %w", err)
	}
	return nil
}

// configureSpill points a DuckDB connection's spill at dir and bounds it by
// maxTempDirSize. An in-memory DuckDB otherwise defaults to ./.tmp under the working
// dir, which is not writable on the distroless runtime image, so any spilling query
// would fail. Both are GLOBAL-scope settings valid over database/sql; dir and
// maxTempDirSize are embedded as single-quote-escaped literals (server-generated
// names, never request-derived).
func configureSpill(ctx context.Context, db *sql.DB, dir, maxTempDirSize string) error {
	if _, err := db.ExecContext(ctx, "SET temp_directory='"+escapeLiteral(dir)+"'"); err != nil {
		return fmt.Errorf("set temp directory: %w", err)
	}
	if _, err := db.ExecContext(ctx, "SET max_temp_directory_size='"+escapeLiteral(maxTempDirSize)+"'"); err != nil {
		return fmt.Errorf("set max temp directory size: %w", err)
	}
	return nil
}

// stageBoundedObject streams an object-store object into dest, bounded so an
// object larger than maxBytes is rejected rather than silently truncated:
// LimitReader is given one extra byte so an over-limit read is detected, not
// swallowed at a boundary (which would yield a silently wrong aggregate for a
// table, or wrong extracted text for a document). The copy runs under ctx so a
// slow Get is cancellable. Shared by both the tabular and document readers so the
// subtle +1 over-limit trick lives in one place.
func stageBoundedObject(ctx context.Context, objects *objectstore.Client, ref, dest string, maxBytes int64) error {
	r, err := objects.Get(ctx, ref)
	if err != nil {
		return err
	}
	defer r.Close()

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, ctxReader{ctx: ctx, r: io.LimitReader(r, maxBytes+1)})
	if err != nil {
		return fmt.Errorf("stage object: %w", err)
	}
	if n > maxBytes {
		return fmt.Errorf("%w: object exceeds %d bytes", errObjectTooLarge, maxBytes)
	}
	return nil
}

// columns runs DESCRIBE over the table function and maps the result into Columns.
// DuckDB's DESCRIBE returns column_name, column_type, null, key, default, extra;
// only the first two are load-bearing here.
func columns(ctx context.Context, db *sql.DB, tableFn string) ([]datasource.Column, error) {
	rows, err := db.QueryContext(ctx, "DESCRIBE SELECT * FROM "+tableFn)
	if err != nil {
		return nil, fmt.Errorf("describe source: %w", err)
	}
	defer rows.Close()

	var cols []datasource.Column
	for rows.Next() {
		var name, typ string
		var null, key, dflt, extra sql.NullString
		if err := rows.Scan(&name, &typ, &null, &key, &dflt, &extra); err != nil {
			return nil, fmt.Errorf("scan column: %w", err)
		}
		cols = append(cols, datasource.Column{Name: name, Type: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describe source: %w", err)
	}
	return cols, nil
}

// tableFunction selects the DuckDB reader for the ref's extension and returns the
// function name plus the extension to stage the file under.
func tableFunction(ref string) (fnName, ext string, err error) {
	switch {
	case strings.HasSuffix(strings.ToLower(ref), ".csv"):
		return "read_csv_auto", ".csv", nil
	case strings.HasSuffix(strings.ToLower(ref), ".parquet"):
		return "read_parquet", ".parquet", nil
	default:
		return "", "", fmt.Errorf("%w: %q", errUnsupportedFormat, ref)
	}
}

// escapeLiteral escapes a string for embedding inside a single-quoted SQL literal
// by doubling embedded single quotes.
func escapeLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ctxReader makes an io.Reader observe context cancellation between reads so a
// slow or stalled download can be aborted by the request context.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}
