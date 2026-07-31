package config_test

import (
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/config"
)

func setPostgresEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_HOST", "h")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_DB", "db")
	t.Setenv("POSTGRES_OWNER_USER", "owner")
	t.Setenv("POSTGRES_OWNER_PASSWORD", "ownerpw")
	t.Setenv("POSTGRES_ORCHESTRATOR_USER", "orch")
	t.Setenv("POSTGRES_ORCHESTRATOR_PASSWORD", "orchpw")
	t.Setenv("POSTGRES_SERVICE_USER", "svc")
	t.Setenv("POSTGRES_SERVICE_PASSWORD", "svcpw")
}

// TestDSNRoleSeparation guards the credential separation the DB-enforced audit
// boundary depends on: each DSN must carry its own role's user and password and
// no other role's.
func TestDSNRoleSeparation(t *testing.T) {
	setPostgresEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		name, dsn, user, pass string
	}{
		{"owner", cfg.Postgres.OwnerDSN(), "owner", "ownerpw"},
		{"orchestrator", cfg.Postgres.OrchestratorDSN(), "orch", "orchpw"},
		{"service", cfg.Postgres.ServiceDSN(), "svc", "svcpw"},
	}
	for _, c := range cases {
		if !strings.Contains(c.dsn, "user='"+c.user+"'") || !strings.Contains(c.dsn, "password='"+c.pass+"'") {
			t.Fatalf("%s DSN missing its own credentials: %q", c.name, c.dsn)
		}
		for _, other := range cases {
			if other.user == c.user {
				continue
			}
			if strings.Contains(c.dsn, "user='"+other.user+"'") || strings.Contains(c.dsn, "password='"+other.pass+"'") {
				t.Fatalf("%s DSN leaked %s credentials: %q", c.name, other.name, c.dsn)
			}
		}
	}
}

// TestDSNValueQuoting verifies a password with a space and a single quote does
// not corrupt the keyword/value DSN.
func TestDSNValueQuoting(t *testing.T) {
	setPostgresEnv(t)
	t.Setenv("POSTGRES_OWNER_PASSWORD", `pa ss'w\rd`)
	cfg, _ := config.Load()
	dsn := cfg.Postgres.OwnerDSN()
	if !strings.Contains(dsn, `password='pa ss\'w\\rd'`) {
		t.Fatalf("password not correctly quoted/escaped in DSN: %q", dsn)
	}
}

func TestSSLModeDefaultAndOverride(t *testing.T) {
	setPostgresEnv(t)
	cfg, _ := config.Load()
	if !strings.Contains(cfg.Postgres.OwnerDSN(), "sslmode='disable'") {
		t.Fatalf("expected default sslmode=disable, got %q", cfg.Postgres.OwnerDSN())
	}
	t.Setenv("POSTGRES_SSLMODE", "require")
	cfg, _ = config.Load()
	if !strings.Contains(cfg.Postgres.OwnerDSN(), "sslmode='require'") {
		t.Fatalf("expected sslmode=require override, got %q", cfg.Postgres.OwnerDSN())
	}
}

func TestEmbeddingDimensionFallback(t *testing.T) {
	t.Setenv("EMBEDDING_DIMENSION", "not-a-number")
	cfg, _ := config.Load()
	if cfg.Embedding.Dimension != 768 {
		t.Fatalf("expected fallback to 768 on unparseable value, got %d", cfg.Embedding.Dimension)
	}
	t.Setenv("EMBEDDING_DIMENSION", "1024")
	cfg, _ = config.Load()
	if cfg.Embedding.Dimension != 1024 {
		t.Fatalf("expected 1024, got %d", cfg.Embedding.Dimension)
	}
}

func TestPathStyleBoolFallback(t *testing.T) {
	t.Setenv("S3_PATH_STYLE", "garbage")
	cfg, _ := config.Load()
	if !cfg.S3.PathStyle {
		t.Fatalf("expected fallback to true on unparseable bool")
	}
	t.Setenv("S3_PATH_STYLE", "false")
	cfg, _ = config.Load()
	if cfg.S3.PathStyle {
		t.Fatalf("expected false override")
	}
}

// TestMinLiftFallback drives the float knob through Load, so the env wiring is
// exercised alongside the parser. An unparseable value must fall back rather than
// zero the knob — a MinLift silently reset to 0 would let every measured
// conjunction clear the materially-better gate.
func TestMinLiftFallback(t *testing.T) {
	setPostgresEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MinLift != 0.05 {
		t.Fatalf("unset = %v, want the 0.05 default", cfg.SleepCycle.MinLift)
	}

	t.Setenv("SLEEPCYCLE_SEARCH_MIN_LIFT", "0.25")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MinLift != 0.25 {
		t.Fatalf("parsed = %v, want 0.25", cfg.SleepCycle.MinLift)
	}

	t.Setenv("SLEEPCYCLE_SEARCH_MIN_LIFT", "not-a-number")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MinLift != 0.05 {
		t.Fatalf("unparseable = %v, want the 0.05 default", cfg.SleepCycle.MinLift)
	}
}

