#!/usr/bin/env bash
set -euo pipefail

ORCHESTRATOR_URL="${ORCHESTRATOR_URL:-http://localhost:8080}"
DATASET="${DATASET:-insurance.csv}"

echo "=========================================="
echo "Arborette Demo: Medical Insurance Costs"
echo "=========================================="
echo ""
echo "Business question: Which customer segments drive the highest"
echo "insurance charges, so we can target wellness programs and"
echo "adjust premiums accordingly?"
echo ""
echo "Dataset: $DATASET (1,338 policyholders)"
echo ""

# 1. Register the goal
echo "--- Step 1: Register goal ---"
RESP=$(curl -sS -X POST "$ORCHORESTRATOR_URL/goals" \
  -F "goal=Maximize insurance charges" \
  -F "import_path=$DATASET")
GOAL_ID=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['optimization_function_id'])")
echo "Goal registered: $GOAL_ID"
echo ""

# 2. Run Phase 1
echo "--- Step 2: Run Phase 1 (hypothesis loop) ---"
curl -sS -X POST "$ORCHESTRATOR_URL/goals/$GOAL_ID/hypothesis-loop" > /dev/null
echo "Loop triggered. Watching for completion (up to 5 min)..."
echo ""
echo "Follow live progress:"
echo "  docker compose logs -f orchestrator"
echo ""

# Poll for completion
for i in $(seq 1 60); do
  sleep 5
  STATUS=$(curl -sS "$ORCHESTRATOR_URL/goals" | python3 -c "
import sys, json
goals = json.load(sys.stdin)
g = [g for g in goals if g['optimization_function_id'] == '$GOAL_ID']
if g:
  s = g[0]['status']
  r = g[0].get('failure_reason', '')
  print(f'{s}|{r}')
else:
  print('not_found|')
")
  STATE=$(echo "$STATUS" | cut -d'|' -f1)
  REASON=$(echo "$STATUS" | cut -d'|' -f2)
  if [ "$STATE" = "completed" ] || [ "$STATE" = "failed" ]; then
    echo "Phase 1: $STATE $REASON"
    break
  fi
  if [ "$STATE" = "not_found" ]; then
    echo "Warning: goal not found yet, retrying..."
  else
    echo "  status: $STATE (waiting...)"
  fi
done
echo ""

# 3. Run Phase 2 (Sleep Cycle)
echo "--- Step 3: Run Phase 2 (sleep cycle) ---"
echo "Starting sleep cycle for $GOAL_ID..."
echo "(This may take several minutes -- the search lattice grows exponentially)"
echo ""

make sleep-cycle GOAL="$GOAL_ID" 2>&1 | grep -E "sleepcycle:|search|abstract|complete"
echo ""

# 4. Query results
echo "--- Step 4: Query published Meta-Heuristics ---"
echo ""
echo "Semantic search for 'insurance charges':"
curl -sS "$ORCHESTRATOR_URL/heuristics/search?q=high+insurance+charges+risk+factors" | python3 -m json.tool
echo ""

echo "=========================================="
echo "Demo complete! Try the Olist e-commerce demo:"
echo "  bash demo/run_olist_demo.sh"
echo "=========================================="
