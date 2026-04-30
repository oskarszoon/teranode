# Getting Started with Teranode

This tutorial walks through a first Docker-based Teranode deployment using
[teranode-quickstart](https://github.com/bsv-blockchain/teranode-quickstart).
By the end, you will have a local Teranode node configured, started, and checked
with the quickstart status, RPC, and CLI helpers.

For multi-node or horizontally scaled deployments, use the Kubernetes operator
guides instead.

## Prerequisites

- Basic command-line familiarity
- Git
- Docker and Docker Compose v2
- Enough host resources for the network you choose

| Network | Recommended RAM | Minimum RAM | Disk | CPU cores |
| --- | --- | --- | --- | --- |
| teratestnet | 32 GB | 16 GB | 100 GB | 8+ |
| testnet | 32 GB | 16 GB | 300 GB | 8+ |
| mainnet | 256 GB | 128 GB | 2 TB+ | 16+ |
| regtest | 8 GB | 4 GB | 20 GB | 4+ |

Teratestnet is usually the quickest shared-network starting point because a
canonical UTXO snapshot is available.

## Install Quickstart

```bash
git clone https://github.com/bsv-blockchain/teranode-quickstart.git
cd teranode-quickstart
```

Read the network notes before setup:

```bash
less docs/NETWORKS.md
```

## Configure

Run the setup script:

```bash
./setup.sh
```

The script asks which network to run, whether to use listen-only or full mode,
whether to enable archival block persistence, and which RPC credentials to use.
It writes the result to `.env`.

For a first node, choose listen-only mode unless you already have a public asset
endpoint, P2P port, DNS, TLS, and firewall path ready.

## Optional: Seed Teratestnet

If you selected teratestnet, seed from the canonical snapshot before starting:

```bash
./seed.sh 000000002ea94a515ad9fd40d710fd249fe8610acef7b74f459446812d565187
```

For mainnet and standard testnet, bring your own compatible seed source or sync
from the network.

## Start

```bash
./start.sh
```

Quickstart starts the Teranode services, PostgreSQL, Redpanda, Aerospike,
Prometheus, Grafana, Kafka Console, and the asset cache. It also performs the
normal FSM startup transition.

## Verify

```bash
./status.sh
./rpc.sh getblockchaininfo
./cli.sh getfsmstate
```

Useful local URLs:

| URL | Purpose |
| --- | --- |
| <http://localhost:8090> | Asset viewer |
| <http://localhost:3005> | Grafana |
| <http://localhost:9090> | Prometheus |
| <http://localhost:8080> | Kafka Console |
| <http://localhost:9292> | RPC endpoint |

RPC credentials are stored in `.env` and used automatically by `./rpc.sh`.

## Logs

Tail all logs:

```bash
./logs.sh
```

Tail one service:

```bash
./logs.sh blockchain
./logs.sh legacy
./logs.sh p2p
```

## Basic Operations

Check block height:

```bash
./rpc.sh getblockcount
```

Check FSM state:

```bash
./cli.sh getfsmstate
```

Stop the node:

```bash
./stop.sh
```

Restart the node:

```bash
./start.sh
```

Check for updates:

```bash
./update.sh --check
```

## Next Steps

1. [Install Teranode with Docker](../../howto/miners/docker/minersHowToInstallation.md)
2. [Configure Docker Teranode](../../howto/miners/docker/minersHowToConfigureTheNode.md)
3. [Sync Docker Teranode](../../howto/miners/docker/minersHowToSyncTheNode.md)
4. [Start and Stop Docker Teranode](../../howto/miners/docker/minersHowToStopStartDockerTeranode.md)
5. [Troubleshoot Docker Teranode](../../howto/miners/docker/minersHowToTroubleshooting.md)
6. [Kubernetes Installation](../../howto/miners/kubernetes/minersHowToInstallation.md)
