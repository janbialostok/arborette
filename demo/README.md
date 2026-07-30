# Arborette Demonstration Datasets

This directory contains datasets and scripts for demonstrating Arborette's
segment-discovery engine with real-world business problems. Each dataset
is paired with a concrete business goal and a shell script that walks through
the full pipeline: goal registration, Phase-1 discovery, Phase-2 aggregation,
and result querying.

## Datasets

### 1. Medical Insurance Costs (`insurance.csv`)

| Detail | Value |
|--------|-------|
| Source | [Kaggle: Medical Cost Personal Datasets](https://www.kaggle.com/datasets/mirichoi0218/insurance) |
| Rows | 1,338 |
| Objective | `Maximize insurance charges` |
| Business role | Insurance pricing / underwriting |

**Columns**: `age`, `sex`, `bmi`, `children`, `smoker`, `region`, `charges`

An insurer wants to understand which customer segments drive the highest
medical claims. Finding that e.g. `smoker = yes ∧ bmi > 30 ∧ age > 50`
carries 6× the average charge suggests targeted wellness programs or premium
adjustments — actionable, traceable to the data, defensible in regulation.

**Examples of segments the model naturally proposes that actually yield
measurable effects**:
- `smoker = yes` — ~3-4× baseline charges, high support
- `region = southeast` — higher baseline than others
- `bmi > 30` — elevated charges (compounds with smoker)
- `age > 55` — older cohorts have higher charges
- `children > 2` — larger families show higher utilization

### 2. Brazilian E-commerce Orders (`olist_orders.csv`)

| Detail | Value |
|--------|-------|
| Source | [Kaggle: Brazilian E-commerce](https://www.kaggle.com/datasets/olistbr/brazilian-ecommerce) |
| Rows | 5,000 (sampled from 100k) |
| Objective | `Maximize order value (price + freight)` |
| Business role | E-commerce / retail analytics |

**Columns**: `customer_state`, `customer_city`,
`product_category_name_english`, `price`, `freight_value`, `total_value`,
`order_status`, `order_purchase_timestamp`

A marketplace operator wants to know which segments generate the highest
order values. Finding that e.g. `customer_state = SP ∧
product_category = electronics` has 2.5× the average order, or that
`bed_bath_table` orders to northern states pay disproportionately high
freight, suggests merchandising and logistics strategies — all without
writing a single SQL query.

**Examples of segments the model naturally proposes**:
- `customer_state = SP` — São Paulo is the largest market
- `product_category_name_english = electronics` — high-ticket category
- `product_category_name_english = furniture` — bulky, high freight
- `customer_state = RJ` — Rio de Janeiro, second-largest market
- Combinations of state + category for cross-segment effects

## Scripts

### Quickstart

```bash
# 1. Bring up the stack (if not already running)
make up

# 2. Run a demo
bash demo/run_insurance_demo.sh

# 3. Or try the e-commerce demo
bash demo/run_olist_demo.sh
```

Each script:
1. Registers the goal with the dataset
2. Triggers the hypothesis loop
3. Polls for completion (with timeout protection)
4. Runs the sleep cycle for publication
5. Queries the published Meta-Heuristics
