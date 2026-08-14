// Package config loads arborette's shared, env-based configuration. Every
// cmd/<service> main constructs a Config and passes the pieces it needs to the
// internal packages. Role passwords come from the environment and are never
// committed; see .env.example for the full variable list.
package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the union of every service's configuration. A given service only
// reads the fields it needs.
type Config struct {
	Neo4j        Neo4jConfig
	Postgres     PostgresConfig
	S3           S3Config
	Ollama       OllamaConfig
	Embedding    EmbeddingConfig
	Sandbox      SandboxConfig
	Orchestrator OrchestratorConfig
	MCP          MCPConfig
	SleepCycle   SleepCycleConfig
	Verifier     VerifierConfig
	LLM          LLMConfig
}

// Neo4jConfig holds the bolt connection details for the graph store.
type Neo4jConfig struct {
	URI      string
	User     string
	Password string
}

// PostgresConfig holds the host/port/db plus the three role credential sets
// that back the owner and two runtime DSNs. Owner credentials are used only by
// the bootstrap and migrate jobs; runtime services use the runtime roles.
type PostgresConfig struct {
	Host    string
	Port    string
	DB      string
	SSLMode string

	OwnerUser            string
	OwnerPassword        string
	OrchestratorUser     string
	OrchestratorPassword string
	ServiceUser          string
	ServicePassword      string
}

// S3Config drives the object-store client. Endpoint + PathStyle select MinIO
// locally; leaving Endpoint empty uses AWS S3 with virtual-host addressing.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// OllamaConfig points at the embedding sidecar's HTTP API.
type OllamaConfig struct {
	URL   string
	Model string
}

// EmbeddingConfig fixes the vector dimension and the retrieval distance floor.
// Dimension is load-bearing: it must match both the pgvector column and the
// model's output width. Swapping providers is a migration + full re-embed, not a
// runtime toggle. DistanceFloor is the cosine distance beyond which a similarity
// hit is dropped, so a query far from the whole corpus returns empty instead of
// the corpus ranked by how distant it is; it is env-tunable pending calibration.
type EmbeddingConfig struct {
	Dimension     int
	DistanceFloor float64
}

// SandboxConfig drives the Sandbox Execution HTTP service. MaxObjectBytes caps how
// much of a data source the sandbox will stage locally before rejecting it,
// bounding disk use; MaxTempDirSize is a DuckDB size string (e.g. 2GiB, with a unit
// -- never a bare byte count) passed verbatim into SET max_temp_directory_size to
// bound query-spill blast radius.
//
// The staging cache amortizes the download and parse of a data source across a
// run's many measurements. StageCacheDir is the cache root (empty ⇒ a boot-time
// temp dir); StageCacheMaxBytes is its disk budget (0 disables the cache, restoring
// per-request staging) -- LRU-by-size eviction under that budget is the eviction
// policy, so there is no separate eviction knob. ExecuteConcurrency is the default
// request class's concurrent-slot count; MaxBodyBytes caps each request body.
//
// DistinctValueCap is the low-cardinality cutoff for value grounding: a
// categorical column (text/boolean/integer) with at most this many distinct
// values has them returned by introspection, so the tree-proposal prompt can be
// grounded on the real values and the deterministic value post-check can reject
// invented ones; a column above the cap is treated as high-cardinality and
// returns no values. A zero/negative value disables the probe.
//
// InternalAuthToken is the shared secret the sandbox verifies on every route -- the
// same INTERNAL_AUTH_TOKEN the orchestrator's internal write surfaces verify and
// the callers present (empty disables the guard).
//
// The analysis surface (POST /analyze) gets its own rate/concurrency class:
// AnalyzeConcurrency is that class's slot count, separate from ExecuteConcurrency so
// a discovery sweep's many round-trips do not starve the hypothesis loop's execute
// calls (or vice versa). AnalyzeMaxColumns bounds the per-kind column count (2
// endpoints + the conditioning-set bound; must be >= the verifier's 2 +
// DiscoveryMaxCondSet); AnalyzeMaxBins bounds a quantile-binned column's bin count.
type SandboxConfig struct {
	Port               string
	MaxObjectBytes     int64
	MaxTempDirSize     string
	StageCacheDir      string
	StageCacheMaxBytes int64
	ExecuteConcurrency int
	AnalyzeConcurrency int
	AnalyzeMaxColumns  int
	AnalyzeMaxBins     int
	MaxBodyBytes       int64
	DistinctValueCap   int
	InternalAuthToken  string
}

