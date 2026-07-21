package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
			src := NewFileSource(client, ref, 1<<20, "1GiB")

			schema, err := src.Introspect(ctx)
			if err != nil {
				t.Fatalf("introspect: %v", err)
			}
			got := map[string]bool{}
			for _, c := range schema.Columns {
				got[c.Name] = true
			}
			for _, want := range []string{"qty", "amount", "name"} {
				if !got[want] {
					t.Fatalf("introspect missing column %q: %+v", want, schema.Columns)
				}
			}

			// avg(amount) where qty > 1 -> avg(20, 30) = 25.
			gt := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}
			value, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, gt)
			if err != nil {
				t.Fatalf("execute avg: %v", err)
			}
			if value == nil || *value != 25 {
				t.Fatalf("expected avg 25, got %v", value)
			}

			// Empty set: avg -> nil (SQL NULL), count -> 0.
			none := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 100}}
			emptyAvg, err := src.Execute(ctx, "avg", domain.Target{Field: "amount"}, none)
			if err != nil {
				t.Fatalf("execute empty avg: %v", err)
			}
			if emptyAvg != nil {
				t.Fatalf("expected nil avg over empty set, got %v", *emptyAvg)
			}
			emptyCount, err := src.Execute(ctx, "count", domain.Target{Field: "amount"}, none)
			if err != nil {
				t.Fatalf("execute empty count: %v", err)
			}
			if emptyCount == nil || *emptyCount != 0 {
				t.Fatalf("expected count 0 over empty set, got %v", emptyCount)
			}
		})
	}
}

func TestServerEndpoints(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	ts := httptest.NewServer(NewServer(client, 1<<20, "1GiB").Routes())
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

func TestOverLimitRejected(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	src := NewFileSource(client, ref, 4, "1GiB")
	if _, err := src.Introspect(ctx); !errors.Is(err, errObjectTooLarge) {
		t.Fatalf("expected errObjectTooLarge, got %v", err)
	}
}

func TestTempDirCleanup(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))
	src := NewFileSource(client, ref, 1<<20, "1GiB")

	before := len(sandboxTempDirs(t))

	if _, err := src.Introspect(ctx); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// A forced error after staging (invalid aggregation) must still clean up.
	if _, err := src.Execute(ctx, "median", domain.Target{Field: "amount"}, nil); err == nil {
		t.Fatalf("expected error for invalid aggregation")
	}

	if after := len(sandboxTempDirs(t)); after != before {
		t.Fatalf("temp dir leak: %d before, %d after", before, after)
	}
}
