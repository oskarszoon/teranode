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
| external parents (40 KB) — **INVALID, see follow-up** | 1,621 | −2% |
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

### One suspicion from code reading, killed

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
5. Parallelise the external fetch loop in `BatchDecorate`. **Superseded — see the
   follow-up section, which promotes this to a primary candidate.**

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

## Follow-up: externalized transactions (production evidence)

Prompted by "is it a lot of externalized transactions?". Answer: **yes, and they
are fetched serially.**

`BatchDecorate` logs its external-fetch counts at INFO
(`stores/utxo/aerospike/get.go:949`), so this is answerable from production logs
directly. Coralogix, `bsva-infra`, archive tier, all three mainnet nodes:

| window | calls | items | external fetched | max in one call |
|---|---|---|---|---|
| 14:29–14:46 (slow block 959984) | 383 | 31,264 | **1,600** | **203** |
| 12:00–12:17 (quiet, sub-11s blocks) | 176 | 33,881 | 519 | 24 |

Same item volume, **3x the external fetches and 8.5x the worst single call**. The
heaviest calls were nearly all-external and clustered at 14:44:24, seconds before
block 959984 finally completed at 14:44:37:

```
Processed 210 items - External txs: 203 fetched, 0 skipped
Processed 165 items - External txs: 160 fetched, 0 skipped
Processed  87 items - External txs:  84 fetched, 0 skipped
```

203 of 210 records external, and `BatchDecorate` walks its results in a single
serial loop calling `GetTxFromExternalStore` inline (`get.go:697-742`). That is
203 sequential blob reads inside one call, with no concurrency at all.

### A harness bug this exposed, and the corrected measurement

The original external-parents axis reported −2%, i.e. no effect. That row was
**wrong** — it fetched zero externals. Two compounding causes:

1. The generated txs were *extended*. `prefetchLevelParents` only requests
   `fields.Tx` when some tx in the level is NOT extended
   (`check_block_subtrees.go:1293`), and without `fields.Tx` `needsFullExternalTx`
   stays false so the external fetch is skipped entirely. The validator likewise
   skips its parent read (`Validator.go:765`). Legacy blocks from an SV node carry
   raw, non-extended txs — the fixture now writes non-extended subtree data.
2. `ulogger.TestLogger.Infof` is a no-op, so the `BatchDecorate` log was silently
   discarded and "no external log lines" was indistinguishable from "no external
   fetches". The capturing logger now counts them.

With both fixed, the axis actually exercises the path:

| axis | tx/s | external fetched |
|---|---|---|
| baseline (small parents) | 1,646 | 0 |
| external parents (40 KB) | 1,194 | **734 in a single serial call** |

734 sequential external reads cost ~360 ms of a 1.31 s run — about **0.49 ms per
read** against a local-file blob store on SSD.

### What that implies for the mainnet nodes

`utxostore.docker.m` (`settings.conf:1271`) uses
`externalStore=file://${DATADIR}/external?hashPrefix=2` — a file store, not S3.
So the per-read cost there is whatever `DATADIR` is backed by.

For 203 serial reads to account for a level taking 8.7 s, each read has to cost
~43 ms rather than 0.49 ms. That is entirely plausible on network-attached
storage, and this codebase already treats NFS-backed blob stores as a real
deployment shape — `check_block_subtrees.go:405` notes "on NFS-backed blob stores
each `Exists` call is a network round-trip". It is not confirmed, because external
read latency is exactly one of the quantities no metric ships from these nodes.

Two related settings checked and ruled out as the constraint:

- `utxostore_externalStoreConcurrency.docker.m = 4` applies only to the **write**
  path (`create.go:1062`), so it does not throttle these reads.
- `utxostore_useExternalTxCache = true` gives a 10-minute cache, but it is keyed by
  hash and a block's parents are mostly distinct, so it cannot absorb 203 misses.

### Revised fix ranking

Parallelising the external fetch loop in `BatchDecorate` moves from "not this
incident, but a real cliff" to a **primary** candidate. It is currently
concurrency 1; even 8-way would cut a 203-read call by ~25x, and it composes with
the per-level round-trip batching above. `getExternalTransaction`
(`get.go:1628-1637`) additionally tries `FileTypeTx` first and only falls back to
`FileTypeOutputs`, so every outputs-only parent pays a wasted lookup first —
worth fixing in the same pass.

Still needed to close this out: external-read latency from the nodes themselves,
which is issue ask 2.

## Follow-up 2: measured on bsva-ovh-teranode-eu-2 directly

SSH access to the node settled the external-read question with measurement
instead of inference. **The external-store latency hypothesis is dead.**

### The store is huge but fast

| fact | value |
|---|---|
| external store size | **2.2 TB** (of 2.3 TB total used on /mnt/data) |
| shard dirs (`hashPrefix=2`) | 256 |
| entries per shard | ~31,200 (`.tx` + `.tx.sha256`) |
| **estimated external transactions** | **~4 million** |
| mean external tx size | ~575 KB |
| backing store | ext4 on `md10`, RAID0 over local NVMe (`ROTA=0`), 21 TB |
| RAM | 251 GB total, 147 GB page cache |