// OrchestratorConfig drives the REST/API service. SandboxURL is the compose
// hostname of the Sandbox Execution service the goal-intake and hypothesis loop
// call; AnalystID backs the stub identity every audit record is stamped with;
// SleepCycleJobName is the named job the Phase-2 trigger launches; LocalImportDir
// is the read-only mount the on-disk ingestion path resolves relative paths
// against (a path escaping it is rejected).
//
// SleepCycleWorkerURL selects how the Phase-2 trigger dispatches: set to the
// worker's serve-mode launch endpoint (an HTTPLauncher POSTs the args map to it)
// and empty to keep the StubLauncher, which logs and returns success while the
// AWS Batch SubmitJob seam stays the production path. The value is the full
// endpoint URL including the /runs path -- the launcher POSTs to it verbatim and
// appends nothing, which is what lets a differently-pathed worker (the Verifier)
// reuse the same launcher type without redesign.
//
// HITLConfidenceThreshold is the service-wide default below which an extracted
// value is queued for human review; a goal may override it. BlockingLoopTimeout
// bounds a run whose goal opted into blocking epoch mode -- such a run waits on
// human review at each queued node, so it needs a human-scale deadline rather
// than the loop's own machine-scale one.
//
// InternalAuthToken is the shared secret guarding the internal write surfaces
// this service exposes to the other services (a secret with no default; empty
// disables the guard). SleepCycleConfig documents why both sides read one variable.
//
// The router knobs govern when causal verification runs; orchestrator.RouterConfig
// documents what each one gates. AutoPromoteShrinkageK defaults to the Sleep Cycle's
// support floor because the orchestrator holds none of its own -- the hypothesis loop
// deliberately does not prune on support, so there is nothing here to derive one from.
type OrchestratorConfig struct {
	Port                    string
	SandboxURL              string
	SleepCycleWorkerURL     string
	VerifierWorkerURL       string
	AnalystID               string
	SleepCycleJobName       string
	LocalImportDir          string
	InternalAuthToken       string
	HITLConfidenceThreshold float64
	BlockingLoopTimeout     time.Duration
	AutoPromoteEnabled      bool
	AutoPromoteTopN         int
	AutoPromoteShrinkageK   int
	StaleReverifyCap        int
	// SessionCookieName is the name of the browser cookie the orchestrator
	// issues after sign-in and that the BFF proxy tunnels to the frontend.
	// SessionSecure marks that cookie Secure-only; it is false by default so
	// local (http) development keeps working, and operators must set it true in
	// any deployment served over TLS.
	SessionCookieName string
	SessionSecure     bool
}

// MCPConfig drives the MCP Server, arborette's read-side interface for
// downstream agents. Port is the streamable-HTTP listen port; OrchestratorURL
// is the compose hostname of the Orchestrator the submit_analyst_goal tool
// proxies goal registration to (the MCP server never writes state directly).
// AuthorizationToken is the bearer token the server requires (a secret with no
// default; empty disables auth). PublicURL is the publicly reachable address of
// this server -- deliberately distinct from OrchestratorURL-style in-network
// hostnames, because the party that dials it is Anthropic's infrastructure, not
// another compose service.
type MCPConfig struct {
	Port               string
	OrchestratorURL    string
	AuthorizationToken string
	PublicURL          string
}

