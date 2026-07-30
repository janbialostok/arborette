#!/usr/bin/env bash
set -euo pipefail

ORCHESTRATOR_URL="${ORCHESTRATOR_URL:-http://localhost:8080}"
DATASET="${DATASET:-olist_orders.csv}"

echo "=========================================="
echo "Arborette Demo: Brazilian E-commerce"
echo "=========================================="
echo ""
echo "Business question: Which customer and product segments drive"
echo "the highest order values, so we can target merchandising and"
echo "logistics strategies?"
echo ""
echo "Dataset: $DATASET (5,000 Brazilian orders)"
echo ""

# 1. Register the goal
echo "--- Step 1: Register goal ---"
RESP=$(curl -sS -X POST "$ORCHESTRATOR_URL/goals" \
  -F "goal=Maximize order value (price plus freight)" \
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
  echo "  status: $STATE (waiting...)"
done
echo ""

# 3. Run Phase 2 (Sleep Cycle)
echo "--- Step 3: Run Phase 2 (sleep cycle) ---"
echo "Starting sleep cycle for $GOAL_ID..."
make sleep-cycle GOAL="$GOAL_ID" 2>&1 | grep -E "sleepcycle:|search|abstract|complete"
echo ""

# 4. Query results
echo "--- Step 4: Query published Meta-Heuristics ---"
echo ""
echo "Semantic search for order value:"
curl -sS "$ORCHESTRATOR_URL/heuristics/search?q=high+order+value+ecommerce+segments" | python3 -m json.tool
echo ""

echo "=========================================="
echo "Demo complete!"
echo "=========================================="
