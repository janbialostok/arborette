package sandbox

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"

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

func TestHandlerValidation(t *testing.T) {
	// nil object store is safe: every case returns before any staging.
	h := NewServer(nil, 1<<20, "1GiB").Routes()
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"execute bad json", "/execute", "{not json", http.StatusBadRequest},
		{"execute empty ref", "/execute", `{"aggregation":"avg","target":{"field":"a"}}`, http.StatusBadRequest},
		{"execute extract type", "/execute", `{"data_source_ref":"x.csv","type":"extract","aggregation":"avg","target":{"field":"a"}}`, http.StatusBadRequest},
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
