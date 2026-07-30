# Findings — unseen-tx throughput reproduction (#1379)

Results from the harness described in
[the design](./1379-unseen-tx-throughput-design.md). Run on an in-memory
single-node Aerospike testcontainer with a local-file external store, real
`validator.Validator`, real GoBDK, FSM pinned to RUNNING.

## Headline: the collapse did not reproduce

The shape-matched baseline — 6,258 txs in mainnet block 959979's measured level
histogram (25 levels, L0 = 2936) — ran at **5,107 tx/s**. Mainnet observed
**27–50 tx/s** on the same shape. That is a ~100x gap.

The measurement is valid, not misconfigured. The harness asserts both halves of
the precondition and both held:

- `Pre-check: 6258/6258 transactions missed in cache` — every tx was genuinely
  unseen, so nothing took the `missed == 0` fast path.
- `blockAssemblyStores=6258` — every tx went to block assembly, confirming
  `addTXToBlockAssembly == true` and therefore the tip path, not the catchup
  path.
- The production level splitter derived exactly the 25 levels the fixture was
  built for.

So no combination of the level machinery, the 25-level serialisation,
`prefetchLevelParents`, per-tx Create + 2PC + block-assembly insert + Kafka
enqueue, real GoBDK script verification and real Aerospike is *intrinsically*
capable of 30–50 tx/s. Something specific to those nodes is.

## Bisect matrix

Reduced scale: 1,587 txs, same 25-level depth and wide-head/thin-tail character.

| axis | tx/s | vs baseline |
|---|---|---|
| baseline (25 levels, 1 input/tx, small parents) | 1,655 | — |
| **single flat level (same tx count)** | **16,404** | **+891%** |
| batcher timers 10 ms → 1 ms | 2,118 | +28% |
| block assembly disabled (no insert, no 2PC) | 1,912 | +16% |
| external parents (40 KB, above the 32 KB threshold) | 1,621 | −2% |
| `inputsPerTx` = 4 | 1,370 | −17% |
| `inputsPerTx` = 16 | 1,303 | −21% |

## What this establishes

### Level depth is a 10x multiplier

The same 1,587 transactions cost 16,404 tx/s in one dependency level and 1,655
tx/s across 25. Nothing else on the matrix is within an order of magnitude of
that.

This contradicts the issue's own conclusion that "level serialization is not
[the defect]". Level serialisation is not a 200x effect, but it is a 10x one, and
it is structural rather than incidental.

The mechanism, from the block profile (320.7 s of aggregate blocking delay over a
1.225 s wall run):

| phase | blocking delay | share |
|---|---|---|
| `Create` (via `CreateInUtxoStore`) | 125.4 s | 39.1% |
| `Spend` (via `spendUtxos`) | 124.1 s | 38.7% |
| `SetLocked` (via `twoPhaseCommitTransaction`) | 63.4 s | 19.8% |
| **all three via `go-batcher completion.(*Group).Wait`** | **312.7 s** | **97.5%** |

97.5% of all blocking time is goroutines waiting on batcher completion groups.
Within a single transaction those three store phases are strictly sequential, and
a level cannot finish until every transaction in it has completed all three. So a
level costs roughly four serialised store round trips (the three above plus
`prefetchLevelParents`) **regardless of how many transactions it contains** —
which is exactly why a 27-tx tail level and a 2,936-tx head level both cost
~45 ms.

Total block cost is therefore approximately `levels × 4 store round trips`, not
`transactions × per-tx cost`. Block *size* is nearly free; block *depth* is what
costs.

The 10 ms batcher windows are a minority of that: cutting all three to 1 ms
bought only 28%. The rest is the round trips themselves.

### This reframes the issue

At ~4 round trips per level, mainnet's observed 8.7 s per level requires
individual Aerospike batch operations to be taking roughly **2 seconds each**.
Locally they take ~10 ms.

