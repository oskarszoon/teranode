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

## Follow-up 5: Class B hypothesis eliminations

Focused hunt for the Class B mechanism. Each row was tested against production logs
or measured in the harness, not reasoned about.

| hypothesis | test | result |
|---|---|---|
| Phase-3 sequential revalidation fallback | `'sequential revalidation'` in both windows | **0** (116 in July overall) |
| Missing / cross-subtree parents | deferral log fires whenever `errorsFound > 0` | never fired → `errorsFound == 0` |
| `DEVICE_OVERLOAD` overload-retry | `'aerospike overloaded'` in both windows | **0** (954 in July overall) |
| Batcher leak guard (160 s) | `'did not complete within'` in both windows | **0** (6 in July overall) |
| `mtpMu` serialising the fan-out | code: `sync.RWMutex`, read path uses `RLock` | shared, no serialisation |
| Pathological level count | both variants set `level = maxParentLevel+1` | tracks longest chain, ~25 |
| External-store read latency | measured on the node | 0.33 ms p50 → 203 serial = 0.11 s |
| **Aerospike connection pool cap** | **harness A/B** | **0.89x — no effect** |

### The connection-pool result is worth keeping

Production caps the client at `ConnectionQueueSize=128` with
`LimitConnectionsToQueueSize=true` (settings.conf:1271) — a hard ceiling where
callers block. `processTransactionsInLevels` independently fans out to
`SpendBatcherSize*2 = 2048` (check_block_subtrees.go:1156). Those two numbers come
from unrelated settings and nothing reconciles them, so on paper the fan-out
oversubscribes a blocking pool ~16x. Every earlier harness run had used
`InitAerospikeContainer`'s bare URL with no connection tuning at all, so the entire
bisect matrix had never modelled it.

Measured: 1,656 tx/s capped vs 1,467 tx/s uncapped. No effect.

The reason is that the fan-out's store operations go through batchers, which
coalesce thousands of validations into a handful of batch calls — so 2048 goroutines
never become 2048 connections. Consistent with `connections_pool_empty = 0` in
production and with the block profile showing all waiting on batcher completion
rather than on connection acquisition.

The harness now applies the production connection params by default anyway, so it
stops flattering itself with an unbounded pool.

### The one positive correlation found

Conflicting-transaction warnings (`[blessMissingTransaction] ... is conflicting`,
logged at Warn so present in Coralogix):

| window | conflicting warnings |
|---|---|
| 959979 (slow) | **189** |
| 959984 (slow) | **13** |
| 12:00–12:17 same day (fast) | **0** |

Present in both slow windows, absent from the fast one. The count does not track
duration (13 conflicts → 12 min; 189 → 3.6 min), so it is not the conflict count
itself — but it is the only block-content marker found so far that separates slow
blocks from fast ones.

The conflict path is expensive and entirely absent from the fixture:
`checkCounterConflictingOnCurrentChain` (counter-conflict hash lookups plus a
`GetMeta` per hash), `MarkConflictingRecursively` (a batched BFS over unconfirmed
descendants — cost scales with descendant depth, not block size), and the
`CreateConflicting` create path. A single slow conflicting tx also stalls its entire
level, because the level's `g.Wait()` cannot return until every tx in it finishes.

### Where the arithmetic points

At 2048-wide fan-out over ~25 levels, 959979 is ~26 waves. 218 s / 26 ≈ **8.4 s per
`blessMissingTransaction`**, against 15 ms mean / 64 ms p99 in production steady
state. So per-call latency degraded ~560x under the fan-out. Steady state validates
~12 tx/s with no concurrency, which is why it looks healthy and why the incident
only shows up on blocks carrying many unseen transactions.

Root cause still not identified. Next untested candidate is the conflicting-tx path,
being the only positive content correlation and wholly unmodelled by the fixture.

## Follow-up 6: REPRODUCED — conflicting transactions abandon the fast path

Modelling conflicting transactions in the fixture reproduces the collapse.

| axis | tx/s | vs baseline | conflict warnings |
|---|---|---|---|
| 0 conflicts | 1,557 | — | 0 |
| **50 conflicts (3.2% of 1,561 txs), descendant depth 0** | **18.8** | **0.012x** | 807 |
| 50 conflicts, depth 5 | 18.6 | 0.012x | 807 |
| 200 conflicts, depth 5 | 11.5 | 0.007x | 1,027 |

