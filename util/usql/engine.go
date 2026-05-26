package usql

import (
	"context"
	"fmt"
	"strings"
)

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
		return "", fmt.Errorf("detect engine: %w", err)
	}
	switch {
	case strings.Contains(v, "CockroachDB"):
		return EngineCockroach, nil
	case strings.Contains(v, "PostgreSQL"):
		return EnginePostgres, nil
	default:
		return "", fmt.Errorf("unrecognized engine: %q", v)
	}
}
