# SQL Engine Support

Teranode's SQL-backed UTXO and blockchain stores work transparently against either PostgreSQL or CockroachDB. Engine identity is auto-detected at connection-init time; no configuration flag is required beyond the standard `postgres://` connection URL.

## Engine identity

`util/usql/engine.go` defines:

| Constant | Value |
|---|---|
| `usql.EnginePostgres` | `"postgres"` |
| `usql.EngineCockroach` | `"cockroach"` |
| `usql.EngineSqlite` | `"sqlite"` |
| `usql.EngineSqliteMemory` | `"sqlitememory"` |

`util.InitSQLDB` calls `usql.DetectEngine` after every `postgres://` connection. The function runs `SELECT version()` once and matches the result against the `CockroachDB` / `PostgreSQL` prefix. The detected engine is stashed on the `*usql.DB` wrapper and is reachable via `db.Engine()`.

`usql.IsPostgresLike(e)` reports whether the engine speaks the Postgres wire protocol and accepts the PG-compatible SQL subset Teranode uses (batching, CTEs, UNNEST, partial indexes, ON CONFLICT). Use it to gate fast paths that work on both PG and CRDB. Use explicit `e == EngineCockroach` or `e == EnginePostgres` for engine-specific branches (advisory locks, plpgsql DO blocks).

## Cockroach-specific behavior at init

When the detected engine is `EngineCockroach`, the SQL store does three things differently:

1. **Skips `pg_advisory_lock`.** Cockroach does not implement advisory locks. Fresh CRDB installs land in the target schema via `CREATE TABLE IF NOT EXISTS` without serialization; CRDB's transactional DDL keeps concurrent creates safe.
2. **Skips the `DO $$ ... END $$` plpgsql migration blocks.** Cockroach has partial plpgsql support and its `pg_constraint` / `pg_attribute` system catalogs differ from PostgreSQL's. These blocks are idempotent migrations for existing PG installs; on a fresh CRDB install they are no-ops anyway, because the bare `CREATE TABLE IF NOT EXISTS` definitions above each block already specify the desired final shape.
3. **Re-opens the connection pool with `SET serial_normalization = 'sql_sequence'`** as an `AfterConnect` hook. Cockroach defaults `BIGSERIAL` columns to `unique_rowid()`, producing 64-bit "snowflake" values that overflow Go's `uint32` fields downstream (e.g. `block.ID`). With `sql_sequence`, `BIGSERIAL` columns get sequence-backed `nextval()` defaults at `CREATE TABLE` time, matching PostgreSQL semantics.

## Schema verification

Both the UTXO and blockchain stores run a `verifyUTXOSchema` / `verifyBlockchainSchema` check after `createPostgresSchema` returns. The check queries `information_schema.columns` (standard, both engines) and asserts the expected post-init column set is present. The verification runs on **both** PostgreSQL and Cockroach so a future migration that drifts the bare `CREATE TABLE` definition (and forgets to mirror the `DO $$` block for Cockroach) fails loud at startup.

## Future migration contract

When adding a new column or constraint:

| Change | What to do |
|---|---|
| **New column** | Add to the `CREATE TABLE IF NOT EXISTS` definition AND add an `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` outside the engine gate (CRDB 22.1+ supports it). Update the `expected` map in the relevant `verifyXxxSchema`. |
| **New FK with `ON DELETE CASCADE`** | Add to the `CREATE TABLE` definition AND add a PG-only `DO $$` block inside `if engine == usql.EnginePostgres { ... }`. |
| **New table** | Add `CREATE TABLE IF NOT EXISTS` engine-agnostically. Add to the `expected` map. |

## Engine-specific compatibility notes

- **`BIGSERIAL` PKs are sequence-backed on Cockroach** when the connection has `serial_normalization = 'sql_sequence'` set (Teranode installs this hook automatically). Without it, `BIGSERIAL` produces large `unique_rowid()` values that overflow `uint32` callers.
- **Modifying CTEs require `RETURNING`** on Cockroach. `WITH ins AS (INSERT INTO ...)` without a `RETURNING` clause fails with SQLSTATE 0A000 "WITH clause does not return any columns". PostgreSQL accepts the same pattern. Add `RETURNING 1` (or another minimal expression) to every modifying CTE.
- **Recursive SQL UDFs are not supported on Cockroach v24.x.** The blockchain store's legacy `reverse_bytes` / `reverse_bytes_iter` functions (now unused, replaced by the `on_main_chain` partial index) are gated to `EnginePostgres` for this reason.

## License caveat

CockroachDB v23.2+ moved to the CockroachDB Community License (CCL) for core enterprise features. **Single-node deployments are under the Business Source License (BSL)** and impose no operational restrictions. Multi-node production deployments require the CCL — verify license terms before recommending Cockroach for any production path.

## Local development

The `teranode-quickstart` repository provides a `cockroach-utxo` compose profile that boots a single-node Cockroach alongside the Teranode services with `utxostore` pointed at it. See that repo for setup instructions.

## CI coverage

`.github/workflows/teranode_pr_tests.yaml` includes a `cockroach-tests` job that runs:

```
go test -tags cockroach -timeout 15m ./util/usql/... ./stores/utxo/sql/... ./stores/blockchain/sql/...
```

The build tag (`//go:build cockroach`) keeps the testcontainers-based suite out of the default test run. Local developers can opt in with the same `-tags cockroach` flag.

## Open caveats

- **Multi-node CRDB performance is untested.** The integration suite boots single-node containers only. The spec's `BIGSERIAL` hot-spotting risk on multi-node clusters remains an open item to address via hash-sharded indexes or alternate ID generation if performance work identifies it.
- **`stores/utxo/postgres/` is unaffected.** The high-performance Postgres store from upstream issue #684 uses `pgx CopyFrom` to staging tables and is not part of this engine-detection path. Its CRDB compatibility needs its own assessment.

## Implementation reference

- Engine detection helper: `util/usql/engine.go`
- Engine field on `*DB`: `util/usql/db.go`
- Pool init with engine wiring: `util/sql.go` (`InitPostgresDB`, `InitSQLiteDB`)
- UTXO schema init + verification: `stores/utxo/sql/sql.go` (`createPostgresSchema`, `verifyUTXOSchema`)
- Blockchain schema init + verification: `stores/blockchain/sql/sql.go` (`createPostgresSchema`, `verifyBlockchainSchema`)
- Testcontainer helper: `test/testcontainers/crdb/cockroach.go` (build-tagged)
