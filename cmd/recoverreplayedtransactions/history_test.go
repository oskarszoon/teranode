package recoverreplayedtransactions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchainsql "github.com/bsv-blockchain/teranode/stores/blockchain/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func TestHistoryCanonicalArchiveAndGaps(t *testing.T) {
	for _, mode := range []string{"valid", "missing-data", "wrong-root", "wrong-raw", "bounded"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			u, err := url.Parse("sqlitememory:///")
			require.NoError(t, err)
			chain, err := blockchainsql.New(ulogger.TestLogger{}, u, test.CreateBaseTestSettings(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, chain.Close(ctx)) })
			archive := memory.New()
			raw := "0100000001" + strings.Repeat("00", 32) + "ffffffff0100ffffffff010100000000000000015100000000"
			coinbase, err := bt.NewTxFromString(raw)
			require.NoError(t, err)
			tx, err := bt.NewTxFromString(strings.Replace(raw, "ffffffff0100", "000000000100", 1))
			require.NoError(t, err)
			st, err := subtree.NewTree(1)
			require.NoError(t, err)
			require.NoError(t, st.AddCoinbaseNode())
			require.NoError(t, st.AddNode(*tx.TxIDChainHash(), 0, uint64(tx.Size())))
			storedRoot := st.RootHash()
			data, err := st.Serialize()
			require.NoError(t, err)
			require.NoError(t, archive.Set(ctx, storedRoot[:], fileformat.FileTypeSubtree, data))
			rawData := tx.Bytes()
			if mode == "wrong-raw" {
				rawData = coinbase.Bytes()
			}
			if mode != "missing-data" {
				require.NoError(t, archive.Set(ctx, storedRoot[:], fileformat.FileTypeSubtreeData, rawData))
			}
			root, err := st.RootHashWithReplaceRootNode(coinbase.TxIDChainHash(), 0, uint64(coinbase.Size()))
			require.NoError(t, err)
			if mode == "wrong-root" {
				root = &chainhash.Hash{1}
			}
			prev, _, err := chain.GetBestBlockHeader(ctx)
			require.NoError(t, err)
			bits, err := model.NewNBitFromString("207fffff")
			require.NoError(t, err)
			block := &model.Block{Header: &model.BlockHeader{Version: 1, HashPrevBlock: prev.Hash(), HashMerkleRoot: root, Timestamp: 1700000001, Bits: *bits}, CoinbaseTx: coinbase, Subtrees: []*chainhash.Hash{storedRoot}, TransactionCount: 2, Height: 1}
			_, _, err = chain.StoreBlock(ctx, block, "test")
			require.NoError(t, err)
			tip := replayrecovery.Tip{Hash: block.Hash().String(), Height: 1}
			limit := 100
			if mode == "bounded" {
				limit = 1
			}
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0700))
			options := HistoryOptions{StartHeight: 1, EndHeight: 1, MaxBlobBytes: 1 << 20, MaxBlockTransactions: limit}
			if mode == "valid" {
				options.MaxBlobBytes = 0
				options.MaxBlockTransactions = 0
			}
			progressCalls := 0
			options.Progress = func(c HistoryCoverage) {
				progressCalls++
				require.Equal(t, uint64(1), c.Scanned)
				require.Equal(t, tip, c.Tip)
			}
			history, err := NewHistory(ctx, filepath.Join(dir, "history.sqlite"), chain, archive, options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, history.Close()) })
			require.NoError(t, history.Build(ctx, tip))
			require.Equal(t, 1, progressCalls)
			rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     uint64 `json:"id"`
					Method string `json:"method"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				var result any
				var failure any
				switch request.Method {
				case "getblockchaininfo":
					result = map[string]any{"bestblockhash": tip.Hash, "blocks": tip.Height, "chain": "regtest"}
				case "getrawtransaction":
					failure = map[string]any{"code": -5, "message": "pruned"}
				case "getblockhash":
					result = tip.Hash
				case "gettxout":
				default:
					t.Errorf("unexpected RPC method: %s", request.Method)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": request.ID, "result": result, "error": failure}))
			}))
			t.Cleanup(rpc.Close)
			source, e := replayrecovery.NewRPCSource(rpc.URL, rpc.Client(), history)
			require.NoError(t, e)
			require.NoError(t, source.Configure(replayrecovery.RPCOptions{RequestsPerSecond: 100000, Concurrency: 2, MaxResponseBytes: 1 << 20, Timeout: time.Second}))
			evidence, e := source.Check(ctx, tx.TxID(), tip)
			if mode == "valid" {
				require.NoError(t, e)
				require.Equal(t, replayrecovery.FullySpent, evidence.Classification)
				require.Equal(t, "local-history+rpc", evidence.Source)
			} else {
				require.Error(t, e)
				require.Equal(t, replayrecovery.Unknown, evidence.Classification)
			}
			inclusion, err := history.Lookup(ctx, tx.TxID(), tip)
			if mode == "valid" {
				require.NoError(t, err)
				got, e := replayrecovery.VerifyInclusion(tx.TxID(), inclusion)
				require.NoError(t, e)
				require.Equal(t, tip.Hash, got)
				require.Equal(t, tx.String(), inclusion.RawTx)
				require.Equal(t, uint32(0), history.Coverage().GapCount)
				require.NoError(t, archive.Del(ctx, storedRoot[:], fileformat.FileTypeSubtreeData))
				require.NoError(t, history.Build(ctx, tip))
				_, err = history.Lookup(ctx, tx.TxID(), tip)
				require.NoError(t, err, "second lookup/build must use index without rescan")
				require.NoError(t, history.Close())
				reopened, e := OpenHistory(filepath.Join(dir, "history.sqlite"))
				require.NoError(t, e)
				history = reopened
				later := replayrecovery.Tip{Hash: strings.Repeat("ab", 32), Height: 2}
				again, e := history.Lookup(ctx, tx.TxID(), later)
				require.NoError(t, e)
				require.Equal(t, inclusion, again)
				require.Equal(t, uint64(1), history.Coverage().Scanned)
			} else {
				require.Error(t, err)
				require.Equal(t, uint32(1), history.Coverage().GapCount)
			}
			_, err = history.Lookup(ctx, strings.Repeat("ff", 32), tip)
			require.Error(t, err)
		})
	}
}

func TestHistoryOpenRejectsMissingIndex(t *testing.T) {
	_, err := OpenHistory(filepath.Join(t.TempDir(), "missing.sqlite"))
	require.Error(t, err)
}