// SleepCycleConfig drives the Sleep-Cycle Worker: the two services it calls plus
// the lattice search's tuning knobs. MaxMeasurements is a safety bound on
// distinct sandbox measurements per run (the beam is finite by construction, so
// this only caps a pathologically wide harvest); BeamWidth is how many nodes
// survive each level; MaxOrder is the largest conjunction the search will form;
// MinSupport is an absolute matched-row floor below which a candidate and every
// superset of it are pruned; MinLift is the relative improvement over the best
// single segment a macro-segment must clear to be written back. MaxPublications
// caps how many segments one run abstracts into Meta-Heuristics — it tunes the
// publication stage rather than the search, which is why its env name carries no
// SEARCH segment.
//
// InternalAuthToken is the shared secret this worker presents on its audit
// writes, and it must be the same value the Orchestrator verifies -- which is why
// both sides read one variable rather than two that have to be kept in step. On a
// mismatch the Orchestrator answers 401 and the worker's audit path logs and
// swallows every failure, so the run reports success while its whole audit trail
// (including the terminal record carrying the run's winners) silently vanishes.
//
// Port is the serve-mode listen port -- serve mode is the local/long-running
// alternative to the one-shot Batch job, driven by the Orchestrator's HTTPLauncher.
//
// Policy selects the traversal strategy: the level-wise beam, or the
// knowledge-guided tree search that consults the accumulated Meta-Heuristic corpus.
// It is a stage-level choice rather than a search knob, which is why its env name
// carries no SEARCH segment. It defaults to the tree search on the strength of the
// calibration regression, which measures both over a dataset with a known planted
// structure; set it back to "beam" to revert without a deploy. What the remaining
// knobs mean is documented on sleepcycle.Config, which consumes and validates them.
type SleepCycleConfig struct {
	Port              string
	SandboxURL        string
	OrchestratorURL   string
	InternalAuthToken string
	MaxMeasurements   int
	BeamWidth         int
	MaxOrder          int
	MinSupport        int
	MinLift           float64
	MaxPublications   int

	Policy                string
	SchemaAtoms           bool
	UCTExploration        float64
	CausalMultiplierScale float64
	GroundingFraction     float64
	RetrievalK            int
	QuantileBins          int
	CrossGoalGrounding    bool
}

// VerifierConfig drives the Verifier (Engine B, stage 1: causal discovery). Port is
// the serve-mode listen port (8085; 8080-8084 are taken by the other services).
// SandboxURL is the Sandbox Execution service the discovery sweep runs its
// conditional-independence tests through; OrchestratorURL is for the audit client
// only — goal data comes straight from Postgres via the goal registry.
// InternalAuthToken is the shared INTERNAL_AUTH_TOKEN.
//
// The discovery knobs mirror the settled tuning: DiscoveryAlpha is the CI-test
// significance threshold; DiscoveryFDR selects the multiple-testing correction (bh
// or none); DiscoveryMaxCondSet bounds the conditioning-set size; DiscoveryBins is
// the quantile bin count for binned columns; DiscoveryColumnCap caps the sweep's
// variable count; DiscoveryMaxTests bounds the total sandbox round-trips (a wide
// dataset at conditioning bound 2 can reach tens of thousands otherwise);
// DiscoveryCallTimeout bounds one analyze call; OrientMaxRepairs bounds the batched
// LLM orientation repair loop.
//
// The verification knobs drive the adjustment + refutation stage: LeaseTTL is a
// leased record's deadline and HeartbeatEvery paces its renewal (lease = 3×
// heartbeat, so two missed beats precede a false reap); InflightCap bounds concurrent
// verifications per goal; VerificationBudget is the per-goal budget default when the
// registry carries no override; RefutationK is the subsample count; RefutationTau is
// the score threshold for causally_verified; SampleFraction is each subsample's
// share; StabilityBand is the relative band a subsample effect must stay within;
// SupportFloor is the per-stratum positivity floor; CollapseRatio is the naive-effect
// fraction the adjusted effect must retain to avoid the confounded verdict;
// RandomStratifierBins is the synthetic random-confounder bucket count. The
// refutation and effect knobs are calibration targets pending tuning against the
// ground-truth fixture.
type VerifierConfig struct {
	Port                 string
	SandboxURL           string
	OrchestratorURL      string
	InternalAuthToken    string
	DiscoveryAlpha       float64
	DiscoveryFDR         string
	DiscoveryMaxCondSet  int
	DiscoveryBins        int
	DiscoveryColumnCap   int
	DiscoveryMaxTests    int
	DiscoveryCallTimeout time.Duration
	OrientMaxRepairs     int

	LeaseTTL             time.Duration
	HeartbeatEvery       time.Duration
	InflightCap          int
	VerificationBudget   int
	RefutationK          int
	RefutationTau        float64
	SampleFraction       float64
	StabilityBand        float64
	SupportFloor         int
	CollapseRatio        float64
	RandomStratifierBins int
}

