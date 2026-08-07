# Architecture

The components, the boundaries between them, and which of them may touch which datastore. For what
the system *does*, start at [How it works](../README.md#how-it-works); for the runtime sequence, see
[the discovery flow](discovery-flow.md).

```mermaid
flowchart TB
  classDef svc fill:#1f6feb22,stroke:#1f6feb,stroke-width:1px
  classDef store fill:#8957e522,stroke:#8957e5,stroke-width:1px
  classDef ext fill:#3fb95022,stroke:#3fb950,stroke-width:1px
  classDef client fill:#d2992222,stroke:#d29922,stroke-width:1px

  subgraph Clients
    analyst["Analyst<br/>browser or curl"]
    agent["Downstream AI agent<br/>MCP consumer"]
  end

  subgraph Services["Go services (cmd/)"]
    web["web :8083<br/>Next.js UI + BFF proxy"]
    orch["orchestrator :8080<br/>REST · SSE hub · goal registry<br/>Phase 1 loop · job launchers<br/>/internal/audit"]
    sbx["sandbox :8081<br/>read-only DuckDB queries<br/>+ document extraction<br/>ONLY package using CGO"]
    mcp["mcpserver :8082<br/>read-side MCP<br/>over Streamable HTTP"]
    sleep["sleepcycle-serve :8084<br/>Phase 2 — conjunction search,<br/>publication, abstraction"]
    ver["verifier-serve :8085<br/>Engine B — PC discovery,<br/>backdoor adjustment, refutation"]
  end

  subgraph Init["Init one-shots (completion-gated)"]
    boot["dbbootstrap<br/>provision runtime roles"]
    mig["migrate<br/>apply schema"]
    pull["ollama-pull<br/>fetch embedding model"]
  end

  subgraph Stores["Datastores"]
    neo[("Neo4j<br/>State→Intervention→Outcome triplets<br/>Meta-Heuristic nodes")]
    pg[("Postgres + pgvector<br/>goal registry · runs · audit log<br/>causal_verifications · embeddings")]
    s3[("MinIO / S3<br/>staged datasets")]
    emb["Ollama sidecar :11434<br/>embedding model"]
  end

  subgraph External["External"]
    llm["LLM provider<br/>Anthropic · DeepInfra · Ollama"]
    conn["Anthropic MCP connector<br/>dials inbound over HTTPS"]
  end

  analyst --> web
  analyst -->|REST + SSE| orch
  web -->|proxies REST + SSE| orch
  agent -->|4 MCP tools| mcp

  mcp -->|submit_analyst_goal proxy| orch
  orch -->|introspect · execute · document| sbx
  orch -->|HTTP launcher| sleep
  orch -->|HTTP launcher| ver
  sleep -->|measure candidates| sbx
  ver -->|CI tests · adjusted effects| sbx
  sleep -->|POST /internal/audit| orch
  ver -->|POST /internal/verification-events| orch

  orch --> neo
  orch -->|orchestrator role<br/>only holder of audit privileges| pg
  orch --> s3
  orch --> llm
  orch --> emb
  sbx -->|stage dataset| s3
  sbx -->|service role<br/>data-source ref registry, read-only| pg
  mcp --> neo
  mcp --> pg
  mcp --> emb
  sleep --> neo
  sleep --> pg
  sleep --> emb
  sleep --> llm
  ver --> neo
  ver --> pg
  ver --> llm

  boot --> pg
  mig --> pg
  pull --> emb

  orch -.->|agent chat preview| conn
  conn -.->|MCP_PUBLIC_URL tunnel| mcp

  class web,orch,sbx,mcp,sleep,ver,boot,mig,pull svc
  class neo,pg,s3,emb store
  class llm,conn ext
  class analyst,agent client
```

## Reading the diagram

**The Sandbox is the only measurement surface.** Every number in the graph — Phase-1 triplet values,
Phase-2 candidate measurements, Engine B's naive and adjusted effects — comes back from a read-only
DuckDB query the Sandbox ran. It is also the only package that links DuckDB, so it is the only one
that needs CGO; every other service reaches it through `internal/sandboxclient`, which carries its
own copy of the wire contract. Callers must not import `internal/sandbox`.

**Two services write, three mostly read.** The Orchestrator and the Sleep-Cycle Worker are the only
writers of triplets and Meta-Heuristics; the Verifier writes verification records and graph
versions. The MCP server writes nothing of its own — goal registration is proxied to the
Orchestrator so intake validation cannot fork.

**Postgres access is role-split.** The Orchestrator authenticates as its own role and is the only
one with audit-table privileges, which is why the Sleep-Cycle Worker and the Verifier record through
`POST /internal/audit` rather than writing the table. Everything else uses the shared service role;
the Sandbox's is read-only and exists only to validate data-source refs.

**The launchers are a seam, not a coupling.** `SLEEPCYCLE_WORKER_URL` and `VERIFIER_WORKER_URL`
select an HTTP launcher; unset, the same code path runs the worker as a one-shot batch job
(`make sleep-cycle`, `make discover`). The Orchestrator dispatches the same arguments either way.

**The agent chat preview inverts the direction of every other edge.** Anthropic's infrastructure
dials the MCP server from outside the network, so it needs a public HTTPS URL — the dashed edges.
Unconfigured, that path is inert and nothing else is affected.

**Storage is split three ways with no shared transaction.** A Meta-Heuristic node in Neo4j and its
embedding in pgvector are written separately, which is the cross-store consistency item on the
[roadmap](../README.md#in-flight).
