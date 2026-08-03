package sandbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/verifier/groundtruth"
	"github.com/arborette/arborette/internal/verifier/stats"
)

// fakeRefValidator lets the handler tests exercise the ref-scoping branch without a
// registry: it reports whether the ref is the one it was seeded with.
type fakeRefValidator struct{ known string }

func (v fakeRefValidator) Validate(_ context.Context, ref string) (bool, error) {
	return ref == v.known, nil
}

func analyzeServer(validator refValidator) *Server {
	// The limiter registers the analyze class so the handler exercises
	// acquireSlotClass(ClassAnalyze); a nil object store and cache are fine because
	// the validation-level cases return before staging.
	limiter := NewClassLimiter(map[string]int{ClassAnalyze: 2})
	return NewServer(nil, validator, nil, limiter, 1<<20, 1<<20, "1GiB", 50, 4, 32)
}

func postAnalyze(t *testing.T, srv *Server, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestAnalyzeRequestValidation covers the schema-independent request-shape checks
// the handler answers before any staging: missing/unknown ref, unknown kind, and the
// per-kind column-count / bin-count caps. These need no object store.
func TestAnalyzeRequestValidation(t *testing.T) {
	srv := analyzeServer(fakeRefValidator{known: "ok.csv"})
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing ref", `{"kind":"contingency","columns":[{"name":"g"}]}`, http.StatusBadRequest},
		{"unknown ref", `{"data_source_ref":"nope.csv","kind":"contingency","columns":[{"name":"g"}]}`, http.StatusNotFound},
		{"unknown kind", `{"data_source_ref":"ok.csv","kind":"histogram","columns":[{"name":"g"}]}`, http.StatusUnprocessableEntity},
		{"contingency no columns", `{"data_source_ref":"ok.csv","kind":"contingency"}`, http.StatusUnprocessableEntity},
		{"contingency over cap", `{"data_source_ref":"ok.csv","kind":"contingency","columns":[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"}]}`, http.StatusUnprocessableEntity},
		{"contingency bad bins", `{"data_source_ref":"ok.csv","kind":"contingency","columns":[{"name":"n","bins":1}]}`, http.StatusUnprocessableEntity},
		{"contingency over-max bins", `{"data_source_ref":"ok.csv","kind":"contingency","columns":[{"name":"n","bins":999}]}`, http.StatusUnprocessableEntity},
		{"moments no variables", `{"data_source_ref":"ok.csv","kind":"moments"}`, http.StatusUnprocessableEntity},
		{"moments over cap", `{"data_source_ref":"ok.csv","kind":"moments","variables":["a","b","c","d","e"]}`, http.StatusUnprocessableEntity},
		{"moments group_by over cap", `{"data_source_ref":"ok.csv","kind":"moments","variables":["a"],"group_by":["b","c","d","e","f"]}`, http.StatusUnprocessableEntity},
		{"invalid body", `{`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := postAnalyze(t, srv, c.body)
			if code != c.want {
				t.Fatalf("status = %d, want %d", code, c.want)
			}
		})
	}
}

func TestFiniteOrZero(t *testing.T) {
	// A NULL stratum sum (no rows) reads as 0, not an error.
	if v, err := finiteOrZero(sql.NullFloat64{Valid: false}); err != nil || v != 0 {
		t.Fatalf("NULL: got (%v, %v), want (0, nil)", v, err)
	}
	if v, err := finiteOrZero(sql.NullFloat64{Valid: true, Float64: 5}); err != nil || v != 5 {
		t.Fatalf("finite: got (%v, %v), want (5, nil)", v, err)
	}
	// A non-finite DOUBLE — an overflow that reached +Inf in DOUBLE space — is
	// rejected rather than emitted in a JSON body that cannot encode it.
	for _, nf := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		if _, err := finiteOrZero(sql.NullFloat64{Valid: true, Float64: nf}); !errors.Is(err, errNonFiniteValue) {
			t.Fatalf("non-finite %v: err = %v, want errNonFiniteValue", nf, err)
		}
	}
}

const analyzeCSV = "g,n\na,1\na,2\nb,3\nb,4\n"

