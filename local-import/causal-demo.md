# causal-demo.csv — bundled ground-truth dataset

A synthetic 2,000-row dataset with a **planted causal structure**, generated
deterministically by `internal/verifier/groundtruth`. It is the regression
fixture for causal-discovery accuracy (precision/recall against the known graph)
and the demo substrate that exercises Engine A, Engine B, and the Sleep Cycle at
default config. Regenerate with:

```
go run ./internal/verifier/groundtruth/cmd/gen -out local-import/causal-demo.csv
```

## Columns

| Column   | Type    | Role |
|----------|---------|------|
| `Z`      | DOUBLE  | Confounder — causes both `X` and `Y` |
| `X`      | DOUBLE  | `X = 1.5·Z + noise`; correlated with `Y` only through `Z` |
| `A`      | DOUBLE  | Independent true cause of `Y` |
| `B`      | VARCHAR | Categorical `{b0, b1, b2}`; part of the `B×C` interaction |
| `C`      | BOOLEAN | `{true, false}`; part of the `B×C` interaction |
| `Y`      | DOUBLE  | Outcome |
| `region` | VARCHAR | Pure-noise categorical `{north, south, east, west}` — no edge |

## Planted graph

```
Z → X
Z → Y
A → Y
B → Y     (via the B×C interaction)
C → Y     (via the B×C interaction)
```

`region` has no edge and must be left unconnected by discovery.

## Effect sizes

- `Y = 1.2·Z + 1.0·A + 2.0·1[B=b1 ∧ C=true] + noise`, each noise term standard normal.
- **Confounder:** `X` and `Y` are spuriously correlated (marginal r ≈ 0.5) but
  independent given `Z` — the edge Engine B must remove at conditioning level 1.
- **True effect:** `A → Y` survives adjustment (partial correlation given the
  other parents ≈ 0.7).
- **Interaction:** `Y` rises by 2.0 (≈1σ) only in the cell `B=b1 ∧ C=true`
  (~1/6 of rows, ≥100 rows), so both `B` and `C` are causes of `Y` while the
  effect concentrates where conjunction search finds it. The cell clears 3× the
  default sleep-cycle `MinSupport`, and both `B` and `C` carry detectable
  marginal associations with `Y`.

## Which CI-test kinds it exercises

- **Numeric×numeric** → partial correlation (Fisher-z): `Z`, `X`, `A`, `Y`.
- **Categorical×categorical / categorical×boolean** → G-test: `B`, `C`, `region`.
- **Mixed** → binned hybrid: e.g. `B`–`Y`, `C`–`Y`.