// LLMConfig selects the LLM provider and holds each provider's credentials.
// Provider is one of "anthropic", "deepinfra", or "ollama".
type LLMConfig struct {
	Provider  string
	Anthropic AnthropicConfig
	DeepInfra DeepInfraConfig
	Ollama    OllamaLLMConfig
}

// AnthropicConfig points the Claude clients at their models and key. Model is
// the structured-output model backing the analytical calls, overridable so a
// cheaper/faster model can back demos without a code change; ChatModel backs the
// interactive agent-preview surface, where latency matters more than analytical
// depth, so the two are tuned separately. APIKey is a secret with no default.
type AnthropicConfig struct {
	APIKey    string
	Model     string
	ChatModel string
}

// DeepInfraConfig points the DeepInfra client at a model and key. BaseURL is
// optional and defaults to https://api.deepinfra.com/v1/openai.
type DeepInfraConfig struct {
	APIKey  string
	Model   string
	BaseURL string
}

// OllamaLLMConfig points the Ollama LLM client at a local model. Endpoint is
// the Ollama server URL; Model is the model name to use.
type OllamaLLMConfig struct {
	Endpoint string
	Model    string
}

// dsn assembles a libpq/pgx keyword DSN for one role against the shared host.
// SSLMode defaults to disable for local docker-compose but is overridable via
// POSTGRES_SSLMODE (e.g. require/verify-full) so non-local deployments can
// encrypt and verify the connection. Every value is single-quoted and escaped
// so a secret containing whitespace, a quote, or a backslash cannot corrupt the
// keyword/value string (which would otherwise parse as stray options).
func (p PostgresConfig) dsn(user, password string) string {
	return fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s sslmode=%s",
		quoteDSNValue(p.Host), quoteDSNValue(p.Port), quoteDSNValue(p.DB),
		quoteDSNValue(user), quoteDSNValue(password), quoteDSNValue(p.SSLMode),
	)
}

