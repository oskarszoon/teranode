package usql

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
