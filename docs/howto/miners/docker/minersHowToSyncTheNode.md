# Sync Docker Teranode

Last modified: 28-April-2026

Docker deployments sync through the
[teranode-quickstart](https://github.com/bsv-blockchain/teranode-quickstart)
workflow. You can either let Teranode sync from the network or seed it from a
compatible UTXO snapshot before startup.

## Choose a Sync Method

| Method | Best for | Command path |
| --- | --- | --- |
| Network sync | Fresh installs without a seed source | `./start.sh` |
| Snapshot seeding | Faster initial sync when a compatible snapshot is available | `./seed.sh ... && ./start.sh` |
| Existing Teranode data | Recovery from your own backup | Restore volumes, then `./start.sh` |

Snapshots are usually pruned. They speed up UTXO initialization, but they do
not provide full historical transaction data unless the source explicitly
contains it. Enable `blockpersister` before syncing if you need raw historical
block data for an explorer, indexer, or archive.

## Network Sync

Network sync needs no seed data:

```bash
./setup.sh
./start.sh
```

Monitor progress:

```bash
./status.sh
./logs.sh blockchain
./rpc.sh getblockchaininfo
```

Mainnet network sync can take days and depends on hardware, bandwidth, peer
quality, and current chain activity.

## Seed from a Snapshot

Seeding writes UTXO state before the full stack starts. Start from a clean data
volume:

```bash
./stop.sh
./clean.sh --data-only
```

### Teratestnet

Teratestnet has a canonical snapshot. Quickstart derives the URL from the block
hash:

```bash
./seed.sh 000000002ea94a515ad9fd40d710fd249fe8610acef7b74f459446812d565187
./start.sh
```

### Mainnet and Testnet

Mainnet and standard testnet do not have a canonical public snapshot URL. Bring
your own compatible seed directory or URL:

```bash
./seed.sh <block-hash> /path/to/seed-dir
./start.sh
```

or:

```bash
./seed.sh <block-hash> https://example.com/path/to/snapshot.zip
./start.sh
```

The block hash must match the snapshot. The snapshot network must match the
configured `.env` network.

### Legacy SV Node Export

If your seed source is an existing SV Node data directory, export it to
quickstart-compatible seed files with the Teranode image. Stop SV Node
gracefully before reading its `blocks` and `chainstate` directories:

```bash
bitcoin-cli stop
```

From the quickstart repository root, load the pinned Teranode version and write
the export to a local seed directory:

```bash
set -a
. ./.env
set +a

mkdir -p /path/to/teranode-seed

docker run --rm \
  --entrypoint /app/teranode-cli \
  -v /path/to/svnode-data:/svnode:ro \
  -v /path/to/teranode-seed:/seed \
  ghcr.io/bsv-blockchain/teranode:"$TERANODE_VERSION" \
  bitcointoutxoset --bitcoinDir=/svnode --outputDir=/seed
```

Use the block hash from the generated seed filenames when loading quickstart:

```bash
./seed.sh <block-hash> /path/to/teranode-seed
./start.sh
```

## Seed from `.env`

You can also configure seed values in `.env`:

```env
SEED_HASH=<block-hash>
SEED_DIR=/path/to/seed-dir
```

Then run:

```bash
./seed.sh
./start.sh
```

Use `SEED_URL` instead of `SEED_DIR` when the seed is a downloadable ZIP file.

## Restore Existing Data

If you maintain backups of the quickstart Docker volumes, restore them while the
stack is stopped, keep `.env` aligned with the restored network and version,
then start normally:

```bash
./start.sh
./status.sh
```

Do not restore data from one network into a configuration for another network.

## Verify a Seed Before the Node Starts Catching Up

The quickstart stack runs the `docker.m` settings context, which boots a fresh
node into `IDLE`. A node with no persisted FSM state — which is what `./seed.sh`
leaves behind, since the seeder writes the UTXO set, headers and the
BlockAssembler checkpoint but deliberately not the FSM state — therefore comes up
quiescent. Nothing dials peers, fetches blocks or mutates the seeded state until
you say so.

Use that window to confirm the seed landed as intended:

```bash
./status.sh                                 # services healthy and connected to their stores
./cli.sh getfsmstate                        # expect IDLE
```

Check, before moving on:

- the seeded tip height and hash match the snapshot you meant to load
- the BlockAssembler checkpoint points where you expect
- every service is up and talking to Aerospike / PostgreSQL / Kafka
- `.env` names the same network the snapshot belongs to — loading a testnet
  snapshot into a mainnet configuration is a real mistake to make, and this is
  the last cheap moment to catch it

Then release the node:

```bash
./cli.sh setfsmstate --fsmstate running
./cli.sh getfsmstate
```

On a checkpointed network (mainnet, testnet) `running` is routed through
catch-up when the seeded tip is still below the network's highest checkpoint, so
`getfsmstate` will report `CATCHINGBLOCKS`. That is expected — the node reaches
`RUNNING` once it has caught up. A snapshot at or above the checkpoint goes
straight to `RUNNING`.

There is no way back to `IDLE` once the node is catching up, so do the
verification above before running this command, not after.

To skip the verification window and have the stack start catching up
immediately, set this in `settings_local.conf`:

```ini
blockchain_initializeNodeInState.docker.m = CATCHINGBLOCKS
```

## Troubleshooting Sync

- Check container health with `./status.sh`.
- Check service logs with `./logs.sh blockchain`, `./logs.sh legacy`, or
  `./logs.sh p2p`.
- Confirm the configured network in `.env`.
- Confirm that seed data matches the configured network and block hash.
- For full mode, confirm that public asset and P2P endpoints pass the
  reachability check from `./start.sh`.
- If local state is inconsistent, reset with `./clean.sh --data-only` and seed
  or sync again.

## Related Documentation

- [Install Teranode with Docker](./minersHowToInstallation.md)
- [Reset Docker Teranode](./minersHowToResetTeranode.md)
- Quickstart network notes: <https://github.com/bsv-blockchain/teranode-quickstart/blob/main/docs/NETWORKS.md>
