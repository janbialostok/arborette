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

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
	"github.com/arborette/arborette/internal/verifier/stats"
)

// TestAnalyzeWireParity pins the byte-for-byte JSON compatibility between the sandbox
// wire structs and their CGO-free sandboxclient twins for the effect kinds: a client
// request must decode into the server struct field-for-field, and a server stratum
// response must decode into the client struct. The interface-fake path cannot catch a
// tag drift; this does.
func TestAnalyzeWireParity(t *testing.T) {
	seg := 1.5
	base := 3.5
	clientReq := sandboxclient.AnalyzeRequest{
		DataSourceRef:        "ref.csv",
		Kind:                 sandboxclient.AnalyzeStratifiedEffect,
		Aggregation:          "avg",
		ValueExpression:      &domain.Expression{Kind: domain.ColumnRefKind, Column: "Y"},
		Segment:              []domain.Constraint{{Field: "X", Op: domain.GreaterThanOrEqual, Value: 0.5}},
		Adjust:               []sandboxclient.AnalyzeColumn{{Name: "Z", Bins: 4}},
		SampleFraction:       0.7,
		RandomStratifierBins: 4,
	}
	var serverReq AnalyzeRequest
	if err := roundTrip(clientReq, &serverReq); err != nil {
		t.Fatalf("request round-trip: %v", err)
	}
	if serverReq.Kind != clientReq.Kind || serverReq.Aggregation != "avg" ||
		serverReq.ValueExpression == nil || serverReq.ValueExpression.Column != "Y" ||
		len(serverReq.Segment) != 1 || serverReq.Segment[0].Field != "X" ||
		len(serverReq.Adjust) != 1 || serverReq.Adjust[0].Name != "Z" || serverReq.Adjust[0].Bins != 4 ||
		serverReq.SampleFraction != 0.7 || serverReq.RandomStratifierBins != 4 {
		t.Fatalf("request did not round-trip field-for-field: %+v", serverReq)
	}

	serverResp := AnalyzeResponse{Kind: AnalyzeStratifiedEffect, Strata: []StratumRow{
		{Values: []string{"0"}, N: 100, SegmentN: 40, SegmentAgg: &seg, BaselineN: 60, BaselineAgg: &base},
		{Values: []string{"1"}, N: 50, SegmentN: 20, SegmentAgg: nil, BaselineN: 30, BaselineAgg: &base},
	}}
	var clientResp sandboxclient.AnalyzeResponse
	if err := roundTrip(serverResp, &clientResp); err != nil {
		t.Fatalf("response round-trip: %v", err)
	}
	if len(clientResp.Strata) != 2 {
		t.Fatalf("strata count = %d, want 2", len(clientResp.Strata))
	}
	s0 := clientResp.Strata[0]
	if s0.N != 100 || s0.SegmentN != 40 || s0.SegmentAgg == nil || *s0.SegmentAgg != 1.5 || s0.BaselineN != 60 {
		t.Fatalf("stratum 0 did not round-trip: %+v", s0)
	}
	// A null aggregate must decode to a nil pointer, not a zero.
	if clientResp.Strata[1].SegmentAgg != nil {
		t.Fatalf("a null segment aggregate must decode to nil, got %v", *clientResp.Strata[1].SegmentAgg)
	}
}

