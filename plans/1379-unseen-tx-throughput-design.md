# Reproducing the unseen-tx block validation throughput collapse

Design for a local reproduction harness and bisect for
[issue #1379](https://github.com/bsv-blockchain/teranode/issues/1379):
block validation drops to ~30–50 tx/s on blocks whose transactions never
arrived via propagation.

## Problem

On mainnet 2026-07-29, block 959984 (27,132 txs, 23.7 MB) took 12+ minutes to
validate on all three BSVA mainnet nodes. The same day, block 959964 —
232,673 txs, 62.6 MB — validated in 0.2–10.6 s on the same nodes.

Size is not the variable. The discriminator is the pre-check in
`processTransactionsInLevels` (`services/subtreevalidation/check_block_subtrees.go:1009-1028`):
when every tx in the block is already in the txMeta cache or UTXO store,
`missed == 0` and the function returns immediately. When the block contains txs
the node never saw via propagation, each one goes through
`blessMissingTransaction` and throughput collapses.

The cost scales with the count of *unseen* txs, not block size, and behaves
identically on v0.15 and v0.16.0-beta-1. A block carrying ~1M never-broadcast
txs would take roughly 6–9 hours per node. Any miner can construct one.

## What code reading already settled

Three of the issue's own hypotheses were resolved by reading the code, before
any measurement.

**TX_LOCKED retry backoff is not the cause.** `services/validator/Validator.go:479-510`
caps backoff at 10/20/40 ms over 3 retries, hard-clamped at 10 attempts. Maximum
~70 ms per tx. It cannot produce the observed ~8.7 s per level.

**The issue quoted the wrong batcher numbers.** It cited
`utxostore_spendBatcherSize = 100` and `spendBatcherDurationMillis = 100`; those
are the Go struct defaults in `settings/settings.go:445-446`, used only when the
key is absent. `settings.conf` overrides them to 1024 and 10. The real
constraint on that path is `utxostore_spendBatcherConcurrency = 4`
(`settings.conf:1298`) — four concurrent Lua batch operations.

**`prefetchLevelParents` is not the root cause.** It landed in `7091af082`,
first tagged v0.16.0-beta-1. The v0.15 nodes were equally slow, so it cannot
explain the collapse. It remains a real latent regression: `BatchDecorate`
(`stores/utxo/aerospike/get.go:697-742`) walks batch results serially and calls
`GetTxFromExternalStore` inline, converting external-store reads that were
previously parallel across the level's goroutines into serial ones.

Two further observations shape the harness:

**The existing histogram already exists.** `prometheusSubtreeValidationBlessMissingTransaction`
is observed at `services/subtreevalidation/SubtreeValidation.go:412`. The issue's
ask for "a latency histogram around `blessMissingTransaction`" is already
satisfied in code; the gap is purely that nothing scrapes it on the mainnet
nodes.

**The validator is in-process.** `useLocalValidator = true` (`settings.conf:1255`).
A Go test wiring subtreevalidation → real `validator.Validator` → real GoBDK is
therefore topology-faithful, not an approximation. This is what makes a local
repro cheap.

**New suspect the issue missed.** At the tip the FSM is RUNNING, so
`addTXToBlockAssembly = true`. Every unseen tx then pays Create-with-locked →
block-assembly insert → Kafka txmeta → `SetLocked` unlock two-phase commit
(`services/validator/Validator.go:940-1057`). Two extra store round trips per tx
that the catchup path does not pay, and the manufacturing source of the
TX_LOCKED children the issue observed. Cached txs skip all of it — which is
exactly the fast/slow discriminator.

## Approach

Reproduce locally against real Aerospike and a real validator, profile, then fix
what the profile names. A replay on a live mainnet node was considered and
rejected on correctness grounds, not just risk: the precondition is that the txs
are unseen, and the first validation put them in the txmeta store, so a replay
hits the `missed == 0` fast path and returns in milliseconds. The bug destroys
its own precondition.

The fix is deliberately out of scope for this design. Scoping it before the
profile lands would be guessing, which is what this exercise exists to stop.

## Harness

New file `services/subtreevalidation/check_block_subtrees_unseen_perf_test.go`,
gated `//go:build aerospike` so `make test` never tries to start a container.
Invoked as `go test -tags aerospike -run TestUnseenTxThroughput`.

Nothing is mocked below the server. The wiring extends the pattern already
proven in `legacy_real_validator_integration_test.go:85-130`:

- Aerospike from `test/utils/aerospike.InitAerospikeContainer()`.
  `aerospikestore.New` registers the Lua UDFs itself
  (`stores/utxo/aerospike/aerospike.go:365-383`), so `SPEND_BATCH_LUA` genuinely
  runs.
- Real `validator.New(...)`, real `TxValidator`, real GoBDK. No
  `SkipScriptValidation`, so script verification cost is inside the measurement.
- `blockchain.NewLocalClient` with the FSM pinned to **RUNNING**, not
  `CATCHINGBLOCKS`. This is where the existing integration test differs and why
  it cannot be reused directly: RUNNING is what sets
  `addTXToBlockAssembly = true`, the state the slow mainnet blocks were actually
  in, and it is what pulls in the per-tx block-assembly insert and the
  `SetLocked` 2PC unlock.
- Entry via `CheckBlockSubtrees(ctx, *CheckBlockSubtreesRequest)` with
  serialized block bytes — not `processTransactionsInLevels` directly — so the
  pre-check, subtree splitting and level machinery all run as they do on the
  wire.

## Fixture

Parameterised on `txCount`, level histogram, `inputsPerTx`, and
`parentExtendedSize`. The last straddles the 32 KB `MaxTxSizeInStoreInBytes`
threshold (`stores/utxo/aerospike/aerospike.go:92`) that decides whether a
parent is stored externally.

Transactions are real coinbase-funded chains with real signatures, so GoBDK does
genuine work. Generation streams through a channel and caches to a blob store,
reusing the generator in `check_block_subtrees_large_test.go` rather than writing
a second one.

The precondition is explicit and is the whole point: **only the parents are
seeded** into Aerospike, as mined with `BlockHeights` set. The block's own txs
are absent from both the txMeta cache and the store. That forces `missed > 0`
instead of the `missed == 0` early return.

Baseline shape matches the measured block 959979: 6,258 txs, 25 levels,
L0 = 2936, median 119 txs/level.

### Deliberate simplifications

Block height sits below mainnet CSVHeight (the 257727 fixture height the
existing integration test documents) so the post-CSV parent-MTP path needs no
real headers. That path is one blockchain call per block, not per tx, so it
cannot affect throughput.

The container namespace is in-memory single-node; mainnet is persistent. A
*slow* local result is therefore strong evidence and a *fast* one is weak. This
asymmetry is a limit of the harness, not a defect to paper over.

## Measurement

Per run: wall time of `CheckBlockSubtrees`, derived tx/s, and `runtime/pprof`
CPU, block and mutex profiles written to the scratchpad.

Block and mutex profiles are the primary artefacts. If this is latency-bound on
a store round trip, CPU will look idle and only the block profile shows where
the time went.

## Bisect matrix

Each axis is flipped independently from the shape-matched baseline. Whichever
flip collapses the runtime names the bottleneck.

| axis | baseline → flip | hypothesis under test |
|---|---|---|
| block assembly | RUNNING+2PC → Disabled | per-tx BA insert + `SetLocked` unlock |
| parent size | <32 KB → >32 KB external | serial `GetTxFromExternalStore` in `BatchDecorate` |
| level shape | 25 levels → 1 flat level | level serialization + per-level `prefetchLevelParents` |
| prefetch | on → off | whether `7091af082` is a regression |
| `spendBatcherConcurrency` | 4 → 32 | Lua batch concurrency ceiling |

## Success criteria

Baseline reproduces **≤100 tx/s** → the hypothesis space is local and the
profile is authoritative. Proceed to the bisect matrix, then design the fix.

Baseline runs at **>1000 tx/s** → the synthetic shape is wrong. Escalate to
replaying real block 959979: fetch the block and its ~3k distinct parent txs
from WhatsOnChain, seed the parents into Aerospike as mined, run the real block
through `CheckBlockSubtrees`.

## Known weakest assumption

The synthetic shape matches tx count and level histogram, but if the trigger is
some aspect of real mainnet tx composition not parameterised here — unusual
input counts, script sizes, or parents split across `utxoBatchSize` records —
the baseline runs fast and the real-block fallback is needed. The bisect matrix
partially mitigates this: sweeping `inputsPerTx` and `parentExtendedSize`
independently probes two of those three dimensions directly.

## Out of scope

Deferred to follow-up work, tracked on the issue:

- The fix itself, pending the profile.
- Promoting the level count, per-level duration and `missed` count from `Debugf`
  to `Infof` (issue ask 1). Worth doing, but the profile should inform which
  numbers are actually worth logging at INFO.
- Shipping node metrics from the mainnet nodes to Coralogix (issue ask 2) —
  infrastructure, not code.
- Deduping concurrent legacy block deliveries per block hash (issue ask 3).
  Independent of the per-tx latency question and a clear win on its own, but it
  reduces wasted work by ~3x rather than touching the 30–50 tx/s floor.
