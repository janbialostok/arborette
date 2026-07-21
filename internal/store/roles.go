package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RoleSpec is a runtime LOGIN role to provision with a configured password.
type RoleSpec struct {
	Name     string
	Password string
}

// EnsureRoles idempotently provisions runtime LOGIN roles using the owner DSN.
// Roles are provisioned OUTSIDE migrations (golang-migrate static SQL cannot
// interpolate env passwords, and CREATE ROLE defaults to NOLOGIN) so both the
// compose db-bootstrap job and integration tests share this one path, and so
// 0005_grants has roles to grant against. Role names and passwords are escaped
// server-side via format() %I/%L; the password is synced on every run.
func EnsureRoles(ctx context.Context, ownerDSN string, roles []RoleSpec) error {
	conn, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		return fmt.Errorf("connect as owner: %w", err)
	}
	defer conn.Close(ctx)

	for _, role := range roles {
		var exists bool
		if err := conn.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", role.Name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check role %q: %w", role.Name, err)
		}

		verb := "CREATE ROLE"
		if exists {
			verb = "ALTER ROLE"
		}
		var stmt string
		if err := conn.QueryRow(ctx,
			"SELECT format('"+verb+" %I WITH LOGIN PASSWORD %L', $1::text, $2::text)",
			role.Name, role.Password,
		).Scan(&stmt); err != nil {
			return fmt.Errorf("build role statement for %q: %w", role.Name, err)
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision role %q: %w", role.Name, err)
		}
	}
	return nil
}
