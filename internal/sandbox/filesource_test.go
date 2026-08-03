package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/testutil"
)

const csvFixture = "qty,amount,name\n1,10.0,a\n2,20.0,b\n3,30.0,c\n"

func newTestClient(t *testing.T, ctx context.Context) *objectstore.Client {
	t.Helper()
	cfg := testutil.RequireIntegration(t)
	client, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return client
}

func putObject(t *testing.T, ctx context.Context, client *objectstore.Client, ext string, body []byte) string {
	t.Helper()
	key := client.NewKey("test", testutil.NewID(t), "data"+ext)
	if err := client.Put(ctx, key, bytes.NewReader(body), "application/octet-stream"); err != nil {
		t.Fatalf("put object: %v", err)
	}
	return key
}

// makeParquet builds a small Parquet fixture with the same shape as csvFixture via
// DuckDB's COPY, exercising the read_parquet branch on read-back.
func makeParquet(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer db.Close()
	_, err = db.Exec("COPY (SELECT * FROM (VALUES (1, 10.0, 'a'), (2, 20.0, 'b'), (3, 30.0, 'c')) AS t(qty, amount, name)) TO '" + path + "' (FORMAT PARQUET)")
	if err != nil {
		t.Fatalf("write parquet: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parquet: %v", err)
	}
	return b
}

// equalStringSet compares two string slices as sets: DISTINCT does not guarantee
// row order, so the distinct-value probe's output is order-independent.
func equalStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func sandboxTempDirs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "arborette-sandbox-*"))
	if err != nil {
		t.Fatalf("glob temp dirs: %v", err)
	}
	return matches
}

func TestIntrospectAndExecute(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)

	formats := []struct {
		name string
		ext  string
		body []byte
	}{
		{"csv", ".csv", []byte(csvFixture)},
		{"parquet", ".parquet", makeParquet(t)},
	}
	for _, f := range formats {
		t.Run(f.name, func(t *testing.T) {
			ref := putObject(t, ctx, client, f.ext, f.body)
			src := NewFileSource(client, nil, ref, 1<<20, "1GiB", 50)

			schema, err := src.Introspect(ctx)
			if err != nil {
				t.Fatalf("introspect: %v", err)
			}
			got := map[string]bool{}
			values := map[string][]string{}
			for _, c := range schema.Columns {
				got[c.Name] = true
				values[c.Name] = c.DistinctValues
			}
			for _, want := range []string{"qty", "amount", "name"} {
				if !got[want] {
					t.Fatalf("introspect missing column %q: %+v", want, schema.Columns)
				}
			}
			// A low-cardinality categorical column returns its distinct values under
			// the cap; a floating-point column is continuous and returns none.
			if !equalStringSet(values["name"], []string{"a", "b", "c"}) {
				t.Fatalf("expected name distinct values {a,b,c}, got %v", values["name"])
			}
			if values["amount"] != nil {
				t.Fatalf("expected no distinct values for a floating-point column, got %v", values["amount"])
			}

			// avg(amount) where qty > 1 -> avg(20, 30) = 25.
			gt := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}
			value, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, gt)
			if err != nil {
				t.Fatalf("execute avg: %v", err)
			}
			if value == nil || *value != 25 {
				t.Fatalf("expected avg 25, got %v", value)
			}

			// Empty set: avg -> nil (SQL NULL), count -> 0.
			none := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 100}}
			emptyAvg, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, nil, none)
			if err != nil {
				t.Fatalf("execute empty avg: %v", err)
			}
			if emptyAvg != nil {
				t.Fatalf("expected nil avg over empty set, got %v", *emptyAvg)
			}
			emptyCount, err := src.Execute(ctx, "count", domain.Target{Field: "amount"}, nil, none)
			if err != nil {
				t.Fatalf("execute empty count: %v", err)
			}
			if emptyCount == nil || *emptyCount != 0 {
				t.Fatalf("expected count 0 over empty set, got %v", emptyCount)
			}
		})
	}
}

