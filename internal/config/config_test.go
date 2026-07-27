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