So the thing to chase is not the per-tx validation path — the issue's original
framing, and mine — but why store operations on those nodes are three orders of
magnitude slower than a local container. Circumstantial support already in the
repo and the issue:

- `aerospike_batchPolicy.docker.m` (`settings.conf:171`) carries the comment
  "guard below the 2m overload-retry budget", so Aerospike overload on the
  mainnet stack is a known, previously-encountered condition.
- The issue recorded 71 `TX_LOCKED [SPEND_BATCH_LUA]` events in the window,
  which is what contention on the spend path looks like.
- There are no `*aerospike*` metrics shipped from these nodes at all, so store
  latency was invisible — which is why this could not be diagnosed from
  telemetry.

Issue ask 2 (ship node metrics) moves from "would be nice" to the thing that
would have answered this outright.

### Two suspicions from code reading, killed

**External-store parents are not the local bottleneck.** `BatchDecorate`
(`stores/utxo/aerospike/get.go:697-742`) does walk batch results serially and
call `GetTxFromExternalStore` inline, so N external parents in a level cost N
sequential blob reads. With a local-file external store that measured −2%, i.e.
free. It stays a latent risk for any node whose external store is S3-backed —
where each of those reads is a network round trip, and
`getExternalTransaction` (`get.go:1628-1637`) tries `FileTypeTx` first and only
falls back to `FileTypeOutputs`, so outputs-only parents pay a wasted lookup
first — but it is not what happened here.

**Parent fan-out is not the driver.** Raising inputs per transaction from 1 to 4
cost 17%, and 4 to 16 only a further 4%. Sublinear, so distinct-parent resolution
is not near a cliff.

### The tip path's extra cost is real but small

Disabling block assembly — removing the per-tx insert and with it
Create-with-locked plus the `SetLocked` 2PC unlock — gained 16%, consistent with
`SetLocked`'s 19.8% share of blocking delay. This is the cost the tip path pays
and the catchup path does not, and it is the source of the TX_LOCKED children.
Worth knowing, not the explanation.

## Fix candidates this points at

Ordered by the evidence, not by ease:

1. **Collapse the per-level round trips.** A level currently serialises
   spend → create → setLocked per transaction. Batching each phase *across* the
   whole level, or overlapping levels that have no actual dependency, attacks the
   10x directly. This is the only local finding with a large multiplier.
2. **Ship Aerospike and subtreevalidation metrics from the mainnet nodes**
   (issue ask 2). Without store-latency histograms the ~2 s-per-operation
   hypothesis cannot be confirmed or killed on the next occurrence.
3. **Promote the per-level and `missed` logs to INFO** (issue ask 1) — the
   per-level table this harness had to reconstruct via a capturing logger should
   be available in production logs.
4. **Dedupe concurrent legacy block deliveries** (issue ask 3) — unchanged by
   these findings, still a ~3x waste reduction independent of the per-level cost.
5. Parallelise the external fetch loop in `BatchDecorate` — not this incident,
   but a real cliff for any S3-backed deployment.

## Harness caveats

Stated in the design and still true:

- The container namespace is in-memory single-node; mainnet is persistent and
  under concurrent production load. **A slow local result would have been strong
  evidence; this fast one is weak** — it bounds what the code path costs in
  isolation, and cannot rule out an environmental cause. That is precisely the
  conclusion reached.
- The block-assembly gRPC hop and the Kafka broker round trip are not measured,
  only the enqueue.
- The synthetic level-histogram tail is flat rather than matching the reported
  median of 119; level width below the 2048 concurrency cap does not change the
  code path taken.

## Reproducing

```bash
# baseline, 6,258 txs in mainnet 959979's level shape
TERANODE_PERF_PROFILE_DIR=/tmp/1379 \
  go test -tags aerospike -run 'TestUnseenTxThroughput$' -v -timeout 40m \
  ./services/subtreevalidation/

# full bisect matrix
go test -tags aerospike -run TestUnseenTxBisect -v -timeout 60m \
  ./services/subtreevalidation/
```