Mainnet observed 27–50 tx/s. The harness now produces 11.5–18.8 tx/s. Same regime.

### The mechanism

`processTransactionsInLevels` treats a conflicting transaction as fatal to the whole
batch. `errorsFound` (check_block_subtrees.go:1189) counts conflicting-tx errors
alongside everything else, and only the *all*-missing-parent case is tolerated
(:1244). Any conflicting transaction therefore makes the fast parallel level pass
return an error at :1248, which throws away all the work it just completed and forces
`CheckBlockSubtrees` into `validateMissingSubtreesWithOrderedRetry` — whose **phase 3
is explicitly sequential, one subtree at a time**.

Three measurements pin this down:

- **The level pass itself is fine.** 25 levels, slowest 70 ms, ~1 s total — but the
  run took 83 s. The time is entirely outside the level loop.
- **Descendant depth is irrelevant** (18.8 vs 18.6 tx/s at depth 0 vs 5), so it is
  not `MarkConflictingRecursively` or the children walk.
- **807 conflict warnings for 50 conflicting txs** — roughly 16 revalidations each,
  which is the fallback re-walking the same transactions.

### Why conflicting transactions are not an error case

Double-spend attempts are routine on mainnet. The code explicitly supports them:
`processTransactionsInLevels` sets `WithCreateConflicting(true)` (:1075), the
validator stores them flagged conflicting, and `blessMissingTransaction` blesses them
after `checkCounterConflictingOnCurrentChain` confirms no counter-conflict is mined on
our chain. The block is valid and the transactions are handled correctly.

They are nonetheless counted as `errorsFound`, so their mere presence costs an ~83x
throughput collapse for the entire block.

### This resolves the puzzle that 13 conflicts cost more than 189

The fallback's cost scales with the **block's transaction count**, not with the
conflict count — because the whole block is revalidated, not just the conflicts.

| block | txs | conflicts | duration | rate |
|---|---|---|---|---|
| 959979 | 6,258 | 189 | 218 s | 28.7 tx/s |
| 959984 | 27,131 | 13 | 716 s | 37.9 tx/s |

The *rate* is near-constant and the *duration* tracks tx count, which is exactly what
a serial revalidation predicts and is why 13 conflicts in a 27k-tx block cost more
than 189 in a 6k-tx block. Earlier sections treated that inversion as evidence
against the conflict hypothesis; it is in fact a prediction of it.

### Consistency with the production logs

The `'sequential revalidation'` INFO line showed 0 occurrences in both Class B
windows, which earlier looked like it ruled the fallback out. It does not: that log
(:1245) fires only when `errorsFound == missingParentErrors`. With conflicts the
counts differ, so :1248 returns a processing error and no such line is emitted. The
absence of that log is what this mechanism predicts.

### Fix direction

Conflicting transactions should not fail the batch. The same deferral already applied
to all-missing-parent errors should extend to conflicting-tx outcomes: they are a
successful, expected result, not a validation failure. That keeps the block on the
parallel level path and removes the ~83x penalty.

Worth checking as part of that change: whether any conflicting tx needs the fallback
at all, or whether the conflicting-create plus counter-conflict check already leaves
the store in its final correct state — in which case the error return is pure waste.

## Follow-up 7: follow-up 6's mechanism was wrong, and the first fix attempt failed

### Correction: the level pass never fails

Follow-up 6 claimed conflicting transactions make `processTransactionsInLevels`
return an error, forcing the phase-2/phase-3 fallback. **That is wrong.** Checked
against the run output: there is no `Failed to process level`, no
`Completed processing with N errors`, and only one pre-check line per run. The level
pass completes successfully.

Reading the code confirms why. For a conflicting tx whose counter-conflict is not
mined on our chain, `blessMissingTransaction` returns `(txMeta, nil)` — the
`ErrTxConflicting` from the validator is deliberately not propagated
(SubtreeValidation.go:426-431), and `checkCounterConflictingOnCurrentChain` reassigns
`err` to nil on success. So `errorsFound` is never incremented and nothing is
deferred.

Also corrected: `validateMissingSubtreesWithOrderedRetry` runs **unconditionally**
after the level pipeline (check_block_subtrees.go:681), not as a fallback. The level
pass is a pre-warm; the per-subtree pass is authoritative.

### What is actually established

The reproduction stands — it is the mechanism explanation that was wrong:

