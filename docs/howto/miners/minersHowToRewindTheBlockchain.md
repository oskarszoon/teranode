# How to Rewind the Blockchain

`teranode-cli rewindblockchain` rolls the blockchain database, UTXO store, and
subtree blob storage back to a target block height. It exists to repair a node
whose UTXO state has diverged from its blockchain DB.

This operation is destructive and cannot be undone. Read the whole page before
running it.

## When to use this

Use it when block validation rejects a block the rest of the network accepted,
and the cause is a UTXO record that disagrees with the chain — for example an
output that should be spendable but is missing from the UTXO set, or one that
should never have been stored.

The symptom is a node stuck at a fixed height, repeatedly failing to validate
the same next block, where restarting and re-syncing do not help. A software
upgrade that corrects the underlying rule does **not** heal an existing
divergence: the wrong record is already written, and block validation does not
rewrite it. Rewinding below the block that created the record deletes it, and
re-validating re-creates it under the corrected rule.

## When not to use this

- **To resync a lagging node** — see [How to Sync the Node](kubernetes/minersHowToSyncTheNode.md).
- **To clear a stuck block assembly** — use `teranode-cli resetblockassembly`.
- **To clear an invalid-block marker** — use `teranode-cli reconsiderblock <blockhash>`.
- **To wipe a node and start over** — see [How to Reset Teranode](kubernetes/minersHowToResetTeranode.md).

## What it does

1. Deletes every block above the target height, across the main chain and all forks.
2. Unspends and deletes those blocks' transactions from the UTXO store.
3. Removes orphaned subtree blobs.
4. Resets the UTXO store's internal block height to the target.
5. Resets `state["BlockAssembler"]` and the block-persister height, and deletes the UTXO-persister `lastProcessed` marker.

## Preconditions

**The node process must be stopped.** The tool does not enforce this. A running
node writing to the same stores during a rewind will corrupt the result.

**The FSM state in the blockchain DB must read `IDLE`.** Check it:

```bash
teranode-cli getfsmstate
```

If it reads anything else, the tool refuses. `--force-not-idle` overrides the
check and exists only to recover from a crashed partial run, where the stored
state is already wrong.

**The current tip must be at or above the target height.** The tool refuses to
"rewind" forward.

**The rewind must be 100 blocks deep or less.** That is the coinbase maturity
window. `--force-deep` overrides it, at the risk that removing coinbase UTXOs
finds children mined on blocks that survive the rewind.

**Subtree blobs for the deleted range must still be present and unpruned.** If
they have been pruned, the rewind cannot reconstruct the range.

## Choosing a target height

With no `--target-height`, the tool targets Block Assembly's persisted height
from `state["BlockAssembler"]`. That is the right default when the two stores
have drifted apart during normal operation.

For a UTXO divergence, target a height **below** the block that created the
divergent output, so the record is deleted and then re-created under the
corrected rule. Targeting above it leaves the bad record in place and the node
wedges again at the same block.

Keep the rewind shallow. Depth is bounded at 100 blocks for a reason.

## Speed: enable outpoint drain mode for the run

Set this for the rewind, then unset it:

```
utxostore_outpointBatcherDrainMode=true
```

The tool calls `PreviousOutputsDecorate` sequentially at roughly two inputs per
call — far below `utxostore_outpointBatcherSize` — so without drain mode the
batcher waits out the full `utxostore_outpointBatcherDurationMillis` (10ms by
default) on every call before flushing. Drain mode removes that wait and is
around 90% faster at these low per-call counts.

Do not leave it enabled on a running node. The batcher is store-wide and shared
with the concurrent validation hot path, which wants predictable latency rather
than drain mode's heavy-tailed behaviour at mid transaction counts.

## Procedure

### 1. Check the FSM state and pick a target — before stopping anything

Do this first, while a pod is still running. Once the node is stopped there is
no Teranode container left to exec into.

```bash
teranode-cli getfsmstate
```