// TestDistinctValueProbeCapAndNulls pins the probe's cardinality boundary and NULL
// handling: a column at exactly the cap stays eligible, a column one over returns
// nil (ineligible), and NULLs are excluded from the set so a NULL never becomes a
// grounding value.
func TestDistinctValueProbeCapAndNulls(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	// low: 2 distinct (== cap). high: 3 distinct (> cap). nullable: 2 non-NULL
	// distinct plus one empty field (read as NULL).
	const fixture = "low,high,nullable\na,x,p\na,y,\nb,z,q\n"
	ref := putObject(t, ctx, client, ".csv", []byte(fixture))
	src := NewFileSource(client, nil, ref, 1<<20, "1GiB", 2)

	schema, err := src.Introspect(ctx)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	values := map[string][]string{}
	for _, c := range schema.Columns {
		values[c.Name] = c.DistinctValues
	}
	if !equalStringSet(values["low"], []string{"a", "b"}) {
		t.Fatalf("a column at the cap must return its values, got %v", values["low"])
	}
	if values["high"] != nil {
		t.Fatalf("a column over the cap must return nil, got %v", values["high"])
	}
	if !equalStringSet(values["nullable"], []string{"p", "q"}) {
		t.Fatalf("NULLs must be excluded from the distinct set, got %v", values["nullable"])
	}
}

func TestServerEndpoints(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	ts := httptest.NewServer(NewServer(client, nil, nil, testLimiter(), 1<<20, 1<<20, "1GiB", 50, 4, 32).Routes())
	defer ts.Close()

	post := func(t *testing.T, path, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return resp.StatusCode, out
	}

	// /introspect binds the target to its column case-insensitively.
	code, out := post(t, "/introspect", `{"data_source_ref":"`+ref+`","targets":[{"field":"AMOUNT","direction":"maximize"}]}`)
	if code != http.StatusOK {
		t.Fatalf("introspect status %d: %v", code, out)
	}
	bindings, _ := out["target_bindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("expected one binding: %v", out)
	}
	b0, _ := bindings[0].(map[string]any)
	if b0["matched"] != true || b0["column"] != "amount" {
		t.Fatalf("unexpected binding: %v", b0)
	}

	// /execute avg(amount) where qty > 1 -> 25.
	code, out = post(t, "/execute", `{"data_source_ref":"`+ref+`","aggregation":"avg","target":{"field":"amount"},"filters":[{"field":"qty","op":"gt","value":1}]}`)
	if code != http.StatusOK {
		t.Fatalf("execute status %d: %v", code, out)
	}
	value, _ := out["value"].(map[string]any)
	if value["amount"] != 25.0 {
		t.Fatalf("expected amount 25, got %v", value)
	}

	// Empty set: avg shapes to {"amount": null}.
	code, out = post(t, "/execute", `{"data_source_ref":"`+ref+`","aggregation":"avg","target":{"field":"amount"},"filters":[{"field":"qty","op":"gt","value":100}]}`)
	if code != http.StatusOK {
		t.Fatalf("empty execute status %d: %v", code, out)
	}
	value, _ = out["value"].(map[string]any)
	if v, ok := value["amount"]; !ok || v != nil {
		t.Fatalf("expected null amount over empty set, got %v (present=%v)", value["amount"], ok)
	}

	// Missing key -> 404.
	code, _ = post(t, "/execute", `{"data_source_ref":"test/nope.csv","aggregation":"avg","target":{"field":"amount"}}`)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing key, got %d", code)
	}
}

// exprCSV has a boolean and a categorical column so the expression path can be
// measured end to end against a real DuckDB engine.
const exprCSV = "flag,name,amount,qty\ntrue,gold,10.0,1\nfalse,silver,20.0,2\ntrue,gold,30.0,3\n"

