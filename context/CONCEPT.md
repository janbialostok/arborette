```markdown
# System Specification: Automated Causal Abstraction Engine (ACAE)
### Tagline: A system which automatically builds domain expert-level knowledge Agents.

---

## 1. System Overview & Intent
The Automated Causal Abstraction Engine (ACAE) is an architecture designed to allow non-technical domain experts (e.g., business analysts, operations managers) to build, seed, and scale cross-model causal knowledge graphs without writing a single line of code. 

The system bypasses traditional query-time context window bloat by decoupling empirical experimentation (**Episodic Memory**) from generalized rule-making (**Semantic Memory**). The ultimate goal is a self-optimizing platform where non-coders organically generate domain-expert level knowledge that can be utilized uniformly across any downstream AI agent model.

---

## 2. Target User Experience (UX) Architecture
The front-end interface abstracts the underlying data engineering and software development entirely away from the analyst through two core interactions:

### A. Natural Language Optimization Function Generation
* **Analyst Action:** The analyst describes their business goal and constraints in plain English (e.g., *"Maximize checkout conversion rates, but ensure server response time stays below 200ms and marketing budget spend doesn't exceed $5,000/month"*).
* **System Action:** An orchestration agent translates these semantic boundaries into an underlying, structured **Evaluation Matrix**. The system maps these directly to targets (objectives to maximize/minimize) and boundaries (hard constraints).

### B. Automated Data Source Wiring
* **Analyst Action:** The analyst points the system to available raw data assets (e.g., operational databases, telemetry streams, CSV uploads).
* **System Action:** The system automatically introspects schemas, identifies relevant temporal variables corresponding to the optimization targets, and wires up sandboxed execution pipelines. The analyst never manually writes connectors or joins.

---

## 3. Core Execution Lifecycle
The system moves through a continuous two-phase cycle: **The Active Hypothesis Loop** (User-driven/Episodic) and **The Sleep Cycle** (Asynchronous/Semantic).


```

[ Analyst Input: Goal & Data ]
│
▼
┌──────────────────────────────────────────────┐
│       ACTIVE HYPOTHESIS TESTING LOOP         │
│  1. LLM proposes Conceptual Hypotheses        │
│  2. Sandboxed Sandbox Engine runs trials      │
│  3. Empirical Causal Triplets generated      │
└──────────────────────┬───────────────────────┘
│ (Streams data via MCP)
▼
┌──────────────────────────────────────────────┐
│         ASYNCHRONOUS SLEEP CYCLE             │
│  1. Background clustering over global graph  │
│  2. Offline LLM abstracts local variations   │
│  3. [Meta-Heuristic] nodes encoded/linked    │
└──────────────────────┬───────────────────────┘
│
▼
[ Cross-Model Expert-Level Knowledge Agent ]

```

### Phase 1: The Active Hypothesis Testing Loop (Conceptual Tree Refinement)
Instead of relying on brittle external git-worktree branching mechanisms, the system executes a native, sandboxed hypothesis tree refinement process:
1. **Hypothesis Generation:** Based on the analyst's optimization function, an LLM generates a directed tree of competing interventions.
2. **Sandboxed Trial Run:** The system runs these interventions through the wired data environments or simulation layers. 
3. **Empirical Grounding:** The system measures the exact delta against the analyst's optimization constraints. The output is a highly strict, non-hallucinated causal triplet written to the graph: `(Measured Start State) -> [Reified Intervention] -> (Measured Outcome State)`.

### Phase 2: The Sleep Cycle & Heuristic Encoding
Once a hypothesis testing cycle completes, the local, task-specific results enter the global consolidation phase:
1. **Asynchronous Clustering:** Language-agnostic background services continuously parse the global graph database using graph density and clustering algorithms (e.g., Louvain community detection) to identify structurally similar interventions executed across different datasets or business departments.
2. **Offline Abstraction:** A background LLM processes these clusters offline to extract the generalized principle (e.g., *"When traffic volume spikes > 2x, reducing third-party tracking pixels by 50% consistently restores latency boundaries without degrading baseline conversion"*).
3. **Heuristic Injection:** This rule is injected as a high-level `[Meta-Heuristic]` node, establishing a hierarchical link down to the specific, empirical episodic triplets that proved it.

---

## 4. Data Model & Schema Definitions
To maintain strict mathematical tracking and ease of navigation for downstream models, the graph schema treats events and actions as primary physical entities:

### Nodes
* **`State`:** High-dimensional vector snapshots or structured telemetry metrics representing a specific point in time.
* **`Intervention`:** A first-class reified entity storing metadata regarding the action taken (e.g., system configuration altered, process change applied, confidence bounds, execution metadata).
* **`Outcome`:** The physical measurement delta resulting from the intervention, explicitly evaluated against the user's optimization function.
* **`Meta-Heuristic`:** Semantic abstractions generated during the sleep cycle summarizing generalized business logic and principles.

### Edges
* **`PRE_CONDITION_FOR`** (State $\rightarrow$ Intervention): Captures environmental contexts required to make an action viable.
* **`PRODUCED`** (Intervention $\rightarrow$ Outcome): Stores calculated effect sizes and probabilistic confidence weights ($0.0 \dots 1.0$) mapped directly from empirical testing.
* **`ABSTRACTED_FROM`** (Meta-Heuristic $\rightarrow$ Intervention/State/Outcome): Connects high-level semantic principles back to the verifiable, auditable raw data chains that support them.

---

## 5. Model Context Protocol (MCP) Interface Design
To ensure that any downstream AI agent model can immediately interface with this seeded graph without custom prompt engineering, the platform exposes a standardized interface via the Model Context Protocol (MCP). The coding agent must implement the following server-side tool schemas:

### `get_optimized_heuristics`
* **Purpose:** Allows a downstream model to query high-level abstracted principles relevant to a novel situation.
* **Input Params:** Semantically embedded string of the current operational state.
* **Returns:** Pre-computed, clear semantic `[Meta-Heuristic]` definitions, avoiding the need for the model to parse raw numerical graphs or calculate edge paths.

### `trace_causal_chain`
* **Purpose:** Provides a full audit trail for any heuristic by returning the underlying raw empirical test runs.
* **Input Params:** `heuristic_id`
* **Returns:** Linearized JSON array of the exact `Start State`, `Intervention`, and `Outcome` metrics generated during Phase 1 testing.

### `submit_analyst_goal`
* **Purpose:** Front-end ingest endpoint for the non-technical analyst interface.
* **Input Params:** `natural_language_goal` (string), `datasource_endpoints` (array)
* **Returns:** Structured `optimization_function_id` and kicks off the Phase 1 active hypothesis cycle.

```