- 3% conflicting transactions collapse throughput 83x (1,557 → 18.8 tx/s), which
  brackets mainnet's observed 27–50 tx/s.
- Descendant depth is irrelevant, so it is not `MarkConflictingRecursively`.
- The level loop accounts for ~1 s of an 80 s run, so the cost is outside it.
- The block profile puts **79.7% of all blocking delay on
  `go-batcher SetMaxConcurrent.func1`** via `chanrecv2` — 5,058 s over an 80 s run,
  i.e. ~63 batch-dispatch goroutines permanently waiting for a slot against a
  64-slot limit. The batchers are saturated for the entire run.
- `utxostore_batcherMaxConcurrent.docker.m = 24` (settings.conf:1338) caps mainnet at
  24 slots; the base default is 64, which is what the harness gets. So production has
  2.7x less headroom than the harness that already collapses.

### Fix attempt 1: batch the counter-conflict reads — DID NOT WORK

`checkCounterConflictingOnCurrentChain` fanned out one `GetMeta` per
counter-conflicting hash across a 128-wide errgroup, nested inside the 2048-wide
per-level fan-out, and fetched whole records when only `BlockIDs` is read. Replaced
with a single `BatchDecorate` requesting just `fields.BlockIDs`.

Measured: **18.8 → 20.5 tx/s. Noise.** Not the bottleneck.

Kept in `git stash` — it is a genuine improvement (N round trips to 1, one field
instead of a whole record, removes a nested fan-out) but it does not fix #1379 and
should not be presented as doing so.

### What is still unknown

Which operation in the conflict path saturates the batchers. The numbers say the cost
is roughly fixed per conflicting transaction and large: 50 conflicts → 80 s (~1.6 s
each), 200 conflicts → 132 s (~0.66 s each). Sub-linear, so there is a shared
component.

Also unexplained: conflict warnings do not scale with conflict count — 807 for 50
conflicts, 1,027 for 200. Roughly constant total, which suggests a fixed quantity of
repeated work rather than per-conflict work.

Next step is a CPU profile alongside the block profile for a conflict run, to
separate "waiting on saturated batchers" from whatever is generating the batch
volume. The block profile alone shows the queue, not its source.

## Follow-up 8: the profiles were misleading; op counts locate it

### Correction: the block profile measures idle parking, not contention

Follow-up 7 read 79.7% of blocking delay on `go-batcher SetMaxConcurrent` as batcher
saturation, and built a `batcherMaxConcurrent.docker.m = 24` story on it. **Both are
withdrawn.**

The goroutine profile shows 7 batchers each holding exactly 128 goroutines parked in
`SetMaxConcurrent.func1` on an empty channel — idle worker pools, not queued work.
Decisive check: the 0.89 s baseline run reports 69 s of blocking delay (78x wall) and
the 78 s conflict run reports 6,200 s (also 78x wall). Identical ratio, so the block
profile is measuring goroutines parked idle, proportional to pool size times wall
time. It says nothing about contention.

The CPU profile is the reliable one: **4.45 s of samples over 78.5 s wall — 5.67%**.
The process is idle. What CPU exists is in the Aerospike client's batch command
execute and connection read/write. So the run is latency-bound on store round trips,
and the question is only how many.

### Op counts from the in-process Prometheus registry

| metric | baseline (0.9 s) | conflicts=50 (86.8 s) |
|---|---|---|
| `subtreevalidation_bless_missing_transaction` | 1,561 (= tx count) | **3,122 (2x)** |
| **`aerospike_txmeta_get`** (single record) | **0** | **15,125** |
| `aerospike_txmeta_get_multi_n` (batched) | 6,245 | 27,613 |
| `validator_transactions_spend_utxos` | 1,561 | 3,122 |

Two measured facts:

1. **Every transaction is blessed twice** in the conflict run and once in the
   baseline. So the second (per-subtree) pass skips everything in the baseline and
   re-validates everything when conflicts are present — all 1,561, not just the 50
   conflicting ones.
2. **15,125 single-record Gets against zero in the baseline.** At 86 s / 15,125 ≈
   5.7 ms each this accounts for the entire runtime, and 5.7 ms is what a partially
   filled 10 ms get-batcher window costs (`utxostore_getBatcherDurationMillis = 10`).

### Mechanism (measured parts vs inferred parts)

