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
	"strconv"
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
	objects          *objectstore.Client
	cache            *StageCache
	dataSourceRef    string
	maxObjectBytes   int64
	maxTempDirSize   string
	distinctValueCap int
}

// NewFileSource builds a FileSource for one object-store ref. cache is optional
// (nil ⇒ per-request staging). maxObjectBytes caps the staged copy; maxTempDirSize
// is a DuckDB size string (with a unit) bounding query spill. distinctValueCap is
// the low-cardinality cutoff for introspection's value probe (0 disables it); it
// is unused on the execute path, which never introspects values.
func NewFileSource(objects *objectstore.Client, cache *StageCache, dataSourceRef string, maxObjectBytes int64, maxTempDirSize string, distinctValueCap int) *FileSource {
	return &FileSource{
		objects:          objects,
		cache:            cache,
		dataSourceRef:    dataSourceRef,
		maxObjectBytes:   maxObjectBytes,
		maxTempDirSize:   maxTempDirSize,
		distinctValueCap: distinctValueCap,
	}
}

var _ datasource.DataSource = (*FileSource)(nil)

// Kind reports the source shape. This reader handles only tabular objects.
func (s *FileSource) Kind() datasource.SourceKind { return datasource.KindTabular }

// Introspect stages the object once and reports its columns. Read-only-ness is
// guaranteed by emitting only DESCRIBE/SELECT over the ephemeral connection, not
// by any access_mode.
func (s *FileSource) Introspect(ctx context.Context) (*datasource.Schema, error) {
	return s.IntrospectQuantiles(ctx, 0)
}

// IntrospectQuantiles is Introspect with an opt-in quantile probe: with bins at
// least 2 every numeric column additionally reports its bins-1 interior cut
// points, over the same staged engine, so a caller deriving threshold predicates
// pays no extra download or parse. Zero bins is exactly Introspect, which is what
// keeps the extension additive for every existing caller.
func (s *FileSource) IntrospectQuantiles(ctx context.Context, bins int) (*datasource.Schema, error) {
	db, tableFn, cleanup, err := s.stage(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cols, err := columns(ctx, db, tableFn)
	if err != nil {
		return nil, err
	}
	if err := s.probeDistinctValues(ctx, db, tableFn, cols); err != nil {
		return nil, err
	}
	if err := s.probeQuantileCuts(ctx, db, tableFn, cols, bins); err != nil {
		return nil, err
	}
	return &datasource.Schema{Kind: datasource.KindTabular, Columns: cols}, nil
}

// probeQuantileCuts fills QuantileCuts for each numeric column over the same staged
// engine. A column whose quantiles all came back NULL — constant or empty — is left
// nil rather than carrying an empty set, so a consumer cannot mistake "no usable
// boundary" for "probed and none needed".
func (s *FileSource) probeQuantileCuts(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column, bins int) error {
	if bins < 2 {
		return nil
	}
	for i := range cols {
		if !isNumeric(cols[i].Type) {
			continue
		}
		cuts, err := s.quantileCuts(ctx, db, tableFn, cols[i].Name, bins)
		if err != nil {
			return err
		}
		if len(cuts) > 0 {
			cols[i].QuantileCuts = cuts
		}
	}
	return nil
}

// probeDistinctValues fills DistinctValues for each low-cardinality categorical
// column (text/boolean/integer) over the same staged engine introspection already
// opened, so value grounding pays no extra download or parse. A continuous or
// high-cardinality column is left nil (ineligible). A zero/negative cap disables
// probing. It mutates cols in place -- the slice shares its backing array with the
// caller's, so the filled values travel back without a copy.
func (s *FileSource) probeDistinctValues(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column) error {
	if s.distinctValueCap <= 0 {
		return nil
	}
	for i := range cols {
		if !isProbeableForDistinctValues(cols[i].Type) {
			continue
		}
		values, err := distinctValues(ctx, db, tableFn, cols[i].Name, s.distinctValueCap)
		if err != nil {
			return err
		}
		cols[i].DistinctValues = values
	}
	return nil
}

// distinctValues reads up to limit+1 distinct non-NULL values of one column, cast
// to VARCHAR, over the staged engine. The limit+1 read is the over-limit trick: a
// column returning more than limit distinct values is ineligible for grounding, so
// it returns (nil, nil) rather than a truncated set that would falsely reject
// valid proposals. The VARCHAR cast makes every probed type read back as a
// comparable string that the value post-check's stringification matches; NULLs are
// excluded. The column name is a schema-derived identifier, quoted through the one
// sanctioned path; the limit is a server-side integer, formatted directly.
func distinctValues(ctx context.Context, db *sql.DB, tableFn, col string, limit int) ([]string, error) {
	q := "SELECT DISTINCT CAST(" + quoteIdent(col) + " AS VARCHAR) FROM " + tableFn +
		" WHERE " + quoteIdent(col) + " IS NOT NULL LIMIT " + strconv.Itoa(limit+1)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("probe distinct values: %w", err)
	}
	defer rows.Close()

	var values []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan distinct value: %w", err)
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("probe distinct values: %w", err)
	}
	if len(values) > limit {
		return nil, nil
	}
	return values, nil
}

