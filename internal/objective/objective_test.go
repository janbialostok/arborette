package objective_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
)

func TestPin(t *testing.T) {
	t.Run("pins the first target", func(t *testing.T) {
		matrix := domain.EvaluationMatrix{Targets: []domain.Target{
			{Field: "revenue", Direction: domain.Maximize, Aggregation: "sum"},
			{Field: "cost", Direction: domain.Minimize, Aggregation: "avg"},
		}}
		obj, err := objective.Pin(matrix)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if obj.Aggregation != "sum" || obj.Direction != domain.Maximize || obj.Label != "sum(revenue)" {
			t.Fatalf("unexpected objective: %+v", obj)
		}
		if obj.Expr.Kind != domain.ColumnRefKind || obj.Expr.Column != "revenue" {
			t.Fatalf("a field-only target should degenerate to a bare ColumnRef: %+v", obj.Expr)
		}
	})

	t.Run("no target is terminal", func(t *testing.T) {
		if _, err := objective.Pin(domain.EvaluationMatrix{}); !errors.Is(err, objective.ErrNoObjective) {
			t.Fatalf("error = %v, want ErrNoObjective", err)
		}
	})

	t.Run("no aggregation is terminal", func(t *testing.T) {
		matrix := domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}}
		if _, err := objective.Pin(matrix); !errors.Is(err, objective.ErrMissingAggregation) {
			t.Fatalf("error = %v, want ErrMissingAggregation", err)
		}
	})
}

// TestExecuteRequestFor pins the seam this package exists for: the hypothesis
// loop and the Sleep-Cycle Worker must shape an identical request, so a measured
// value is carried under the same label end to end.
func TestExecuteRequestFor(t *testing.T) {
	obj, err := objective.Pin(domain.EvaluationMatrix{Targets: []domain.Target{
		{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"},
	}})
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	filters := []domain.Constraint{{Field: "region", Op: domain.GreaterThanOrEqual, Value: 1}}
	req := objective.ExecuteRequestFor("ref.csv", obj, filters)

	if req.DataSourceRef != "ref.csv" || req.Aggregation != "avg" || req.ObjectiveLabel != "avg(revenue)" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if req.ValueExpression == nil || req.ValueExpression.Column != "revenue" {
		t.Fatalf("value expression not carried: %+v", req.ValueExpression)
	}
	// The expression carries the objective, so Target.Field must stay empty or the
	// sandbox measures a bare column instead.
	if req.Target.Field != "" {
		t.Fatalf("target field must be empty: %+v", req.Target)
	}
	if req.Target.Direction != domain.Maximize || len(req.Filters) != 1 {
		t.Fatalf("unexpected request: %+v", req)
	}
	// The count is opt-in; only the search asks for it.
	if req.IncludeRowCount {
		t.Fatal("the shared request must not request a row count by default")
	}
}

// TestNumericValue covers both JSON number shapes. json.Number is not exotic
// here: any decoder configured with UseNumber produces it, so a regression in
// that branch would be invisible to a fake that only ever yields float64.
func TestNumericValue(t *testing.T) {
	cases := map[string]struct {
		value map[string]any
		want  float64
		ok    bool
	}{
		"float64":     {map[string]any{"f": 12.5}, 12.5, true},
		"json.Number": {map[string]any{"f": json.Number("7")}, 7, true},
		"nil value":   {map[string]any{"f": nil}, 0, false},
		"missing key": {map[string]any{}, 0, false},
		"string":      {map[string]any{"f": "12"}, 0, false},
		"bad number":  {map[string]any{"f": json.Number("nope")}, 0, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := objective.NumericValue(c.value, "f")
			if ok != c.ok || got != c.want {
				t.Fatalf("= (%v, %v), want (%v, %v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestImproves(t *testing.T) {
	if !objective.Improves(10, 12, domain.Maximize) || objective.Improves(10, 8, domain.Maximize) {
		t.Fatal("maximize: only a larger value improves")
	}
	if !objective.Improves(10, 8, domain.Minimize) || objective.Improves(10, 12, domain.Minimize) {
		t.Fatal("minimize: only a smaller value improves")
	}
	if objective.Improves(10, 10, domain.Maximize) || objective.Improves(10, 10, domain.Minimize) {
		t.Fatal("an equal value improves in neither direction")
	}
}
