package usql

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/bsv-blockchain/teranode/errors"
)

// SchemaQuerier is the minimal surface VerifySchemaColumns needs. Both *usql.DB
// and the store packages' DBExecutor interfaces satisfy it.
type SchemaQuerier interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// VerifySchemaColumns checks that every column listed in expected is present
// on the corresponding table in the database's public schema. Designed to run
// post-init on PostgreSQL-like engines; catches drift where a future migration
// added a column to one engine's path but not the other.
//
// Returns a single error per call listing every (table, missing-columns)
// finding so operators see the full delta instead of a leaky one-at-a-time
// view. Iteration over the expected map is sorted by table name so error
// messages are deterministic across runs.
func VerifySchemaColumns(ctx context.Context, db SchemaQuerier, engine Engine, expected map[string][]string) error {
	tables := make([]string, 0, len(expected))
	for table := range expected {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	var findings []string
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `
			SELECT column_name
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1
		`, table)
		if err != nil {
			return errors.NewStorageError("verify %s schema (%s)", table, engine, err)
		}
		present := map[string]struct{}{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				_ = rows.Close()
				return errors.NewStorageError("verify %s schema (%s) scan", table, engine, err)
			}
			present[c] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return errors.NewStorageError("verify %s schema (%s) iter", table, engine, err)
		}
		_ = rows.Close()

		var missing []string
		for _, col := range expected[table] {
			if _, ok := present[col]; !ok {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			findings = append(findings, table+": "+strings.Join(missing, ","))
		}
	}
	if len(findings) > 0 {
		return errors.NewStorageError("missing columns on %s: %s", engine, strings.Join(findings, "; "))
	}
	return nil
}

// Engine identifies the SQL engine behind a *usql.DB connection.
// PostgreSQL and CockroachDB both speak the Postgres wire protocol but
// diverge on a few features (advisory locks, plpgsql DO blocks); call
// sites use IsPostgresLike for shared fast paths and explicit equality
// checks for engine-specific branches.
type Engine string

const (
	EnginePostgres     Engine = "postgres"
	EngineCockroach    Engine = "cockroach"
	EngineSqlite       Engine = "sqlite"
	EngineSqliteMemory Engine = "sqlitememory"
)

// IsPostgresLike reports whether the engine speaks the Postgres wire
// protocol and accepts the PG-compatible SQL subset used by Teranode
// (batching, CTEs, UNNEST, partial indexes, ON CONFLICT).
func IsPostgresLike(e Engine) bool {
	return e == EnginePostgres || e == EngineCockroach
}

// DetectEngine probes the connected database and returns its engine identity.
// Safe to call once per *DB at init; the result is stable for the connection's
// lifetime. Runs a single SELECT version() query against an arbitrary connection
// from the pool — version() is a session-independent constant.
//
// Returns an error if the version string matches neither known engine. Loud
// failure is preferred over silently falling back to a wrong engine assumption.
func DetectEngine(ctx context.Context, db *DB) (Engine, error) {
	var v string
	if err := db.DB.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return "", errors.New(errors.ERR_ERROR, "detect engine", err)
	}
	switch {
	case strings.Contains(v, "CockroachDB"):
		return EngineCockroach, nil
	case strings.Contains(v, "PostgreSQL"):
		return EnginePostgres, nil
	default:
		return "", errors.New(errors.ERR_ERROR, "unrecognized engine: %q", v)
	}
}
