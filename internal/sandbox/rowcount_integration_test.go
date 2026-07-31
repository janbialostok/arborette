package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExecuteReturnsRowCount exercises the seam end to end against a real DuckDB
// scan: the count must come back alongside the objective, must be measured under
// the same filters, must be present even when the objective is SQL NULL, and must
// be absent when the caller did not ask for it.
func TestExecuteReturnsRowCount(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, ctx)
	ref := putObject(t, ctx, client, ".csv", []byte(csvFixture))

	ts := httptest.NewServer(NewServer(client, nil, nil, testLimiter(), 1<<20, 1<<20, "1GiB").Routes())
	defer ts.Close()

	post := func(t *testing.T, body string) map[string]any {
		t.Helper()
		resp, err := http.Post(ts.URL+"/execute", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post /execute: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		value, _ := out["value"].(map[string]any)
		return value
	}

	// avg(amount) where qty > 1 -> 25 over the 2 matching rows.
	value := post(t, `{"data_source_ref":"`+ref+`","aggregation":"avg","target":{"field":"amount"},`+
		`"include_row_count":true,"filters":[{"field":"qty","op":"gt","value":1}]}`)
	if value["amount"] != 25.0 {
		t.Fatalf("objective = %v, want 25", value["amount"])
	}
	if value[rowCountKey] != 2.0 {
		t.Fatalf("row_count = %v, want the 2 filtered rows", value[rowCountKey])
	}

	// An empty-filtered aggregate: the objective is null but support 0 is exactly
	// what a support floor needs to see, so the count must still be present.
	value = post(t, `{"data_source_ref":"`+ref+`","aggregation":"avg","target":{"field":"amount"},`+
		`"include_row_count":true,"filters":[{"field":"qty","op":"gt","value":100}]}`)
	if v, ok := value["amount"]; !ok || v != nil {
		t.Fatalf("expected a null objective over an empty set, got %v", value["amount"])
	}
	if value[rowCountKey] != 0.0 {
		t.Fatalf("row_count = %v, want 0 alongside the null objective", value[rowCountKey])
	}

	// Not requested: the legacy response shape is unchanged.
	value = post(t, `{"data_source_ref":"`+ref+`","aggregation":"avg","target":{"field":"amount"}}`)
	if _, ok := value[rowCountKey]; ok {
		t.Fatalf("row_count must be absent when not requested: %v", value)
	}
}