Note the current tip and decide the target height (see
[Choosing a target height](#choosing-a-target-height)).

### 2. Set the FSM to IDLE

```bash
teranode-cli setfsmstate --fsmstate=idle
```

The rewind refuses to run unless the FSM state stored in the blockchain DB reads
`IDLE`.

### 3. Stop the node

Docker — the stack runs one container per service (`blockchain`, `asset`,
`legacy`, `subtreevalidation`, and so on), so stop them all rather than a single
`teranode` service:

```bash
docker compose stop
docker compose ps
```

Kubernetes: scale the cluster down or delete the CR, as in
[How to Stop and Start Teranode](kubernetes/minersHowToStopStartKubernetesTeranode.md).
Confirm no pod is still writing to the stores before continuing:

```bash
kubectl get pods -n teranode-operator
```

The infrastructure pods (Aerospike, PostgreSQL, Kafka) must stay **up** — the
rewind reads and writes through them.

### 4. Get a shell that can run `teranode-cli`

The Teranode containers are gone at this point, so the rewind runs in a fresh
one-off container built from the same image with the same configuration.

Docker — `run` starts a new container from a stopped service's definition, so it
inherits that service's image, volumes, environment, and `SETTINGS_CONTEXT`, and
allocates a TTY by default so the confirmation prompt works. Borrow the
`blockchain` service definition; it shares the same `x-teranode-base` anchor as
every other Teranode service, so its `settings_local.conf` and data mounts are
the node's:

```bash
docker compose run --rm --entrypoint teranode-cli blockchain rewindblockchain --help
```

`--rm` removes the throwaway container on exit. `run` does not start the
service's own process, so nothing competes with the rewind for the stores.

Kubernetes — start a one-off pod from the same image, with the same configmap
and volume mounts the Teranode pods used, then exec into it. The exact image
tag, configmap name, and mounts come from your deployment's CR; check them
against `kubernetes/teranode/teranode-cr.yaml` and
`kubernetes/teranode/teranode-configmap.yaml` rather than copying values from
this page.

Confirm the tool can see the node's configuration before trusting a rewind from
this shell:

```bash
teranode-cli settings | grep -iE 'blockchain_store|utxostore|subtreestore'
```

Those must match the values the node was running with. If they do not, the
rewind would operate on the wrong stores — stop and fix the configuration first.

### 5. Dry run

```bash
teranode-cli rewindblockchain --target-height <height> --dry-run
```

This prints the current tip, the target, and the number of blocks that would be
deleted across the main chain and forks. It modifies nothing. Check the numbers
before going further — particularly that the block count matches the depth you
expect.

### 6. Run it

From the same shell, with a TTY so the confirmation prompt works:

```bash
teranode-cli rewindblockchain --target-height <height> --verify
```

The prompt shows the tip, target, and block count, and waits for `y`:

```
About to rewind the blockchain:
  current tip: 1749337
  target:      1749330
  blocks to delete (main + fork): 7
  This operation is DESTRUCTIVE and CANNOT be undone.

Proceed? [y/N]
```

Without a TTY, the tool reports:

```
no input on stdin (not a TTY?); re-run with an interactive shell (kubectl exec -it / docker exec -it) or pass --assume-yes
```

In that case either re-run with `-it`, or pass `--assume-yes` to skip the
prompt. `--verify` checks after the rewind that the best block sits at the
target height.

### 7. Restart and resume

Exit the one-off container, start the node again (Docker: `docker compose start`;
Kubernetes: re-apply the CR), then take it out of `IDLE`:

```bash
teranode-cli setfsmstate --fsmstate=running
```

Watch that it validates the block it previously rejected.

## If the rewind dies part-way

The rewind is not transactional, but it is safe to re-run. Deletion tolerates
records that are already gone, so re-running with the **same**
`--target-height` continues from wherever it stopped.

The FSM state may no longer read `IDLE` after a crash. In that case add
`--force-not-idle` to the retry — this is the case that flag exists for.

## Worked example: the testnet-eu-1 divergence

testnet-eu-1 (v0.15.5) wedged at height 1749337, rejecting 1749338. A
transaction in the rejected block spent a post-Genesis 0-satoshi bare
`OP_RETURN` output. An earlier, era-blind version of `ShouldStoreOutputAsUTXO`
had dropped that output from the UTXO set, so the spend looked invalid.

Post-Genesis bare `OP_RETURN` outputs are anyone-can-spend, so this class of
divergence is reachable on any network, mainnet included.

Upgrading to the corrected predicate did not heal the node: the divergent record
was already written, and block validation never rewrote it. The repair was a
rewind to a height below the block that created the flipped output, so the
record was deleted and then re-created under the corrected predicate.
