# Implementation plan — unseen-tx throughput repro

Executes [the design](./1379-unseen-tx-throughput-design.md) for
[issue #1379](https://github.com/bsv-blockchain/teranode/issues/1379).

Each phase ends in a runnable verification. Phases 1–3 build the harness; phase 4
is the measurement that decides whether the rest of the plan is the bisect or the
real-block fallback.

## Phase 1 — Aerospike-backed server fixture

Stand up the wiring with a single transaction, proving every real component
connects before any generator work.

New file `services/subtreevalidation/check_block_subtrees_unseen_perf_test.go`,
build tag `aerospike`.

1. `InitAerospikeContainer()` → URL, deferred cleanup.
2. `aerospikestore.New` UTXO store against that URL. Confirms Lua UDF
   registration succeeded.
3. `test.CreateBaseTestSettings(t)`, mainnet chaincfg, fixture height 257727.
4. Real `validator.New(...)` with mock Kafka producers.
5. `blockchain.NewLocalClient` wrapped so `GetFSMCurrentState` reports **RUNNING**
   — reuse or mirror the `fsmStateOverrideClient` already in the package.
6. subtreevalidation `New(...)` server.
7. Reuse the mainnet parent/child fixture pair from
   `legacy_real_validator_integration_test.go:67-71`, seed the parent as mined,
   drive one tx through `CheckBlockSubtrees`.

**Verify:** `go test -tags aerospike -run TestUnseenTxThroughput_Smoke -v` passes,
the child is blessed, and the parent output is spent. Assert
`addTXToBlockAssembly` actually took effect by checking the child's meta went
through the locked → unlocked 2PC transition, not just that it exists — otherwise
the RUNNING pin is silently ineffective and the whole harness measures the wrong
path.

## Phase 2 — Parameterised fixture generator

Generate a block of unseen txs over seeded parents.

1. Config struct: `txCount`, `levelSizes []int`, `inputsPerTx`,
   `parentExtendedSize`, `subtreeSize`.
2. Coinbase-funded chain generation with real signatures. Reuse
   `generateChainToChannel` / `streamTransactionsToSubtrees` from
   `check_block_subtrees_large_test.go` where they fit; extract rather than
   duplicate if they need to be shared.
3. Level shaping: place txs so the in-block dependency graph produces exactly
   `levelSizes`. Verify against `selectPrepareTxsPerLevel` rather than trusting
   the construction — a generator that thinks it built 25 levels but built 1 is
   the most likely way this whole exercise silently measures nothing.
4. Seed **only parents** into Aerospike via `utxoStore.Create` with mined info so
   `BlockHeights` is non-empty. Block txs stay absent from cache and store.
5. Assemble subtrees + subtreeData into the blob store, build the `model.Block`,
   serialize for the request.

**Verify:** a 500-tx / 5-level fixture round-trips through `CheckBlockSubtrees`
with no errors, and an assertion confirms `missed == txCount` on the pre-check —
i.e. the unseen precondition genuinely holds. If `missed` comes back 0 the
fixture is seeding too much and every subsequent number is meaningless.

## Phase 3 — Instrumentation and profiling

1. Wall-clock timing around `CheckBlockSubtrees`, derived tx/s.
2. `runtime/pprof` CPU, block and mutex profiles to the scratchpad. Set
   `runtime.SetBlockProfileRate` and `SetMutexProfileFraction` — without them the
   block and mutex profiles come back empty, which is the failure mode that
   wastes a whole run.
3. Capture per-level durations. The existing per-level logs are `Debugf`
   (`check_block_subtrees.go:1112`, `:1223`); capture them via a recording logger
   rather than editing production log levels, so the harness needs no production
   change to work.

**Verify:** a run emits non-empty CPU, block and mutex profiles and a per-level
duration table.

## Phase 4 — Baseline measurement (decision point)

Run the shape-matched baseline: 6,258 txs, 25 levels, L0 = 2936, median 119.

**Verify and branch:**

- **≤100 tx/s** → reproduced. Proceed to phase 5.
- **>1000 tx/s** → not reproduced. Stop, report, escalate to the real-block-959979
  fallback agreed in the design.
- **Between** → report the number and the profile before choosing; a partial
  reproduction may still localise the bottleneck, but the decision is the user's.

## Phase 5 — Bisect matrix

Only if phase 4 reproduced. Flip each axis independently from baseline:

| run | flip |
|---|---|
| A | block assembly disabled |
| B | `parentExtendedSize` > 32 KB (forces external store) |
| C | single flat level |
| D | `prefetchLevelParents` disabled |
| E | `spendBatcherConcurrency` 4 → 32 |

**Verify:** a table of tx/s per run against baseline, plus the profile for
whichever flip moved the number most.

## Phase 6 — Report

Post findings to issue #1379: the corrected hypotheses from code reading, the
reproduced number, the bisect table, and the named bottleneck. Fix design is a
separate pass.

## Non-goals

The fix, the INFO log promotion, the Coralogix metrics shipping, and the legacy
delivery dedup. All tracked on the issue, all deferred until the profile names
the bottleneck.