func roundTrip(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

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
		{"effect no aggregation", `{"data_source_ref":"ok.csv","kind":"stratified_effect","value_expression":{"kind":"column_ref","column":"y"}}`, http.StatusUnprocessableEntity},
		{"effect adjust over cap", `{"data_source_ref":"ok.csv","kind":"stratified_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"y"},"adjust":[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"}]}`, http.StatusUnprocessableEntity},
		{"effect bad bins", `{"data_source_ref":"ok.csv","kind":"stratified_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"y"},"adjust":[{"name":"n","bins":1}]}`, http.StatusUnprocessableEntity},
		{"effect random bins over max", `{"data_source_ref":"ok.csv","kind":"stratified_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"y"},"random_stratifier_bins":999}`, http.StatusUnprocessableEntity},
		{"sampled fraction zero", `{"data_source_ref":"ok.csv","kind":"sampled_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"y"},"sample_fraction":0}`, http.StatusUnprocessableEntity},
		{"sampled fraction over one", `{"data_source_ref":"ok.csv","kind":"sampled_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"y"},"sample_fraction":1.5}`, http.StatusUnprocessableEntity},
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
		// fixable), not a masked 500, so the sweep can tell it from a fault. Both
		// analyze kinds bin columns through one helper, so both are asserted: the
		// quantile probe runs ahead of the compiler that would otherwise catch this,
		// and an unguarded probe reaches DuckDB as a binder error that masks to 500.
		binned := map[string]string{
			"contingency":       `{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"g","bins":2}]}`,
			"stratified_effect": `{"data_source_ref":"` + ref + `","kind":"stratified_effect","aggregation":"avg","value_expression":{"kind":"column_ref","column":"n"},"segment":[{"field":"n","op":"gt","value":1}],"adjust":[{"name":"g","bins":2}]}`,
			"sampled_effect":    `{"data_source_ref":"` + ref + `","kind":"sampled_effect","sample_fraction":1,"aggregation":"avg","value_expression":{"kind":"column_ref","column":"n"},"segment":[{"field":"n","op":"gt","value":1}],"adjust":[{"name":"g","bins":2}]}`,
		}
		for kind, body := range binned {
			code, _ := post(body)
			if code != http.StatusBadRequest {
				t.Fatalf("%s: binned categorical column status = %d, want 400", kind, code)
			}
		}

		// An unknown binned column is resolved before the probe too, for the same
		// reason: it is analyst-fixable, not a fault.
		unknown := `{"data_source_ref":"` + ref + `","kind":"contingency","columns":[{"name":"nope","bins":2}]}`
		if code, _ := post(unknown); code != http.StatusBadRequest {
			t.Fatalf("unknown binned column status = %d, want 400", code)
		}
	})
}

// TestAnalyzeEffectStaging drives the real DuckDB staging path for the effect kinds:
// a Z-less naive stratified_effect (one stratum, both arms), a binned-Z stratified
// effect (per-stratum arms), the synthetic random stratifier, and a sampled_effect at
// fraction 1.0 (reservoir at 100% equals the unsampled result). Integration-gated
// (needs the object store).
func TestAnalyzeEffectStaging(t *testing.T) {
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

	// The segment g = 'a' has rows n∈{1,2}; the baseline (g = 'b') has n∈{3,4}.
	segment := `"aggregation":"avg","value_expression":{"kind":"column_ref","column":"n"},"segment":[{"field":"g","op":"eq","operand":{"string":"a"}}]`

	t.Run("naive z-less", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"stratified_effect",` + segment + `}`)
		if code != http.StatusOK || len(resp.Strata) != 1 {
			t.Fatalf("status = %d, strata = %d", code, len(resp.Strata))
		}
		s := resp.Strata[0]
		if s.N != 4 || s.SegmentN != 2 || s.BaselineN != 2 {
			t.Fatalf("counts = n:%d segN:%d baseN:%d, want 4/2/2", s.N, s.SegmentN, s.BaselineN)
		}
		if s.SegmentAgg == nil || *s.SegmentAgg != 1.5 || s.BaselineAgg == nil || *s.BaselineAgg != 3.5 {
			t.Fatalf("aggs = seg:%v base:%v, want 1.5/3.5", s.SegmentAgg, s.BaselineAgg)
		}
	})

	t.Run("binned adjustment strata", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"stratified_effect",` + segment + `,"adjust":[{"name":"n","bins":2}]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		var total int64
		for _, s := range resp.Strata {
			total += s.N
		}
		if total != 4 {
			t.Fatalf("stratum counts sum to %d, want 4", total)
		}
	})

	t.Run("random stratifier adds a key column", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"stratified_effect",` + segment + `,"random_stratifier_bins":2}`)
		if code != http.StatusOK || len(resp.Strata) == 0 {
			t.Fatalf("status = %d, strata = %d", code, len(resp.Strata))
		}
		var total int64
		for _, s := range resp.Strata {
			if len(s.Values) != 1 {
				t.Fatalf("random-stratified values = %v, want one bucket key", s.Values)
			}
			total += s.N
		}
		if total != 4 {
			t.Fatalf("random-stratum counts sum to %d, want 4", total)
		}
	})

	t.Run("sampled fraction 1.0 equals unsampled", func(t *testing.T) {
		code, resp := post(`{"data_source_ref":"` + ref + `","kind":"sampled_effect",` + segment + `,"sample_fraction":1.0}`)
		if code != http.StatusOK || len(resp.Strata) != 1 {
			t.Fatalf("status = %d, strata = %d", code, len(resp.Strata))
		}
		if resp.Strata[0].N != 4 {
			t.Fatalf("sampled n = %d, want 4 (reservoir at 100%% is the whole set)", resp.Strata[0].N)
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
