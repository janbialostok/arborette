package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/service"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

func TestWriteStageErrStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"modeled not found", fmt.Errorf("get object: %w", &types.NoSuchKey{}), http.StatusNotFound},
		{"api not found", fmt.Errorf("get object: %w", &smithy.GenericAPIError{Code: "NoSuchKey"}), http.StatusNotFound},
		{"api not found alias", fmt.Errorf("get object: %w", &smithy.GenericAPIError{Code: "NotFound"}), http.StatusNotFound},
		{"unknown field", fmt.Errorf("%w: x", errUnknownField), http.StatusBadRequest},
		{"ambiguous field", fmt.Errorf("%w: x", errAmbiguousField), http.StatusBadRequest},
		{"non numeric", fmt.Errorf("%w: x", errNonNumeric), http.StatusBadRequest},
		{"unknown aggregation", fmt.Errorf("%w: x", errUnknownAggregation), http.StatusBadRequest},
		{"unknown operator", fmt.Errorf("%w: x", errUnknownOperator), http.StatusBadRequest},
		{"unsupported format", fmt.Errorf("%w: x", errUnsupportedFormat), http.StatusBadRequest},
		{"too large", fmt.Errorf("%w: x", errObjectTooLarge), http.StatusBadRequest},
		{"masked internal", errors.New("boom internal detail"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeStageErr(rec, c.err)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
			if c.want == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "boom internal detail") {
				t.Fatalf("internal error detail leaked in body: %q", rec.Body.String())
			}
		})
	}
}

func TestBindTargets(t *testing.T) {
	cols := []datasource.Column{{Name: "Amount", Type: "DOUBLE"}, {Name: "qty", Type: "BIGINT"}}

	got, err := bindTargets([]domain.Target{{Field: "amount"}, {Field: "missing"}}, cols)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(got))
	}
	if !got[0].Matched || got[0].Column != "Amount" {
		t.Fatalf("case-insensitive match should bind to actual column: %+v", got[0])
	}
	if got[1].Matched || got[1].Column != "" {
		t.Fatalf("no-match should be Matched:false with empty column: %+v", got[1])
	}

	ambiguous := []datasource.Column{{Name: "Amount"}, {Name: "amount"}}
	if _, err := bindTargets([]domain.Target{{Field: "AMOUNT"}}, ambiguous); err == nil {
		t.Fatalf("expected ambiguous-column error")
	}
}

// testLimiter is the real limiter the handler tests drive: a small default-class
// cap, never nil, since the nil-store validation path reaches staticValidate only
// after Acquire succeeds.
func testLimiter() *classLimiter {
	return NewClassLimiter(map[string]int{ClassDefault: 8})
}

func TestHandlerValidation(t *testing.T) {
	// nil object store, cache, and validator are safe: every case returns before any
	// staging. A real limiter is required because Acquire precedes staticValidate.
	h := NewServer(nil, nil, nil, testLimiter(), 1<<20, 1<<20, "1GiB", 50, 4, 32).Routes()
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"execute bad json", "/execute", "{not json", http.StatusBadRequest},
		{"execute empty ref", "/execute", `{"aggregation":"avg","target":{"field":"a"}}`, http.StatusBadRequest},
		{"execute extract type", "/execute", `{"data_source_ref":"x.csv","type":"extract","aggregation":"avg","target":{"field":"a"}}`, http.StatusBadRequest},
		// Static validation returns 400 before any staging: with a nil object store
		// a reached download would 500 or panic, so a 400 proves it never staged.
		{"execute bad aggregation", "/execute", `{"data_source_ref":"x.csv","aggregation":"median","target":{"field":"a"}}`, http.StatusBadRequest},
		{"execute bad operator", "/execute", `{"data_source_ref":"x.csv","aggregation":"avg","value_expression":{"kind":"comparison","op":"~=","left":{"kind":"column_ref","column":"a"},"right":{"kind":"literal","literal":{"number":1}}},"objective_label":"x"}`, http.StatusBadRequest},
		{"execute bad cast", "/execute", `{"data_source_ref":"x.csv","aggregation":"avg","value_expression":{"kind":"cast","cast_type":"INTEGER","operand":{"kind":"column_ref","column":"a"}},"objective_label":"x"}`, http.StatusBadRequest},
		{"introspect bad json", "/introspect", "{nope", http.StatusBadRequest},
		{"introspect empty ref", "/introspect", `{"targets":[]}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// fakeValidator is the ref-scoping seam under test: it answers a fixed verdict
// (and optional error) without a Postgres connection.
type fakeValidator struct {
	ok  bool
	err error
}

func (f fakeValidator) Validate(context.Context, string) (bool, error) { return f.ok, f.err }

// TestRefScoping proves an unregistered ref is rejected with the same 404 shape an
// object-store miss produces (no registered-vs-missing oracle), and a validator
// fault masks to 500 -- both before any staging, so the nil object store is never
// reached.
func TestRefScoping(t *testing.T) {
	body := `{"data_source_ref":"x.csv","aggregation":"avg","target":{"field":"a"}}`
	cases := []struct {
		name      string
		validator refValidator
		want      int
		wantBody  string
	}{
		{"unregistered ref", fakeValidator{ok: false}, http.StatusNotFound, "data source not found"},
		{"validator fault", fakeValidator{err: errors.New("db down")}, http.StatusInternalServerError, "internal error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServer(nil, c.validator, nil, testLimiter(), 1<<20, 1<<20, "1GiB", 50, 4, 32).Routes()
			req := httptest.NewRequest(http.MethodPost, "/execute", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", rec.Body.String(), c.wantBody)
			}
		})
	}
}

// TestRoutesComposeWithBearerAuth proves every sandbox route inherits auth when the
// mux is wrapped in the shared bearer guard (the one wrap cmd/sandbox applies): a
// request with no credential is rejected before reaching a handler, and a correct
// one passes through to the handler's own validation.
func TestRoutesComposeWithBearerAuth(t *testing.T) {
	h := service.BearerAuth("s3cret", NewServer(nil, nil, nil, testLimiter(), 1<<20, 1<<20, "1GiB", 50, 4, 32).Routes())

	unauth := httptest.NewRequest(http.MethodPost, "/execute", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, unauth)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential: status = %d, want 401", rec.Code)
	}

	authed := httptest.NewRequest(http.MethodPost, "/execute", strings.NewReader(`{}`))
	authed.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authed)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("authorized empty ref: status = %d, want 400 (past auth, into handler)", rec.Code)
	}
}

func TestTableFunction(t *testing.T) {
	if fn, ext, err := tableFunction("dir/data.CSV"); err != nil || fn != "read_csv_auto" || ext != ".csv" {
		t.Fatalf("csv: got (%q, %q, %v)", fn, ext, err)
	}
	if fn, ext, err := tableFunction("dir/data.PARQUET"); err != nil || fn != "read_parquet" || ext != ".parquet" {
		t.Fatalf("parquet: got (%q, %q, %v)", fn, ext, err)
	}
	if _, _, err := tableFunction("dir/data.txt"); !errors.Is(err, errUnsupportedFormat) {
		t.Fatalf("expected errUnsupportedFormat, got %v", err)
	}
}
