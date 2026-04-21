package sql

// Tests for the SetConflicting cascade bug live in:
//   - stores/utxo/process_conflicting_cascade_test.go (mock-based, proves the interface gap)
//   - stores/utxo/aerospike/setconflicting_cascade_test.go (Aerospike TestContainer)
//
// SQLite's single-writer model deadlocks when SetConflicting (which starts a
// write transaction) internally calls GetSpend (which needs a read connection
// from the pool). This is a test infrastructure limitation, not a code bug.