Measured: the doubling of blessings, the 15,125 single Gets, 5.67% CPU, and the
per-Get cost matching the batcher window.

Inferred and still to be confirmed: that the second pass is
`validateMissingSubtreesWithOrderedRetry` re-validating whole subtrees because they
contain a conflicting transaction, and that the single Gets arise because
`ValidateSubtreeInternal` has no equivalent of `prefetchLevelParents` — so parent
resolution falls back to a per-parent `Get` in the validator instead of one batched
read per level.

This also accounts for the two anomalies follow-up 7 could not explain:

- **Sub-linearity** (50 conflicts → 80 s, 200 → 132 s rather than 4x): in both cases
  every subtree is already being reprocessed, so extra conflicts add little.
- **Near-constant warning count** (807 at 50 conflicts, 1,027 at 200): the repeated
  work is per-subtree, not per-conflict.

### Fix direction

The cost is not the conflict handling itself — it is that one conflicting transaction
in a subtree forfeits the level pass's batched parent prefetch for every transaction
in that subtree. Two candidate directions, both needing the inference above confirmed
first:

1. Stop a conflicting transaction from invalidating the whole subtree's
   already-validated state, so the second pass skips the other transactions as it does
   in the baseline.
2. Give the per-subtree path the same batched parent prefetch the level path has, so
   the fallback costs one batched read per subtree rather than ~10 single Gets per
   transaction.

The harness now dumps these op counts on every run, so either change can be checked
against `aerospike_txmeta_get` returning to zero rather than only against tx/s.

## Follow-up 9: pass attribution refutes follow-up 8's inference

Snapshotting the counters at the `[CheckBlockSubtrees] Completed processing ...` Infof
(check_block_subtrees.go:608), which sits between the level pipeline and
`validateMissingSubtreesWithOrderedRetry`, attributes every store op to one pass.

| pass | baseline (0.9 s) | conflicts=50 (78 s) |
|---|---|---|
| level pipeline | `txmeta_get=1` `bless=1,561` `get_multi_n=4,684` | `txmeta_get=3,380` **`bless=3,122`** `get_multi_n=14,307` |
| per-subtree pass | `txmeta_get=0` `bless=0` `get_multi_n=1,561` | **`txmeta_get=11,745`** **`bless=0`** `get_multi_n=13,306` |

Follow-up 8 inferred that the per-subtree pass re-validates every transaction when
conflicts are present, and that its missing parent-prefetch turns those into single
Gets. **Both halves are wrong.**

### The blessing doubling is inside the level pipeline

`bless_missing_transaction` goes 1,561 → 3,122 **entirely within the level pipeline**;
the per-subtree pass records **zero** blessings in both runs. So the second pass is not
re-validating anything. Something makes the level pipeline bless every transaction
twice when conflicts are present — the pipeline is repeating its own work, and that is
where the extra 3,380 single Gets on that side come from too.

The baseline shows the level pipeline blessing each transaction exactly once
(1,561 = tx count), so the doubling is conflict-induced, not structural.

### The Gets are in the per-subtree pass, but it blesses nothing

11,745 of the 15,125 single Gets (78%) occur in the per-subtree pass, which performs
**zero** blessings. So that pass is not validating transactions; it is reading records
individually for some other reason. `blessMissingTransaction` is not on that path, so
the earlier guess that the absence of `prefetchLevelParents` degrades parent resolution
there cannot be the explanation either — no parent resolution for validation is
happening.

Its batched reads also roughly match the level pipeline's (13,306 vs 14,307), so the
pass is doing a full pre-check either way; the single Gets are additional to that.

### Where this leaves it

Two separate unexplained behaviours, both conflict-induced:

1. The level pipeline blesses every transaction twice (3,122 vs 1,561) and issues
   3,380 single Gets against the baseline's 1.
2. The per-subtree pass issues 11,745 single Gets while blessing nothing.

Between them these account for the runtime, since 15,125 Gets at the ~5.7 ms cost of a
partially filled 10 ms get-batcher window is ~86 s.

Neither is explained yet. What is now firmly established, and useful regardless: the
harness reproduces the collapse deterministically, attributes cost per pass, and has
refuted three successive mechanism hypotheses — the phase-3 fallback (follow-up 6),
batcher saturation (follow-up 7), and per-subtree revalidation (follow-up 8). The
remaining question is narrow and instrumented: what issues single-record Gets on each
of these two paths.