// Execute measures one aggregate over the object under the given filters. It
// stages once (sharing the download + engine with introspection's path), reads
// the schema to validate the compiled query against, runs the single SELECT, and
// returns the scalar. When expr is non-nil the aggregate measures that compiled
// objective value expression; otherwise it measures the bare target column (the
// legacy path). A nil result means the aggregate filtered to an empty set (SQL
// NULL) for avg/sum/min/max; count over an empty set returns 0, not nil.
func (s *FileSource) Execute(ctx context.Context, agg string, target domain.Target, expr *domain.Expression, filters []domain.Constraint) (*float64, error) {
	m, err := s.ExecuteCounted(ctx, agg, target, expr, filters, false, "", "")
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
// scans into a plain int64. entityKey/timeColumn are the window bindings a windowed
// value expression compiles against; both empty for a plain aggregate objective.
func (s *FileSource) ExecuteCounted(ctx context.Context, agg string, target domain.Target, expr *domain.Expression, filters []domain.Constraint, withCount bool, entityKey, timeColumn string) (Measurement, error) {
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
		query, args, err = compileObjectiveCounted(tableFn, cols, agg, *expr, filters, withCount, entityKey, timeColumn)
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

// Analyze stages the object once and answers a contingency or moments aggregation
// over it — the read-only surface the causal-discovery sweep runs its
// conditional-independence tests through. It reuses the same staging, schema
// validation, and injection-safe compilers as Execute; a contingency request first
// computes each binned column's quantile cut points over the same staged engine, so
// binning pays no extra download.
func (s *FileSource) Analyze(ctx context.Context, req AnalyzeRequest) (AnalyzeResponse, error) {
	db, tableFn, cleanup, err := s.stage(ctx)
	if err != nil {
		return AnalyzeResponse{}, err
	}
	defer cleanup()

	cols, err := columns(ctx, db, tableFn)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	switch req.Kind {
	case AnalyzeContingency:
		return s.analyzeContingency(ctx, db, tableFn, cols, req)
	case AnalyzeMoments:
		return s.analyzeMoments(ctx, db, tableFn, cols, req)
	case AnalyzeStratifiedEffect, AnalyzeSampledEffect:
		return s.analyzeEffect(ctx, db, tableFn, cols, req)
	default:
		return AnalyzeResponse{}, fmt.Errorf("%w: %q", errAnalyzeUnknownKind, req.Kind)
	}
}

func (s *FileSource) analyzeContingency(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column, req AnalyzeRequest) (AnalyzeResponse, error) {
	cuts, err := s.binnedCuts(ctx, db, tableFn, cols, req.Columns)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	query, args, err := compileContingency(tableFn, cols, req.Columns, req.Filters, cuts)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("contingency query: %w", err)
	}
	defer rows.Close()

	n := len(req.Columns)
	var cells []ContingencyCell
	for rows.Next() {
		scanTargets := make([]any, n+1)
		values := make([]string, n)
		for i := range values {
			scanTargets[i] = &values[i]
		}
		var count int64
		scanTargets[n] = &count
		if err := rows.Scan(scanTargets...); err != nil {
			return AnalyzeResponse{}, fmt.Errorf("scan contingency cell: %w", err)
		}
		cells = append(cells, ContingencyCell{Values: values, Count: count})
	}
	if err := rows.Err(); err != nil {
		return AnalyzeResponse{}, fmt.Errorf("contingency query: %w", err)
	}
	return AnalyzeResponse{Kind: AnalyzeContingency, Cells: cells}, nil
}

func (s *FileSource) analyzeMoments(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column, req AnalyzeRequest) (AnalyzeResponse, error) {
	query, args, nVars, err := compileMoments(tableFn, cols, req.Variables, req.GroupBy, req.Filters)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("moments query: %w", err)
	}
	defer rows.Close()

	nGroup := len(req.GroupBy)
	nCross := nVars * (nVars - 1) / 2
	var moments []MomentsRow
	for rows.Next() {
		group := make([]string, nGroup)
		var count int64
		// perVarMoments holds each variable's sum and sum-of-squares interleaved
		// (index 2v is the sum, 2v+1 the sum-of-squares), the order compileMoments
		// emits them in.
		perVarMoments := make([]sql.NullFloat64, 2*nVars)
		cross := make([]sql.NullFloat64, nCross)

		scanTargets := make([]any, 0, nGroup+1+2*nVars+nCross)
		for i := range group {
			scanTargets = append(scanTargets, &group[i])
		}
		scanTargets = append(scanTargets, &count)
		for i := range perVarMoments {
			scanTargets = append(scanTargets, &perVarMoments[i])
		}
		for i := range cross {
			scanTargets = append(scanTargets, &cross[i])
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return AnalyzeResponse{}, fmt.Errorf("scan moments row: %w", err)
		}

		row := MomentsRow{N: count, Sum: make([]float64, nVars), SumSq: make([]float64, nVars), Cross: make([]float64, nCross)}
		if nGroup > 0 {
			row.Group = group
		}
		for v := 0; v < nVars; v++ {
			if row.Sum[v], err = finiteOrZero(perVarMoments[2*v]); err != nil {
				return AnalyzeResponse{}, err
			}
			if row.SumSq[v], err = finiteOrZero(perVarMoments[2*v+1]); err != nil {
				return AnalyzeResponse{}, err
			}
		}
		for i := range cross {
			if row.Cross[i], err = finiteOrZero(cross[i]); err != nil {
				return AnalyzeResponse{}, err
			}
		}
		moments = append(moments, row)
	}
	if err := rows.Err(); err != nil {
		return AnalyzeResponse{}, fmt.Errorf("moments query: %w", err)
	}
	return AnalyzeResponse{Kind: AnalyzeMoments, Moments: moments}, nil
}

