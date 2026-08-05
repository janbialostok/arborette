package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

// TestStageCacheAmortizesAndFreezesType exercises the cache end to end against a
// real object store and DuckDB: a CSV staged through the cache is downloaded and
// converted exactly once across repeated measurements, the converted Parquet yields
// the same aggregate as the direct-CSV path (the type-freeze spot-check), and the
// cache root is left holding only settled artifacts (no scratch dirs).
func TestStageCacheAmortizesAndFreezesType(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	root := t.TempDir()
	cache, err := NewStageCache(root, 1<<30)
	if err != nil {
		t.Fatalf("new stage cache: %v", err)
	}

	cached := NewFileSource(client, cache, ref, 1<<20, "1GiB", 50)
	gt := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}

	first, err := cached.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt)
	if err != nil {
		t.Fatalf("first cached execute: %v", err)
	}
	if _, err := cached.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt); err != nil {
		t.Fatalf("second cached execute: %v", err)
	}

	// One download+convert across two measurements: objectstore has no Delete, so the
	// fill counter is the only observable that the second call did not re-download.
	if cache.Fills() != 1 {
		t.Fatalf("fills = %d, want exactly 1 across two measurements", cache.Fills())
	}
	if cache.Hits() < 1 {
		t.Fatalf("hits = %d, want the second measurement to hit", cache.Hits())
	}

	// The converted Parquet must measure identically to the raw CSV read (nil cache).
	direct := NewFileSource(client, nil, ref, 1<<20, "1GiB", 50)
	want, err := direct.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt)
	if err != nil {
		t.Fatalf("direct execute: %v", err)
	}
	if first == nil || want == nil || *first != *want {
		t.Fatalf("converted-Parquet aggregate %v != direct-CSV aggregate %v", first, want)
	}

	// The cache root holds only settled artifacts: every fill scratch dir was removed.
	scratches, _ := filepath.Glob(filepath.Join(root, "fill-*"))
	if len(scratches) != 0 {
		t.Fatalf("scratch dirs left in cache root: %v", scratches)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read cache root: %v", err)
	}
	if len(entries) != 1 || entries[0].IsDir() {
		t.Fatalf("cache root should hold exactly one artifact file, got %v", entries)
	}
}

// TestStageCacheParquetPathAndBypass covers the two FileSource cache branches the
// CSV happy-path test does not: a Parquet ref cached as-is (single reservation, read
// back via read_parquet on the extension-less artifact), and the source-level
// errCacheBypass fallback (a cache too small to admit the reservation degrades to
// per-request staging and still measures correctly).
func TestStageCacheParquetPathAndBypass(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".parquet", makeParquet(t))
	gt := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}

	cache, err := NewStageCache(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	src := NewFileSource(client, cache, ref, 1<<20, "1GiB", 50)
	v1, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt)
	if err != nil {
		t.Fatalf("parquet execute 1: %v", err)
	}
	if _, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt); err != nil {
		t.Fatalf("parquet execute 2: %v", err)
	}
	if cache.Fills() != 1 {
		t.Fatalf("parquet fills = %d, want 1 across two measurements", cache.Fills())
	}
	if v1 == nil || *v1 != 25 {
		t.Fatalf("parquet avg = %v, want 25", v1)
	}

	// A cache whose whole budget cannot admit the reservation bypasses to per-request
	// staging: no fill is charged, and the raw-CSV/Parquet reader still measures right.
	tiny, err := NewStageCache(t.TempDir(), 100)
	if err != nil {
		t.Fatalf("tiny cache: %v", err)
	}
	bypassed := NewFileSource(client, tiny, ref, 1<<20, "1GiB", 50)
	v2, err := bypassed.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt)
	if err != nil {
		t.Fatalf("bypass execute: %v", err)
	}
	if tiny.Fills() != 0 {
		t.Fatalf("bypass charged %d fills, want 0 (per-request fallback)", tiny.Fills())
	}
	if v2 == nil || *v2 != 25 {
		t.Fatalf("bypass avg = %v, want 25", v2)
	}
}

// TestStageCacheDocumentRouting covers DocumentSource cache routing: a PDF is cached
// as raw bytes, read back from the extension-less artifact by content via pdf.Open,
// and downloaded once across two reads.
func TestStageCacheDocumentRouting(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	key := putObject(t, ctx, client, ".pdf", minimalPDF(t, "CachedDocMarker"))

	cache, err := NewStageCache(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	src := NewDocumentSource(client, cache, key, 512<<20)

	pages, err := src.Pages(ctx)
	if err != nil {
		t.Fatalf("pages 1: %v", err)
	}
	if len(pages) != 1 || !strings.Contains(pages[0], "CachedDocMarker") {
		t.Fatalf("unexpected pages from cached artifact: %q", pages)
	}
	if _, err := src.Pages(ctx); err != nil {
		t.Fatalf("pages 2: %v", err)
	}
	if cache.Fills() != 1 {
		t.Fatalf("document fills = %d, want 1 across two reads", cache.Fills())
	}
	if cache.Hits() < 1 {
		t.Fatalf("document hits = %d, want the second read to hit", cache.Hits())
	}
}
