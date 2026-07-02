package sql

// Tests for the SetConflicting cascade bug live in:
//   - stores/utxo/setconflicting_cascade_bug_test.go (mock-based, proves the cascade)
//   - stores/utxo/aerospike/setconflicting_cascade_test.go (Aerospike TestContainer)
//
// SQLite-backed coverage is possible since the Phase-1 read hoist in SetConflicting
// (sql.go ~4280): all s.Get/s.GetSpend reads now run on the pool BEFORE s.db.Begin()
// opens the write transaction, so the historical shared-cache deadlock (reads
// interleaved inside an open write txn on a single-connection pool) is gone.
// TestProcessConflicting_SelfHealsDanglingLoserSlot in
// counter_conflicting_dangling_test.go exercises SetConflicting end-to-end on SQLite.
