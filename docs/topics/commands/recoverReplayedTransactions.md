# Recover replayed confirmed transactions

`teranode-cli recoverreplayedtransactions` audits unmined Aerospike records and
block assembly for transactions that were confirmed, fully spent, pruned, then
recreated by replay after a surviving parent lost its `deletedChildren` marker.
Ordinary reset, full reset, and reset with input validation can retain these
records. None of those reset modes performs this recovery.

The default mode is **read-only discovery**. Repair requires independent canonical
confirmation evidence, current output state, complete dependency enumeration,
matching local records, and explicit maintenance acknowledgement. It never
unspends the child's confirmed inputs or removes retained transaction blobs.

## Evidence and supported storage

Apply/resume supports Aerospike only. Use the same settings context, namespace,
set, output batch size, external transaction store, and subtree store as the node.
The command opens a direct Aerospike connection without starting normal UTXO
store background workers. Blockchain and assembly access use their gRPC APIs;
run the matching node version with recovery RPC support.

An explicit trusted SV Node RPC endpoint is required. For example:
`https://bsv-rpc.publicnode.com`. Availability and historical retention can change.
Requests are individual, bounded, rate limited, and unauthenticated when the URL
contains no credentials. The RPC's UTXO view remains a trust dependency.

The command rehashes raw transactions, verifies their Merkle inclusion and
canonical containing block, and checks outputs with `gettxout` using
`include_mempool=false`. A null response alone never authorizes deletion. A
locally empty block-ID list does not prove that a transaction was never mined.
The local blockchain, assembly, and RPC must agree on the pinned tip.

A pruned RPC may lack old transaction bytes. `--history-index` builds or reuses a
private SQLite index from retained canonical blocks and subtree/subtree-data
archives. In discovery, set `--history-start` and `--history-end` to bound a new
index's scan; end zero means the agreed tip. Reusing an existing index does not
extend its range; use a new path to index additional blocks. The index validates
archive commitments, then RPC checks
the canonical inclusion again when evidence is used. Missing or oversized
archives become visible coverage gaps, not proof of absence. Defaults bound each
blob to 64 MiB and each block to one million transactions. Index size can be large.
Interrupted builds require a new index path; completed indexes can be reused.

Raw external transaction blobs can establish dependency identities, but never
confirmation or spentness. Missing bytes, unsupported schemas, missing parent
records/pages, generation conflicts, and incomplete dependency graphs fail
closed. Automatic repair refuses parents with finite expiry: their replay guards
cannot be protected atomically across child record deletions without changing
their TTL. Child expiry and all recorded TTLs are preserved, never extended.

## Maintenance procedure

1. Back up the affected store and preserve the current assembly state and logs.
   Create a dedicated recovery directory on durable storage, owned by the operator,
   mode `0700`. Keep it outside temporary directories and automated cleanup paths.
   Manifest, journal, and history files must be distinct regular files, mode
   `0600`; recovery files and lock files cannot be symlinks. Journals and index
   builds use exclusive locks; completed manifests and indexes allow shared reads.
2. Quiesce ingress and block processing. Keep read access to blockchain and
   assembly available for discovery, with no pending assembly queue. Stabilize
   the local and reference tips. Long scans abort if their pinned tip changes.
3. Discover and export the audit. Review classifications, coverage, dependencies,
   backend identity, raw before-images, generations, expiry, and complete page
   inventories. Use fresh paths for each discovery attempt.
4. Stop **every writer sharing the store**, including remote node replicas,
   validators, block processing, assembly, pruners, DAH workers, and administrative
   jobs. Disable automatic restarts. Keep an isolated blockchain read service
   available. The command cannot enforce this operational isolation;
   `--maintenance` acknowledges it. Changes since discovery are rejected before
   any mutation. If shutdown changed records, perform a fresh quiescent audit.
5. Apply the reviewed manifest. Keep the manifest and journal together. Never
   substitute another namespace, endpoint, or batch-size configuration.
6. After successful apply, restart assembly in a controlled environment and run
   verification with `--reset`. This awaits a real ordinary reset completion,
   checks surviving parents and repaired-record absence, and audits a fresh
   mining candidate. Verification remains pending until a later invocation sees
   a canonical tip transition **after that reset**.
7. Allow a controlled tip advance; run verification again without `--reset`.
   Independently validate a complete fresh mining candidate using the normal
   consensus-validation path before restoring mining/ingress. Preserve the audit
   artifacts for incident review.

Example commands (run from durable storage and supply the deployment's normal
settings context):