// TestMaxPublicationsFallback pins the publication cap's env wiring. Its name is
// the one knob in the sleep-cycle set that drops the SEARCH segment, so a typo
// would fall back to the default invisibly.
func TestMaxPublicationsFallback(t *testing.T) {
	setPostgresEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MaxPublications != 20 {
		t.Fatalf("unset = %v, want the 20 default", cfg.SleepCycle.MaxPublications)
	}

	t.Setenv("SLEEPCYCLE_MAX_PUBLICATIONS", "5")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MaxPublications != 5 {
		t.Fatalf("parsed = %v, want 5", cfg.SleepCycle.MaxPublications)
	}

	t.Setenv("SLEEPCYCLE_MAX_PUBLICATIONS", "not-a-number")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SleepCycle.MaxPublications != 20 {
		t.Fatalf("unparseable = %v, want the 20 default", cfg.SleepCycle.MaxPublications)
	}
}

// TestInternalAuthTokenIsOneSecret pins that the service verifying the internal
// shared secret and the job presenting it read the same variable, and that it
// takes no default. Giving either side a name of its own compiles and passes
// every other test, and the mismatch it produces is silent — see the field docs
// in config.go for what silently stops happening.
func TestInternalAuthTokenIsOneSecret(t *testing.T) {
	setPostgresEnv(t)

	// Cleared rather than assumed absent: the integration target sources .env,
	// where an operator who minted a token has one set.
	t.Setenv("INTERNAL_AUTH_TOKEN", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Orchestrator.InternalAuthToken != "" || cfg.SleepCycle.InternalAuthToken != "" {
		t.Fatalf("unset = %q/%q, want empty on both sides (a secret takes no default)",
			cfg.Orchestrator.InternalAuthToken, cfg.SleepCycle.InternalAuthToken)
	}

	t.Setenv("INTERNAL_AUTH_TOKEN", "s3cret")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Orchestrator.InternalAuthToken != "s3cret" || cfg.SleepCycle.InternalAuthToken != "s3cret" ||
		cfg.Sandbox.InternalAuthToken != "s3cret" {
		t.Fatalf("orchestrator = %q, sleepcycle = %q, sandbox = %q, want all three to read INTERNAL_AUTH_TOKEN",
			cfg.Orchestrator.InternalAuthToken, cfg.SleepCycle.InternalAuthToken, cfg.Sandbox.InternalAuthToken)
	}
}

// TestSandboxCacheAndLimitDefaults pins the staging-cache and boundary knobs' env
// wiring and defaults, so a typo in an env name would fall back invisibly.
func TestSandboxCacheAndLimitDefaults(t *testing.T) {
	setPostgresEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sandbox.StageCacheDir != "" {
		t.Fatalf("StageCacheDir unset = %q, want empty (boot-time temp dir)", cfg.Sandbox.StageCacheDir)
	}
	if cfg.Sandbox.StageCacheMaxBytes != 2<<30 {
		t.Fatalf("StageCacheMaxBytes = %d, want the 2GiB default", cfg.Sandbox.StageCacheMaxBytes)
	}
	if cfg.Sandbox.ExecuteConcurrency != 8 {
		t.Fatalf("ExecuteConcurrency = %d, want the 8 default", cfg.Sandbox.ExecuteConcurrency)
	}
	if cfg.Sandbox.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d, want the 1MiB default", cfg.Sandbox.MaxBodyBytes)
	}

	t.Setenv("SANDBOX_STAGE_CACHE_DIR", "/var/cache/arborette")
	t.Setenv("SANDBOX_STAGE_CACHE_MAX_BYTES", "0")
	t.Setenv("SANDBOX_EXECUTE_CONCURRENCY", "4")
	t.Setenv("SANDBOX_MAX_BODY_BYTES", "2048")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sandbox.StageCacheDir != "/var/cache/arborette" {
		t.Fatalf("StageCacheDir override = %q", cfg.Sandbox.StageCacheDir)
	}
	if cfg.Sandbox.StageCacheMaxBytes != 0 {
		t.Fatalf("StageCacheMaxBytes override = %d, want 0 (cache disabled)", cfg.Sandbox.StageCacheMaxBytes)
	}
	if cfg.Sandbox.ExecuteConcurrency != 4 {
		t.Fatalf("ExecuteConcurrency override = %d, want 4", cfg.Sandbox.ExecuteConcurrency)
	}
	if cfg.Sandbox.MaxBodyBytes != 2048 {
		t.Fatalf("MaxBodyBytes override = %d, want 2048", cfg.Sandbox.MaxBodyBytes)
	}
}
