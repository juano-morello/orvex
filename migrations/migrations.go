// Package migrations embeds the Orvex PostgreSQL schema migrations (ADR-005,
// ADR-007; decisions P-3 and P-4 of
// docs/specs/17380a6e_postgresql-schema-migrations.md, LBT-10).
//
// Files are NNNN_name.up.sql and NNNN_name.down.sql, contiguous from 0001.
// The runner in internal/persistence/postgres applies one file per
// transaction and records SHA-256 checksums of both files. A merged
// migration is immutable: the checksums make an edited file fail closed, so
// every schema change goes into a new migration.
package migrations

import "embed"

// FS holds every *.sql file of this directory.
//
//go:embed *.sql
var FS embed.FS
