package pgtest

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier runs a query; *pgx.Conn, *pgxpool.Pool and pgx.Tx implement it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// schemaFilter selects the namespaces a fingerprint covers; $1 is the
// caller's excluded schema names. %s is the namespace column.
const schemaFilter = `%[1]s NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
	AND %[1]s NOT LIKE 'pg\_temp\_%%' AND %[1]s NOT LIKE 'pg\_toast\_temp\_%%'
	AND NOT (%[1]s = ANY ($1::text[]))`

// fingerprintQueries each return one text line per catalog object. None
// prints an OID or a database name.
var fingerprintQueries = []string{
	// Schemas.
	`SELECT 'schema ' || n.nspname
	 FROM pg_namespace n WHERE ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Relations: tables, views, materialized views, sequences, foreign and
	// partitioned tables, composite types.
	`SELECT 'relation ' || n.nspname || '.' || c.relname || ' kind=' || c.relkind::text
	        || ' persistence=' || c.relpersistence::text
	 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f', 'c') AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Columns.
	`SELECT 'column ' || n.nspname || '.' || c.relname || '.' || a.attname
	        || ' #' || (row_number() OVER (PARTITION BY a.attrelid ORDER BY a.attnum))::text
	        || ' type=' || format_type(a.atttypid, a.atttypmod)
	        || ' notnull=' || a.attnotnull::text
	        || ' default=' || coalesce(pg_get_expr(d.adbin, d.adrelid), '')
	        || ' generated=' || a.attgenerated::text
	        || ' identity=' || a.attidentity::text
	 FROM pg_attribute a
	 JOIN pg_class c ON c.oid = a.attrelid
	 JOIN pg_namespace n ON n.oid = c.relnamespace
	 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	 WHERE a.attnum > 0 AND NOT a.attisdropped
	   AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'c') AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Constraints.
	`SELECT 'constraint ' || n.nspname || '.' || coalesce(c.relname, t.typname, '') || '.' || k.conname
	        || ' type=' || k.contype::text
	        || ' def=' || pg_get_constraintdef(k.oid)
	        || ' deferrable=' || k.condeferrable::text || ' deferred=' || k.condeferred::text
	 FROM pg_constraint k
	 JOIN pg_namespace n ON n.oid = k.connamespace
	 LEFT JOIN pg_class c ON c.oid = k.conrelid
	 LEFT JOIN pg_type t ON t.oid = k.contypid
	 WHERE ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Indexes.
	`SELECT 'index ' || pg_get_indexdef(i.indexrelid)
	 FROM pg_index i
	 JOIN pg_class c ON c.oid = i.indexrelid
	 JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Non-internal triggers.
	`SELECT 'trigger ' || pg_get_triggerdef(g.oid) || ' enabled=' || g.tgenabled::text
	 FROM pg_trigger g
	 JOIN pg_class c ON c.oid = g.tgrelid
	 JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE NOT g.tgisinternal AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Functions and procedures.
	`SELECT 'function ' || n.nspname || '.' || p.proname
	        || '(' || pg_get_function_identity_arguments(p.oid) || ')'
	        || ' md5=' || md5(pg_get_functiondef(p.oid))
	 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	 WHERE p.prokind IN ('f', 'p') AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Views.
	`SELECT 'view ' || n.nspname || '.' || c.relname || ' md5=' || md5(pg_get_viewdef(c.oid))
	 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE c.relkind IN ('v', 'm') AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// User types (row types of relations included).
	`SELECT 'type ' || n.nspname || '.' || t.typname || ' kind=' || t.typtype::text
	 FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
	 WHERE ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	// Comments on schemas, relations, columns and functions.
	`SELECT 'comment schema ' || n.nspname || ' ' || d.description
	 FROM pg_description d JOIN pg_namespace n ON n.oid = d.objoid
	 WHERE d.classoid = 'pg_namespace'::regclass AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	`SELECT 'comment relation ' || n.nspname || '.' || c.relname
	        || CASE WHEN d.objsubid = 0 THEN '' ELSE '.' || a.attname END || ' ' || d.description
	 FROM pg_description d
	 JOIN pg_class c ON c.oid = d.objoid
	 JOIN pg_namespace n ON n.oid = c.relnamespace
	 LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = d.objsubid
	 WHERE d.classoid = 'pg_class'::regclass AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
	`SELECT 'comment function ' || n.nspname || '.' || p.proname
	        || '(' || pg_get_function_identity_arguments(p.oid) || ') ' || d.description
	 FROM pg_description d
	 JOIN pg_proc p ON p.oid = d.objoid
	 JOIN pg_namespace n ON n.oid = p.pronamespace
	 WHERE d.classoid = 'pg_proc'::regclass AND ` + fmt.Sprintf(schemaFilter, "n.nspname"),
}

// Fingerprint returns a sorted, line-per-object description of every schema,
// relation, column (position among live columns, type, nullability,
// default, generated, identity), constraint (pg_get_constraintdef,
// deferrability), index (pg_get_indexdef), non-internal trigger
// (pg_get_triggerdef), function (md5 of pg_get_functiondef), view (md5 of
// pg_get_viewdef), user type and comment outside pg_catalog,
// information_schema, pg_toast, the temporary schemas and the excluded
// schemas. Two catalogs with equal fingerprints hold the same schema
// objects.
func Fingerprint(ctx context.Context, db Querier, exclude ...string) (string, error) {
	if exclude == nil {
		exclude = []string{}
	}
	var lines []string
	for _, q := range fingerprintQueries {
		rows, err := db.Query(ctx, q, exclude)
		if err != nil {
			return "", fmt.Errorf("pgtest: fingerprint: %w", err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return "", fmt.Errorf("pgtest: fingerprint: %w", err)
		}
		lines = append(lines, got...)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}