// quoteDSNValue single-quotes a libpq keyword/value entry, escaping backslashes
// and single quotes per libpq rules.
func quoteDSNValue(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// OwnerDSN authenticates as the table owner. Used only by the bootstrap and
// migrate jobs (and integration tests), never by a runtime service.
func (p PostgresConfig) OwnerDSN() string {
	return p.dsn(p.OwnerUser, p.OwnerPassword)
}

// OrchestratorDSN authenticates as arborette_orchestrator (goal registry,
// audit inserts, embedding reads).
func (p PostgresConfig) OrchestratorDSN() string {
	return p.dsn(p.OrchestratorUser, p.OrchestratorPassword)
}

// ServiceDSN authenticates as arborette_service (embedding upsert/read, goal
// registry reads); it has no access to the audit table.
func (p PostgresConfig) ServiceDSN() string {
	return p.dsn(p.ServiceUser, p.ServicePassword)
}

// Load reads the full configuration from the environment, applying defaults for
// local docker-compose. Secrets have no defaults: an unset password/key loads
// as empty, and the consuming client fails at connect time. Load does not
// validate them here because it is shared across services with different
// credential needs (e.g. only the bootstrap/migrate jobs use the owner password;
// runtime services never do), so a required-for-everyone check would reject valid
// per-service configs; the error return is reserved for future structural
// validation.
func Load() (Config, error) {
	cfg := Config{
		Neo4j: Neo4jConfig{
			URI:      env("NEO4J_URI", "bolt://neo4j:7687"),
			User:     env("NEO4J_USER", "neo4j"),
			Password: os.Getenv("NEO4J_PASSWORD"),
		},
		Postgres: PostgresConfig{
			Host:                 env("POSTGRES_HOST", "postgres"),
			Port:                 env("POSTGRES_PORT", "5432"),
			DB:                   env("POSTGRES_DB", "arborette"),
			SSLMode:              env("POSTGRES_SSLMODE", "disable"),
			OwnerUser:            env("POSTGRES_OWNER_USER", "arborette_owner"),
			OwnerPassword:        os.Getenv("POSTGRES_OWNER_PASSWORD"),
			OrchestratorUser:     env("POSTGRES_ORCHESTRATOR_USER", "arborette_orchestrator"),
			OrchestratorPassword: os.Getenv("POSTGRES_ORCHESTRATOR_PASSWORD"),
			ServiceUser:          env("POSTGRES_SERVICE_USER", "arborette_service"),
			ServicePassword:      os.Getenv("POSTGRES_SERVICE_PASSWORD"),
		},
		S3: S3Config{
			Endpoint:  os.Getenv("S3_ENDPOINT"),
			Region:    env("S3_REGION", "us-east-1"),
			Bucket:    env("S3_BUCKET", "arborette"),
			AccessKey: os.Getenv("S3_ACCESS_KEY"),
			SecretKey: os.Getenv("S3_SECRET_KEY"),
			PathStyle: boolEnv("S3_PATH_STYLE", true),
		},
		Ollama: OllamaConfig{
			URL:   env("OLLAMA_URL", "http://ollama:11434"),
			Model: env("OLLAMA_MODEL", "nomic-embed-text"),
		},
		Embedding: EmbeddingConfig{
			Dimension:     intEnv("EMBEDDING_DIMENSION", 768),
			DistanceFloor: floatEnv("EMBEDDING_DISTANCE_FLOOR", 0.5),
		},
		Sandbox: SandboxConfig{
			Port:               env("SANDBOX_PORT", "8081"),
			MaxObjectBytes:     int64Env("SANDBOX_MAX_OBJECT_BYTES", 512<<20),
			MaxTempDirSize:     env("SANDBOX_MAX_TEMP_DIR_SIZE", "2GiB"),
			StageCacheDir:      os.Getenv("SANDBOX_STAGE_CACHE_DIR"),
			StageCacheMaxBytes: int64Env("SANDBOX_STAGE_CACHE_MAX_BYTES", 2<<30),
			ExecuteConcurrency: intEnv("SANDBOX_EXECUTE_CONCURRENCY", 8),
			AnalyzeConcurrency: intEnv("SANDBOX_ANALYZE_CONCURRENCY", 8),
			AnalyzeMaxColumns:  intEnv("SANDBOX_ANALYZE_MAX_COLUMNS", 4),
			AnalyzeMaxBins:     intEnv("SANDBOX_ANALYZE_MAX_BINS", 32),
			MaxBodyBytes:       int64Env("SANDBOX_MAX_BODY_BYTES", 1<<20),
			DistinctValueCap:   intEnv("SANDBOX_DISTINCT_VALUE_CAP", 50),
			InternalAuthToken:  os.Getenv("INTERNAL_AUTH_TOKEN"),
		},
		Orchestrator: OrchestratorConfig{
			Port:                    env("ORCHESTRATOR_PORT", "8080"),
			SandboxURL:              env("SANDBOX_URL", "http://sandbox:8081"),
			SleepCycleWorkerURL:     os.Getenv("SLEEPCYCLE_WORKER_URL"),
			VerifierWorkerURL:       os.Getenv("VERIFIER_WORKER_URL"),
			AnalystID:               env("ARBORETTE_ANALYST_ID", "analyst-stub"),
			SleepCycleJobName:       env("SLEEPCYCLE_JOB_NAME", "arborette-sleepcycle"),
			LocalImportDir:          env("ARBORETTE_LOCAL_IMPORT_DIR", "/import"),
			InternalAuthToken:       os.Getenv("INTERNAL_AUTH_TOKEN"),
			HITLConfidenceThreshold: floatEnv("HITL_CONFIDENCE_THRESHOLD", 0.8),
			BlockingLoopTimeout:     time.Duration(intEnv("HITL_BLOCKING_LOOP_TIMEOUT_MINUTES", 1440)) * time.Minute,
			AutoPromoteEnabled:      boolEnv("ORCHESTRATOR_AUTOPROMOTE_ENABLED", true),
			AutoPromoteTopN:         intEnv("ORCHESTRATOR_AUTOPROMOTE_TOP_N", 5),
			AutoPromoteShrinkageK:   intEnv("ORCHESTRATOR_AUTOPROMOTE_SHRINKAGE_K", 30),
			StaleReverifyCap:        intEnv("ORCHESTRATOR_STALE_REVERIFY_CAP", 10),
			SessionCookieName:       env("ARBORETTE_SESSION_COOKIE", "arborette_session"),
			SessionSecure:           boolEnv("ARBORETTE_SESSION_SECURE", false),
		},
		MCP: MCPConfig{
			Port:               env("MCP_PORT", "8082"),
			OrchestratorURL:    env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
			AuthorizationToken: os.Getenv("MCP_AUTHORIZATION_TOKEN"),
			PublicURL:          os.Getenv("MCP_PUBLIC_URL"),
		},
		SleepCycle: SleepCycleConfig{
			Port:              env("SLEEPCYCLE_PORT", "8084"),
			SandboxURL:        env("SANDBOX_URL", "http://sandbox:8081"),
			OrchestratorURL:   env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
			InternalAuthToken: os.Getenv("INTERNAL_AUTH_TOKEN"),
			MaxMeasurements:   intEnv("SLEEPCYCLE_SEARCH_MAX_MEASUREMENTS", 200),
			BeamWidth:         intEnv("SLEEPCYCLE_SEARCH_BEAM_WIDTH", 10),
			MaxOrder:          intEnv("SLEEPCYCLE_SEARCH_MAX_ORDER", 3),
			MinSupport:        intEnv("SLEEPCYCLE_SEARCH_MIN_SUPPORT", 30),
			MinLift:           floatEnv("SLEEPCYCLE_SEARCH_MIN_LIFT", 0.05),
			MaxPublications:   intEnv("SLEEPCYCLE_MAX_PUBLICATIONS", 20),

			Policy:                env("SLEEPCYCLE_POLICY", "uct"),
			SchemaAtoms:           boolEnv("SLEEPCYCLE_SEARCH_SCHEMA_ATOMS", false),
			UCTExploration:        floatEnv("SLEEPCYCLE_SEARCH_UCT_EXPLORATION", math.Sqrt2),
			CausalMultiplierScale: floatEnv("SLEEPCYCLE_SEARCH_CAUSAL_MULTIPLIER_SCALE", 1.0),
			GroundingFraction:     floatEnv("SLEEPCYCLE_SEARCH_GROUNDING_FRACTION", 0.3),
			RetrievalK:            intEnv("SLEEPCYCLE_SEARCH_RETRIEVAL_K", 8),
			QuantileBins:          intEnv("SLEEPCYCLE_SEARCH_QUANTILE_BINS", 3),
			CrossGoalGrounding:    boolEnv("SLEEPCYCLE_SEARCH_CROSS_GOAL_GROUNDING", true),
		},
		Verifier: VerifierConfig{
			Port:                 env("VERIFIER_PORT", "8085"),
			SandboxURL:           env("SANDBOX_URL", "http://sandbox:8081"),
			OrchestratorURL:      env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
			InternalAuthToken:    os.Getenv("INTERNAL_AUTH_TOKEN"),
			DiscoveryAlpha:       floatEnv("VERIFIER_DISCOVERY_ALPHA", 0.05),
			DiscoveryFDR:         env("VERIFIER_DISCOVERY_FDR", "bh"),
			DiscoveryMaxCondSet:  intEnv("VERIFIER_DISCOVERY_MAX_COND_SET", 2),
			DiscoveryBins:        intEnv("VERIFIER_DISCOVERY_BINS", 4),
			DiscoveryColumnCap:   intEnv("VERIFIER_DISCOVERY_COLUMN_CAP", 50),
			DiscoveryMaxTests:    intEnv("VERIFIER_DISCOVERY_MAX_TESTS", 20000),
			DiscoveryCallTimeout: time.Duration(intEnv("VERIFIER_DISCOVERY_CALL_TIMEOUT_SECONDS", 60)) * time.Second,
			OrientMaxRepairs:     intEnv("VERIFIER_ORIENT_MAX_REPAIRS", 2),
			LeaseTTL:             time.Duration(intEnv("VERIFIER_LEASE_TTL_SECONDS", 90)) * time.Second,
			HeartbeatEvery:       time.Duration(intEnv("VERIFIER_HEARTBEAT_EVERY_SECONDS", 30)) * time.Second,
			InflightCap:          intEnv("VERIFIER_INFLIGHT_CAP", 4),
			VerificationBudget:   intEnv("VERIFIER_VERIFICATION_BUDGET", 20),
			RefutationK:          intEnv("VERIFIER_REFUTATION_K", 20),
			RefutationTau:        floatEnv("VERIFIER_REFUTATION_TAU", 0.8),
			SampleFraction:       floatEnv("VERIFIER_SAMPLE_FRACTION", 0.7),
			StabilityBand:        floatEnv("VERIFIER_STABILITY_BAND", 0.5),
			SupportFloor:         intEnv("VERIFIER_SUPPORT_FLOOR", 20),
			CollapseRatio:        floatEnv("VERIFIER_COLLAPSE_RATIO", 0.5),
			RandomStratifierBins: intEnv("VERIFIER_RANDOM_STRATIFIER_BINS", 4),
		},
		LLM: LLMConfig{
			Provider: env("LLM_PROVIDER", "anthropic"),
			Anthropic: AnthropicConfig{
				APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
				Model:     env("ANTHROPIC_MODEL", "claude-opus-4-8"),
				ChatModel: env("ANTHROPIC_CHAT_MODEL", "claude-sonnet-5"),
			},
			DeepInfra: DeepInfraConfig{
				APIKey:  os.Getenv("DEEP_INFRA_API_KEY"),
				Model:   env("DEEP_INFRA_MODEL", "meta-llama/Llama-3.3-70B-Instruct"),
				BaseURL: env("DEEP_INFRA_BASE_URL", "https://api.deepinfra.com/v1/openai"),
			},
			Ollama: OllamaLLMConfig{
				Endpoint: env("OLLAMA_LLM_ENDPOINT", "http://localhost:11434"),
				Model:    env("OLLAMA_LLM_MODEL", "llama3"),
			},
		},
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func int64Env(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func floatEnv(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func boolEnv(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