// analyzeEffect stages once and answers a stratified or sampled backdoor-adjustment
// aggregation: it precomputes each binned adjustment column's quantile cut points
// over the full staged engine (so bin boundaries stay stable across a sampled
// subsample), compiles the per-stratum query, and scans each row into a StratumRow.
// The segment and baseline aggregates scan through sql.NullFloat64 so an empty arm
// stays a null pointer rather than a spurious zero — the positivity signal the caller
// gates on.
func (s *FileSource) analyzeEffect(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column, req AnalyzeRequest) (AnalyzeResponse, error) {
	cuts, err := s.binnedCuts(ctx, db, tableFn, cols, req.Adjust)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	query, args, err := compileStratifiedEffect(tableFn, cols, req, cuts)
	if err != nil {
		return AnalyzeResponse{}, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return AnalyzeResponse{}, fmt.Errorf("effect query: %w", err)
	}
	defer rows.Close()

	nGroup := len(req.Adjust)
	if req.RandomStratifierBins > 0 {
		nGroup++
	}
	var strata []StratumRow
	for rows.Next() {
		values := make([]string, nGroup)
		var n, segN, baseN int64
		var segAgg, baseAgg sql.NullFloat64
		scanTargets := make([]any, 0, nGroup+5)
		for i := range values {
			scanTargets = append(scanTargets, &values[i])
		}
		scanTargets = append(scanTargets, &n, &segN, &segAgg, &baseN, &baseAgg)
		if err := rows.Scan(scanTargets...); err != nil {
			return AnalyzeResponse{}, fmt.Errorf("scan stratum row: %w", err)
		}
		row := StratumRow{Values: values, N: n, SegmentN: segN, BaselineN: baseN}
		if row.SegmentAgg, err = finitePtr(segAgg); err != nil {
			return AnalyzeResponse{}, err
		}
		if row.BaselineAgg, err = finitePtr(baseAgg); err != nil {
			return AnalyzeResponse{}, err
		}
		strata = append(strata, row)
	}
	if err := rows.Err(); err != nil {
		return AnalyzeResponse{}, fmt.Errorf("effect query: %w", err)
	}
	return AnalyzeResponse{Kind: req.Kind, Strata: strata}, nil
}