```bash
mkdir -m 700 ./replay-recovery

# No UTXO writes. Add --history-index and explicit bounds when old RPC history
# is unavailable and the corresponding local archives are retained.
teranode-cli recoverreplayedtransactions \
  --manifest ./replay-recovery/manifest.db \
  --rpc https://bsv-rpc.publicnode.com

teranode-cli recoverreplayedtransactions --mode export \
  --manifest ./replay-recovery/manifest.db \
  > ./replay-recovery/audit.jsonl

# Only after all shared-store writers have stopped.
teranode-cli recoverreplayedtransactions --mode apply --maintenance \
  --manifest ./replay-recovery/manifest.db \
  --journal ./replay-recovery/journal.db \
  --rpc https://bsv-rpc.publicnode.com

# After restarting assembly; exit 3 is expected until a later tip is observed.
teranode-cli recoverreplayedtransactions --mode verify --reset \
  --manifest ./replay-recovery/manifest.db \
  --journal ./replay-recovery/journal.db \
  --rpc https://bsv-rpc.publicnode.com

# After the post-reset tip transition.
teranode-cli recoverreplayedtransactions --mode verify \
  --manifest ./replay-recovery/manifest.db \
  --journal ./replay-recovery/journal.db \
  --rpc https://bsv-rpc.publicnode.com
```

When using a history index, pass the same `--history-index` path to apply, resume,
and verify. Keep the directory private when exporting JSONL: before-images
contain transaction metadata and raw bytes.

## Interrupted apply

`--mode resume --maintenance` deliberately resumes the **same manifest and
journal**. The journal commits each mutation intent before a remote write and
records its verified outcome. It retains all child page keys even after the
master record was deleted. Resume accepts only exact before-state, exact own
marker mutation, or a deletion already authorized by a recorded intent.
Unrelated changes stop recovery; a record merely containing a marker is not
enough. Tip changes require fresh canonical evidence against the newly agreed
local/RPC tip.

All required parent masters and addressed pages receive and retain the marker
before any child record is deleted. Every deletion boundary rechecks those
parents. Descendants are repaired before affected ancestors. Shared parent
generation changes caused by this journal are tracked explicitly.

Do not delete or edit a journal to bypass a failure, automatically retry with
fresh state, or restore old before-images over a running store. There is no
automatic rollback: investigate divergent records while writers remain stopped.
Never run a blanket unmined purge as a substitute.

## Results and verification limits

The command emits a JSON summary on stdout and progress/errors on stderr;
`export` emits JSONL. Default timeout is 30 minutes, RPC rate 5 requests/second,
and maximum concurrency 4. Adjust `--timeout`, `--rpc-rate`, and `--concurrency`
for the environment. These are bounds, not promises of parallel processing.

| Exit | Meaning |
| --- | --- |
| 0 | Requested mode succeeded; apply alone still needs verification. |
| 1 | Invalid configuration, changed evidence/state, incomplete enumeration, or execution failure. |
| 2 | Audit contains live, unknown, blocked, or unresolved dependency records. |
| 3 | Verification awaits restart, completed reset, or a post-reset tip transition. |

Confirmed transactions with live outputs and transactions whose history cannot
be proved remain untouched. Uncertain members block their connected affected
component. With a complete graph, apply can repair independent eligible
components and still return exit 2. Unconfirmed legitimate transactions can also
remain `unknown`; this conservative classification prevents an automatic claim
that the entire incident is resolved. Export shows unresolved rows even when
the dependency graph is incomplete; such a graph cannot authorize apply.

Verification checks full assembly enumeration and transactions in an actually
materialized mining candidate, surviving original parent spend ownership, replay
markers, repaired-record absence, duplicate candidate spends, and current input
availability. In-candidate inputs must refer to available earlier outputs;
provably unspendable outputs are excluded using the configured Genesis height.
This audit is **not a full script or consensus validator**. A coinbase-only
candidate, a reset acknowledgement, or an apparently healthy template alone
does not demonstrate recovery. The journal also requires store evidence, an
observed assembly restart, completed reset, and subsequent canonical tip change.

For a separate validation pass after maintenance, run the existing
`teranode-cli checkblocktemplate` with the same deployment settings. It requests
a new assembly block candidate and sends it through the block-validation service;
retain its returned block identity and validation result alongside the recovery
journal. Run it only in the controlled post-maintenance environment, since the
normal validation path can have store side effects. This complements the
recovery command's independent RPC input audit; it does not replace that audit
or prove historical spentness from local cached validation alone. Any failure
keeps the node out of normal mining service pending investigation.

If any part remains unresolved, retain the artifacts and report incomplete
recovery; do not equate an empty template with a repaired UTXO store.
