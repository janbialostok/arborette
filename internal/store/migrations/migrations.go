// Package migrations embeds the SQL migration files so golang-migrate can apply
// them via an iofs source without the files being present on disk at runtime.
package migrations

import "embed"

// FS holds every .sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
