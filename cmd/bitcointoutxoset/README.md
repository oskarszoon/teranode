# bitcointoutxoset

Extracts and converts the UTXO set from an SV Node (bitcoind) data directory into Teranode's native format. Reads LevelDB (`chainstate` + `blocks`) and writes `{blockhash}.utxo-headers` and `{blockhash}.utxo-set` files consumed by the Teranode `seeder` command.

## Usage

Invoked via `teranode-cli`:

```bash
teranode-cli bitcointoutxoset --bitcoinDir=<bitcoin-data-path> --outputDir=<output-dir-path> [options]
```

The SV Node must be gracefully shut down (`bitcoin-cli stop`) before export. See the [Syncing the Blockchain Guide](../../docs/howto/miners/docker/minersHowToSyncTheNode.md#legacy-sv-node-export) for the full export and seeding workflow.

## Flags

- `--bitcoinDir` — SV Node data directory (required, must contain `blocks` and `chainstate`)
- `--outputDir` — output directory for generated files (required)
- `--skipHeaders` — skip processing block headers
- `--skipUTXOs` — skip processing UTXOs
- `--blockHash` — block hash to start from
- `--previousBlockHash` — previous block hash
- `--blockHeight` — block height to start from
- `--dumpRecords` — dump N records from index for inspection

## Development

- Main logic: `bitcoin_to_utxo_set.go`
- Run tests: `go test -race -tags testtxmetacache ./...` in this directory, or `make test` from project root.

---

For more information, see the main project documentation.