func TestExecuteExpression(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(exprCSV))
	src := NewFileSource(client, nil, ref, 1<<20, "1GiB", 50)

	// avg over a boolean column: (1 + 0 + 1) / 3.
	v, err := src.Execute(ctx, "avg", domain.Target{}, ptrExpr(col("flag")), nil)
	if err != nil {
		t.Fatalf("execute boolean avg: %v", err)
	}
	if v == nil || math.Abs(*v-2.0/3.0) > 1e-9 {
		t.Fatalf("expected boolean rate 0.667, got %v", v)
	}

	// avg over a categorical indicator name = 'gold': (1 + 0 + 1) / 3.
	catExpr := cmp("=", col("name"), strLit("gold"))
	v, err = src.Execute(ctx, "avg", domain.Target{}, &catExpr, nil)
	if err != nil {
		t.Fatalf("execute categorical avg: %v", err)
	}
	if v == nil || math.Abs(*v-2.0/3.0) > 1e-9 {
		t.Fatalf("expected categorical rate 0.667, got %v", v)
	}

	// Division by zero yields +Inf, which the non-finite guard rejects.
	divExpr := domain.Expression{Kind: domain.ArithmeticKind, Op: "/",
		Left:  ptrExpr(col("amount")),
		Right: ptrExpr(numLit(0))}
	if _, err := src.Execute(ctx, "sum", domain.Target{}, &divExpr, nil); !errors.Is(err, errNonFiniteValue) {
		t.Fatalf("expected errNonFiniteValue for divide-by-zero, got %v", err)
	}
}

// TestExecuteLegacyNonFinite proves the guard covers the legacy path too: a source
// DOUBLE column containing inf makes avg non-finite with no expression involved.
func TestExecuteLegacyNonFinite(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte("val\n1.0\ninf\n3.0\n"))
	src := NewFileSource(client, nil, ref, 1<<20, "1GiB", 50)
	if _, err := src.Execute(ctx, "avg", domain.Target{Field: "val"}, nil, nil); !errors.Is(err, errNonFiniteValue) {
		t.Fatalf("expected errNonFiniteValue for legacy avg over inf column, got %v", err)
	}
}

// TestServerExecuteExpression exercises the HTTP contract: a value expression is
// measured and returned keyed by the objective label, and an expression naming an
// unknown column surfaces as a descriptive 400.
func TestServerExecuteExpression(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(exprCSV))

	ts := httptest.NewServer(NewServer(client, nil, nil, testLimiter(), 1<<20, 1<<20, "1GiB", 50, 4, 32).Routes())
	defer ts.Close()

	post := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/execute", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post /execute: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode /execute: %v", err)
		}
		return resp.StatusCode, out
	}

	code, out := post(t, `{"data_source_ref":"`+ref+`","aggregation":"avg","value_expression":{"kind":"column_ref","column":"flag"},"objective_label":"avg(flag)"}`)
	if code != http.StatusOK {
		t.Fatalf("execute status %d: %v", code, out)
	}
	value, _ := out["value"].(map[string]any)
	got, ok := value["avg(flag)"].(float64)
	if !ok || math.Abs(got-2.0/3.0) > 1e-9 {
		t.Fatalf("expected value keyed by label 'avg(flag)' ~0.667, got %v", value)
	}

	code, out = post(t, `{"data_source_ref":"`+ref+`","aggregation":"avg","value_expression":{"kind":"column_ref","column":"ghost"},"objective_label":"avg(ghost)"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown column, got %d: %v", code, out)
	}
	if _, ok := out["error"].(string); !ok {
		t.Fatalf("expected descriptive error body, got %v", out)
	}
}

func TestOverLimitRejected(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	src := NewFileSource(client, nil, ref, 4, "1GiB", 50)
	if _, err := src.Introspect(ctx); !errors.Is(err, errObjectTooLarge) {
		t.Fatalf("expected errObjectTooLarge, got %v", err)
	}
}

func TestTempDirCleanup(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))
	src := NewFileSource(client, nil, ref, 1<<20, "1GiB", 50)

	before := len(sandboxTempDirs(t))

	if _, err := src.Introspect(ctx); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// A forced error after staging (invalid aggregation) must still clean up.
	if _, err := src.Execute(ctx, "median", domain.Target{Field: "amount"}, nil, nil); err == nil {
		t.Fatalf("expected error for invalid aggregation")
	}

	if after := len(sandboxTempDirs(t)); after != before {
		t.Fatalf("temp dir leak: %d before, %d after", before, after)
	}
}