So yes — externalized transactions are numerous, and they dominate this node's
storage. But reading them is cheap. 160 random `.tx` reads sampled across 40
shards (92 MB total, mostly cold given 2.2 TB against 147 GB of cache):

```
latency ms: min=0.15 p50=0.33 p90=0.82 p99=3.93 max=5.32 mean=0.55
```

203 serial reads — the worst single production `BatchDecorate` call — therefore
costs **0.11 s at p50, 0.8 s even at p99**. Not 8.7 s. The serial fetch loop in
`BatchDecorate` remains a genuine design wart (concurrency 1, and it would bite
hard on an S3-backed external store), but on this hardware it cannot account for
the collapse.

`utxostore.docker.m` is confirmed in use: the container has
`SETTINGS_CONTEXT=docker.m`, so `externalStore=file://${DATADIR}/external?hashPrefix=2`
with `DATADIR` on the NVMe RAID0.

### Why the incident itself can no longer be diagnosed from this node

- Teranode's own metrics endpoints work and expose the right histograms
  (`teranode_aerospike_get_external`, `set_external`) on `:9091` per service — but
  every counter is 0 because all teranode containers were restarted minutes
  before, so there is no incident-era data.
- Aerospike was NOT restarted (up 4 weeks), but its log driver is `json-file` with
  `max-size:100m` and console-only logging, so retained history starts at
  **Jul 30 02:02** — well after the Jul 29 14:29 incident.
- The node now runs **v0.16.0-beta-9**; the incident was on beta-1.

### The monitoring stack was not actually running (since fixed)

Resolved — see follow-up 3. Recorded here as found. Both containers had exited ~2 minutes after being started:

```
prometheus  Exited (2)   open /etc/prometheus/prometheus.yml: permission denied
grafana     Exited (1)   open /etc/grafana/provisioning/dashboards/main.yaml: permission denied
```

Cause was bind-mount permissions, not config content. `config/` and
`config/grafana_dashboards/` are `drwxrwx--- ubuntu:ubuntu` and the mounted files
are `-rw-rw---- ubuntu:ubuntu`, while `prom/prometheus:v2.44.0` runs as uid 65534
and `grafana:12.2.0` as uid 472 — neither can traverse the directory or read the
files. Only `aerospike-exporter` (which needs no host config) came up, which is
why the stack looked partially alive.

No secrets are involved: `prometheus.yml` is scrape targets only and
`grafana_datasource.yaml` contains no password/token, so a group/other read bit is
not an exposure.

### Where this leaves the root cause

External reads: ruled out by measurement. Level depth: confirmed 10x, but a 10x on
a ~45 ms/level floor is ~1 s for a 25-level block, not 12 minutes. The remaining
unexplained factor is still Aerospike operation latency during the incident, and
it is now unmeasurable retroactively on this node.

Closing this out therefore depends on instrumentation being live *before* the next
occurrence — which makes fixing the monitoring stack the highest-value next action,
ahead of any code change.

## Follow-up 3: live production metrics from bsva-ovh-teranode-eu-2