// TestAnalyzeStaging drives the real DuckDB staging path for both kinds: a
// categorical contingency, a moments aggregate, quantile binning, and filter
// binding. Integration-gated (needs the object store).
func TestAnalyzeStaging(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(analyzeCSV))

	limiter := NewClassLimiter(map[string]int{ClassAnalyze: 2})
	srv := NewServer(client, nil, nil, limiter, 1<<20, 1<<20, "1GiB", 50, 4, 32)

	post := func(body string) (int, AnalyzeResponse) {
		req := httptest.NewRequest(http.MethodPost, "/analyze", strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		var resp AnalyzeResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	t.Run("contingency categorical", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"g"}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		counts := map[string]int64{}
		for _, cell := range resp.Cells {
			counts[cell.Values[0]] = cell.Count
		}
		if counts["a"] != 2 || counts["b"] != 2 {
			t.Fatalf("contingency counts = %v, want a:2 b:2", counts)
		}
	})

	t.Run("moments", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"moments","variables":["n"]}`)
		if code != http.StatusOK || len(resp.Moments) != 1 {
			t.Fatalf("status = %d, rows = %d", code, len(resp.Moments))
		}
		m := resp.Moments[0]
		if m.N != 4 || m.Sum[0] != 10 || m.SumSq[0] != 30 {
			t.Fatalf("moments = n:%d sum:%v sumsq:%v, want 4/10/30", m.N, m.Sum, m.SumSq)
		}
	})

	t.Run("quantile binning covers all rows", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"n","bins":2}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		var total int64
		for _, cell := range resp.Cells {
			total += cell.Count
		}
		if total != 4 {
			t.Fatalf("binned cell counts sum to %d, want 4", total)
		}
	})

	t.Run("filter binding", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"g"}],"filters":[{"field":"n","op":"gte","value":3}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		counts := map[string]int64{}
		for _, cell := range resp.Cells {
			counts[cell.Values[0]] = cell.Count
		}
		if counts["a"] != 0 || counts["b"] != 2 {
			t.Fatalf("filtered counts = %v, want only b:2", counts)
		}
	})

	t.Run("post-staging compile error maps to 400", func(t *testing.T) {
		// Binning the categorical g column is a schema-dependent compile error
		// (errNonNumeric) surfaced only after staging; it must map to 400 (analyst-
		// fixable), not a masked 500, so the sweep can tell it from a fault.
		code, _ := post(`{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"g","bins":2}]}`)
		if code != http.StatusBadRequest {
			t.Fatalf("binned categorical column status = %d, want 400", code)
		}
	})
}

// TestAnalyzeGroundTruthParity drives the ground-truth dataset through the real
// DuckDB /analyze path and checks the moments it returns reproduce the planted
// associations (marginal X–Y present, partial X–Y|Z near zero), catching
// compile/binding drift between the in-process fake analyzer and the real SQL.
// Integration-gated (needs the object store).
func TestAnalyzeGroundTruthParity(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)

	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	var buf bytes.Buffer
	if err := csv.NewWriter(&buf).WriteAll(ds.Records()); err != nil {
		t.Fatalf("render csv: %v", err)
	}
	ref := putObject(t, ctx, client, ".csv", buf.Bytes())

	limiter := NewClassLimiter(map[string]int{ClassAnalyze: 2})
	srv := NewServer(client, nil, nil, limiter, 512<<20, 1<<20, "1GiB", 50, 4, 32)

	moments := func(vars string) MomentsRow {
		req := httptest.NewRequest(http.MethodPost, "/analyze",
			strings.NewReader(`{"data_source_ref":"`+ref+`","kind":"moments","variables":[`+vars+`]}`))
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("moments status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp AnalyzeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Moments) != 1 {
			t.Fatalf("decode moments: err=%v rows=%d", err, len(resp.Moments))
		}
		return resp.Moments[0]
	}

	xy := moments(`"X","Y"`)
	if xy.N != int64(len(ds.Rows)) {
		t.Fatalf("moments n = %d, want %d (complete-case over full data)", xy.N, len(ds.Rows))
	}
	marginal, ok := stats.PearsonFromMoments(int(xy.N), xy.Sum[0], xy.Sum[1], xy.SumSq[0], xy.SumSq[1], xy.Cross[0])
	if !ok || math.Abs(marginal) < 0.35 {
		t.Fatalf("real-sandbox marginal corr(X,Y) = %.3f, want >= 0.35", marginal)
	}

	// Partial X–Y given Z, built from the real 3-variable moment matrix, must be near
	// zero — the confounder-removal invariant reproduced through real DuckDB SQL.
	xyz := moments(`"X","Y","Z"`)
	corr := corrMatrix(int(xyz.N), xyz.Sum, xyz.SumSq, xyz.Cross)
	partial, ok := stats.PartialCorrelation(corr)
	if !ok || math.Abs(partial) > 0.12 {
		t.Fatalf("real-sandbox partial corr(X,Y|Z) = %.3f, want <= 0.12", partial)
	}
}

// corrMatrix builds a 3-variable correlation matrix from moment sums, mirroring the
// verifier's own reconstruction (kept local so this CGO test does not import the
// verifier package's unexported helper).
func corrMatrix(n int, sum, sumSq, cross []float64) [][]float64 {
	fn := float64(n)
	s := [3][3]float64{}
	for i := 0; i < 3; i++ {
		s[i][i] = sumSq[i] - sum[i]*sum[i]/fn
	}
	idx := 0
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			v := cross[idx] - sum[i]*sum[j]/fn
			s[i][j], s[j][i] = v, v
			idx++
		}
	}
	out := make([][]float64, 3)
	for i := 0; i < 3; i++ {
		out[i] = make([]float64, 3)
		for j := 0; j < 3; j++ {
			out[i][j] = s[i][j] / math.Sqrt(s[i][i]*s[j][j])
		}
	}
	return out
}