// binnedCuts precomputes the quantile cut points of every binned column in a
// request, keyed by the column's request index for the compile step to bind.
//
// It resolves and type-checks each column first, because the probe runs ahead of
// the compiler that would otherwise catch it: quantile_cont over a categorical
// column reaches DuckDB as a binder error, which masks to a 500, where the same
// request rejected at compile time is the analyst-fixable 400 that says which
// column cannot be binned. Both analyze kinds share it so the two cannot diverge on
// which error a caller sees.
func (s *FileSource) binnedCuts(ctx context.Context, db *sql.DB, tableFn string, cols []datasource.Column, columns []AnalyzeColumn) (map[int][]float64, error) {
	cuts := map[int][]float64{}
	for i, c := range columns {
		if c.Bins <= 0 {
			continue
		}
		col, err := resolveColumn(cols, c.Name)
		if err != nil {
			return nil, err
		}
		if !isNumeric(col.Type) {
			return nil, fmt.Errorf("%w: binned column %q (%s)", errNonNumeric, col.Name, col.Type)
		}
		got, err := s.quantileCuts(ctx, db, tableFn, col.Name, c.Bins)
		if err != nil {
			return nil, err
		}
		cuts[i] = got
	}
	return cuts, nil
}

// quantileCuts reads one column's bins-1 interior quantile cut points over the
// staged engine, dropping any that came back NULL (an empty or degenerate column),
// so binExpr collapses to a single bucket rather than binding a NULL boundary.
func (s *FileSource) quantileCuts(ctx context.Context, db *sql.DB, tableFn, colName string, bins int) ([]float64, error) {
	row := db.QueryRowContext(ctx, quantileCutsSQL(tableFn, colName, bins))
	raw := make([]sql.NullFloat64, bins-1)
	targets := make([]any, bins-1)
	for i := range raw {
		targets[i] = &raw[i]
	}
	if err := row.Scan(targets...); err != nil {
		return nil, fmt.Errorf("quantile cuts for %q: %w", colName, err)
	}
	cuts := make([]float64, 0, bins-1)
	for _, c := range raw {
		if c.Valid && !math.IsInf(c.Float64, 0) && !math.IsNaN(c.Float64) {
			cuts = append(cuts, c.Float64)
		}
	}
	return cuts, nil
}

// finiteOrZero unwraps a scanned aggregate sum: an SQL NULL (an empty stratum)
// becomes 0, and a non-finite DOUBLE — an overflow that reached +Inf in DOUBLE
// space rather than erroring mid-scan — is rejected as errNonFiniteValue (mapped to
// 400) rather than emitted in a JSON body that cannot encode it.
func finiteOrZero(v sql.NullFloat64) (float64, error) {
	if !v.Valid {
		return 0, nil
	}
	if math.IsInf(v.Float64, 0) || math.IsNaN(v.Float64) {
		return 0, errNonFiniteValue
	}
	return v.Float64, nil
}

// finitePtr unwraps a scanned stratum aggregate into a nullable pointer: an SQL NULL
// (an empty segment/baseline arm) becomes nil so the caller sees an unpopulated arm
// rather than a zero, and a non-finite DOUBLE is rejected as errNonFiniteValue (a
// JSON body cannot encode it) rather than emitted.
func finitePtr(v sql.NullFloat64) (*float64, error) {
	if !v.Valid {
		return nil, nil
	}
	if math.IsInf(v.Float64, 0) || math.IsNaN(v.Float64) {
		return nil, errNonFiniteValue
	}
	f := v.Float64
	return &f, nil
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