The monitoring stack on that node was not actually running (see follow-up 2:
Prometheus and Grafana both exited on bind-mount permission errors, filed upstream
as bsv-blockchain/teranode-quickstart#9). With permissions fixed, all 9 scrape
targets are up and these are the first real numbers from the #1379 hot path.

The path is genuinely active: `bless_missing_transaction` accumulated 3,784
samples, so unseen transactions are being blessed continuously in steady state.

### Steady-state latency, subtreevalidation service

| metric | n | p50 | p90 | p99 | mean |
|---|---|---|---|---|---|
| `subtreevalidation_bless_missing_transaction` | 3,784 | 16 ms | 16 ms | **64 ms** | 15.2 ms |
| `validator_transactions_input_block_heights` | 3,784 | 16 ms | 16 ms | 16 ms | 11.4 ms |
| `validator_transactions_spend_utxos` | 376 | 33 ms | 33 ms | 131 ms | 25.3 ms |
| `validator_transactions_2phase_commit` | 351 | 8 ms | 8 ms | 8 ms | 6.4 ms |
| `validator_transactions_validate` (GoBDK script) | 376 | <1 ms | <1 ms | 66 ms | 0.56 ms |

Histogram buckets are coarse (16/33/64/131/262 ms), so quantiles are
bucket-granular. Roughly 5 minutes of steady state on **v0.16.0-beta-9**, not
incident conditions.

### The incident was a transient, not the steady state

`blessMissingTransaction` p99 is 64 ms here. The incident's 8.7 s per level needs
per-operation costs in the seconds — 30–100x above what this node does normally.
That closes off any reading of #1379 in which this code path is simply slow by
nature. Whatever happened on 2026-07-29 was a departure from this baseline, and the
baseline is healthy.

### Production confirms the batcher-wait finding

This is the most useful result. Compare the validator-level wrapper against the
underlying Aerospike operation it drives:

| layer | mean |
|---|---|
| `validator_transactions_spend_utxos` | 25.3 ms (p50 33 ms) |
| `aerospike_utxo_spend_batch` | **0.54 ms** |
| `aerospike_utxo_create_batch` | 4.07 ms |

A ~60x gap between the wrapper and the store operation. That gap is batcher
queueing plus the flush timer, which is precisely the local block-profile result
(97.5% of blocking delay in `go-batcher completion.(*Group).Wait`, spread across
Spend 38.7% / Create 39.1% / SetLocked 19.8%) — now reproduced on production
hardware under real load rather than in a testcontainer.

**The store itself is fast. The batching wrapper is where the time goes.** This is
the strongest evidence so far for fix candidate 1 (collapse the per-level round
trips), and it means that fix is justified on steady-state grounds alone,
independent of whatever caused the incident.

### Parent resolution dominates, not script verification

`input_block_heights` is 11.4 ms of the 15.2 ms bless cost, about 75%. GoBDK script
verification is 0.56 ms. So per-tx cost is dominated by parent lookups — which is
the component that scales with parent count and with external-store behaviour, and
the right place to aim any per-tx optimisation.

### External reads are bursty, not continuous

`teranode_aerospike_get_external` has **no samples at all** in this window, despite
~4 million external transactions on disk. So the 203-fetches-in-one-call bursts
seen in the incident logs are event-driven per block, not a standing load. This is
consistent with follow-up 2: external reads are irrelevant in steady state and
appear only on particular blocks.

### Correction to an earlier claim

The section above states "there are no `*aerospike*` metrics shipped from these
nodes at all". That was true when written and is no longer: the metrics existed in
the code and were exposed on `:9091` all along, but nothing scraped them. Issue
ask 2 is therefore narrower than originally filed — it is a scrape/permissions
problem, not missing instrumentation.

## Follow-up 4: the slow events are TWO distinct classes

Grouping every minute-scale `processTransactionsInLevels` event in July by how many
nodes logged it for the same block height splits the data cleanly, and the split
matters: two of the conclusions above were drawn from the wrong class.

| height | txs | nodes hit | hosts |
|---|---|---|---|
| 1730302 | 1 | 1 | `ip-10-11-164-241` |
| 1734144 | 17,808 | 1 | `ip-10-11-164-241` |
| 1735305 | 20,001 | 1 | `ip-10-11-164-241` |
| 956467 | 149,738 | 1 | `ip-10-11-164-241` |
| 956694 | 226,914 | 1 | `ip-10-11-164-241` |
| 957561 | 506 | 1 | `ip-10-11-164-241` |
| **959979** | 6,258 | **2** | `ip-10-11-181-104`, `ovh-eu-2` |
| **959984** | 27,131 | **3** | `ip-10-11-181-104`, `ovh-eu-1`, `ovh-eu-2` |

### Class A — one bad host, not a Teranode bug

All six single-node events are on the **same** EKS worker, `ip-10-11-164-241`. It
spans both mainnet (~956k) and teratestnet (~1.73M) heights because multiple
namespaces schedule pods onto that one machine. Durations bear no relation to
transaction count (1 tx → 64 s; 149,738 tx → 78 s), which is the signature of a
host-level stall — disk, noisy neighbour, CPU throttling.

Out of scope for #1379. Worth raising separately with whoever owns that node.

### Class B — the actual issue

Two events, and they hit a *different* EKS host plus both OVH bare-metal boxes:
independent machines, two hardware providers, simultaneously slow on the same
block. That cannot be node-local. It is block-content-driven.

| height | txs | duration range | rate |
|---|---|---|---|
| 959979 | 6,258 | 3m38s – 3m53s | **~28 tx/s** |
| 959984 | 27,131 | 2m42s – 12m17s | **~37 tx/s** |

~28–40 tx/s, consistently, across unrelated hardware.

### Two corrections to earlier sections

**Follow-up 3's "duration is independent of tx count" is withdrawn.** That was
derived from the 1-tx/64 s event, which is Class A. It says nothing about #1379.
Restricted to Class B, the rate is roughly constant per transaction (~30 tx/s),
which is the original issue framing.

**The level-depth finding is therefore back in play**, not sidelined. Follow-up 3
demoted it on the strength of a Class A datapoint; that demotion was wrong.

### Why this reframes the investigation favourably

Whatever the mechanism is, it is deterministic enough to produce the same ~30 tx/s
on three different machines across two providers. That rules out intermittent
node-local Aerospike stalls, and it means a local reproduction *should* be
achievable.

Which makes the harness's 5,107 tx/s the most valuable remaining clue rather than a
dead end: the ~170x gap is not environmental, so it is something the fixture does
not model about these specific blocks.
