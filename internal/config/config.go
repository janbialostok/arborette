// Package config loads arborette's shared, env-based configuration. Every
// cmd/<service> main constructs a Config and passes the pieces it needs to the
// internal packages. Role passwords come from the environment and are never
// committed; see .env.example for the full variable list.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	Anthropic    AnthropicConfig
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

// EmbeddingConfig fixes the vector dimension. It is load-bearing: it must match
// both the pgvector column and the model's output width. Swapping providers is
// a migration + full re-embed, not a runtime toggle.
type EmbeddingConfig struct {
	Dimension int
}

// SandboxConfig drives the stateless Sandbox Execution HTTP service. MaxObjectBytes
// caps how much of a data source the sandbox will stage locally before rejecting
// it, bounding disk use; MaxTempDirSize is a DuckDB size string (e.g. 2GiB, with a
// unit -- never a bare byte count) passed verbatim into SET max_temp_directory_size
// to bound query-spill blast radius.
type SandboxConfig struct {
	Port           string
	MaxObjectBytes int64
	MaxTempDirSize string
}

// OrchestratorConfig drives the REST/API service. SandboxURL is the compose
// hostname of the Sandbox Execution service the goal-intake and hypothesis loop
// call; AnalystID backs the stub identity every audit record is stamped with;
// SleepCycleJobName is the named job the Phase-2 trigger launches; LocalImportDir
// is the read-only mount the on-disk ingestion path resolves relative paths
// against (a path escaping it is rejected).
type OrchestratorConfig struct {
	Port              string
	SandboxURL        string
	AnalystID         string
	SleepCycleJobName string
	LocalImportDir    string
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
type SleepCycleConfig struct {
	SandboxURL      string
	OrchestratorURL string
	MaxMeasurements int
	BeamWidth       int
	MaxOrder        int
	MinSupport      int
	MinLift         float64
	MaxPublications int
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
// credential needs (e.g. the sandbox uses no Postgres role), so a required-for-
// everyone check would reject valid per-service configs; the error return is
// reserved for future structural validation.
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
			Dimension: intEnv("EMBEDDING_DIMENSION", 768),
		},
		Sandbox: SandboxConfig{
			Port:           env("SANDBOX_PORT", "8081"),
			MaxObjectBytes: int64Env("SANDBOX_MAX_OBJECT_BYTES", 512<<20),
			MaxTempDirSize: env("SANDBOX_MAX_TEMP_DIR_SIZE", "2GiB"),
		},
		Orchestrator: OrchestratorConfig{
			Port:              env("ORCHESTRATOR_PORT", "8080"),
			SandboxURL:        env("SANDBOX_URL", "http://sandbox:8081"),
			AnalystID:         env("ARBORETTE_ANALYST_ID", "analyst-stub"),
			SleepCycleJobName: env("SLEEPCYCLE_JOB_NAME", "arborette-sleepcycle"),
			LocalImportDir:    env("ARBORETTE_LOCAL_IMPORT_DIR", "/import"),
		},
		MCP: MCPConfig{
			Port:               env("MCP_PORT", "8082"),
			OrchestratorURL:    env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
			AuthorizationToken: os.Getenv("MCP_AUTHORIZATION_TOKEN"),
			PublicURL:          os.Getenv("MCP_PUBLIC_URL"),
		},
		SleepCycle: SleepCycleConfig{
			SandboxURL:      env("SANDBOX_URL", "http://sandbox:8081"),
			OrchestratorURL: env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
			MaxMeasurements: intEnv("SLEEPCYCLE_SEARCH_MAX_MEASUREMENTS", 200),
			BeamWidth:       intEnv("SLEEPCYCLE_SEARCH_BEAM_WIDTH", 10),
			MaxOrder:        intEnv("SLEEPCYCLE_SEARCH_MAX_ORDER", 3),
			MinSupport:      intEnv("SLEEPCYCLE_SEARCH_MIN_SUPPORT", 30),
			MinLift:         floatEnv("SLEEPCYCLE_SEARCH_MIN_LIFT", 0.05),
			MaxPublications: intEnv("SLEEPCYCLE_MAX_PUBLICATIONS", 20),
		},
		Anthropic: AnthropicConfig{
			APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
			Model:     env("ANTHROPIC_MODEL", "claude-opus-4-8"),
			ChatModel: env("ANTHROPIC_CHAT_MODEL", "claude-sonnet-5"),
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
