package blockassembly

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	as "github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	rr "github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	blockchainoptions "github.com/bsv-blockchain/teranode/stores/blockchain/options"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	astore "github.com/bsv-blockchain/teranode/stores/utxo/aerospike"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	aeroTest "github.com/bsv-blockchain/testcontainers-aerospike-go"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/require"
)

// This fixture is an independent deterministic transaction history/UTXO oracle,
// not the damaged local store. Headers commit the fixture transaction bytes.
type replayChainFixture struct {
	tip       rr.Tip
	child     *bt.Tx
	inclusion *model.BlockHeader
	parent    *bt.Tx
}

func (f *replayChainFixture) Tip(context.Context) (rr.Tip, error) { return f.tip, nil }
func (f *replayChainFixture) Check(_ context.Context, id string, tip rr.Tip) (rr.Evidence, error) {
	if tip != f.tip {
		return rr.Evidence{}, errors.NewError("fixture tip mismatch")
	}
	ev := rr.Evidence{TxID: id, Tip: tip, Classification: rr.Unknown, Source: "deterministic canonical fixture"}
	if id == f.child.TxID() {
		ev.RawTx = hex.EncodeToString(f.child.Bytes())
		ev.BlockHash = f.inclusion.Hash().String()
		ev.BlockHeight = 1
		ev.Classification = rr.FullySpent
	}
	return ev, nil
}
func (f *replayChainFixture) Unspent(_ context.Context, id string, vout uint32, tip rr.Tip) (bool, error) {
	if tip != f.tip {
		return false, errors.NewError("fixture tip mismatch")
	}
	return id == f.parent.TxID() && vout == 1, nil
}

type replayAssemblyAdapter struct{ b *BlockAssembler }

func (a replayAssemblyAdapter) State(ctx context.Context) (rr.AssemblyState, error) {
	return a.b.RecoveryState(ctx)
}
func (a replayAssemblyAdapter) Reset(ctx context.Context) (rr.AssemblyState, error) {
	return a.b.RecoveryReset(ctx)
}
func (a replayAssemblyAdapter) Transactions(ctx context.Context, visit func(string) error) (rr.AssemblyState, error) {
	return a.b.RecoveryTransactions(ctx, false, visit)
}
func (a replayAssemblyAdapter) Candidate(ctx context.Context, visit func(string) error) (rr.AssemblyState, error) {
	return a.b.RecoveryTransactions(ctx, true, visit)
}

func replayFixtureBlock(t *testing.T, previous *model.BlockHeader, coinbase *bt.Tx, txs ...*bt.Tx) *model.Block {
	t.Helper()
	leaves := []chainhash.Hash{*coinbase.TxIDChainHash()}
	for _, tx := range txs {
		leaves = append(leaves, *tx.TxIDChainHash())
	}
	for len(leaves) > 1 {
		if len(leaves)%2 != 0 {
			leaves = append(leaves, leaves[len(leaves)-1])
		}
		next := make([]chainhash.Hash, 0, len(leaves)/2)
		for i := 0; i < len(leaves); i += 2 {
			raw := append(leaves[i].CloneBytes(), leaves[i+1][:]...)
			next = append(next, chainhash.DoubleHashH(raw))
		}
		leaves = next
	}
	header := &model.BlockHeader{Version: 1, HashPrevBlock: previous.Hash(), HashMerkleRoot: &leaves[0], Timestamp: previous.Timestamp + 600, Bits: previous.Bits, Nonce: 1}
	return &model.Block{Header: header, CoinbaseTx: coinbase, TransactionCount: uint64(len(txs) + 1), Subtrees: []*chainhash.Hash{}}
}

func TestReplayRecoveryAerospikeAssemblyEndToEnd(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		t.Run(fmt.Sprintf("paginated=%v", paginated), func(t *testing.T) { runReplayRecoveryAerospikeAssemblyEndToEnd(t, paginated) })
	}
}
func runReplayRecoveryAerospikeAssemblyEndToEnd(t *testing.T, paginated bool) {
	if testing.Short() {
		t.Skip("requires real Aerospike")
	}
	initPrometheusMetrics()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	container, err := aeroTest.RunContainer(ctx, aeroTest.WithTTLSupport("test"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.ServicePort(ctx)
	require.NoError(t, err)
	settings := createTestSettings(t)
	if paginated {
		settings.UtxoStore.UtxoBatchSize = 2
	}
	settings.UtxoStore.DisableDAHCleaner = true
	settings.BlockAssembly.UnminedTxDiskSortEnabled = false
	settings.BlockAssembly.OnRestartValidateParentChain = false
	logger := ulogger.NewErrorTestLogger(t)
	target, err := url.Parse(fmt.Sprintf("aerospike://%s:%d/test?set=recovery_e2e&externalStore=memory://", host, port))
	require.NoError(t, err)
	store, err := astore.New(ctx, logger, settings, target)
	require.NoError(t, err)
	store.SetExternalStore(memory.New())
	require.NoError(t, store.SetBlockHeight(2))
	nativeBackend, err := astore.NewRecoveryBackend(store.GetClient().Client, "test", "recovery_e2e", settings.UtxoStore.UtxoBatchSize, target.Host)
	require.NoError(t, err)
	chainURL, err := url.Parse("sqlitememory://")
	require.NoError(t, err)
	chainStore, err := blockchainstore.NewStore(logger, chainURL, settings)
	require.NoError(t, err)
	chain, err := blockchain.NewLocalClient(logger, settings, chainStore, nil, nil)
	require.NoError(t, err)
	genesis, err := chainStore.GetBlockByID(ctx, 0)
	require.NoError(t, err)
	parent := bt.NewTx()
	require.NoError(t, parent.From("1111111111111111111111111111111111111111111111111111111111111111", 0, "51", 10000))
	for i := 0; i < 2; i++ {
		require.NoError(t, parent.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 4000))
	}
	_, err = store.Create(ctx, parent, 1)
	require.NoError(t, err)
	makeChild := func(parent *bt.Tx, vout uint32, amount uint64) *bt.Tx {
		tx := bt.NewTx()
		require.NoError(t, tx.From(parent.TxID(), vout, parent.Outputs[vout].LockingScript.String(), parent.Outputs[vout].Satoshis))
		require.NoError(t, tx.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", amount))
		return tx
	}
	amount := uint64(3000)
	if paginated {
		amount = 1000
	}
	child := makeChild(parent, 0, amount)
	if paginated {
		for i := 0; i < 2; i++ {
			require.NoError(t, child.PayToAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", 1000))
		}
	}
	_, _, err = store.SpendAndCreate(ctx, child, 1)
	require.NoError(t, err)
	grandchild := makeChild(child, 0, 2000)
	if paginated {
		for vout := uint32(1); vout < 3; vout++ {
			require.NoError(t, grandchild.From(child.TxID(), vout, child.Outputs[vout].LockingScript.String(), child.Outputs[vout].Satoshis))
		}
	}
	_, _, err = store.SpendAndCreate(ctx, grandchild, 2)
	require.NoError(t, err)
	cb1, err := bt.NewTxFromBytes(genesis.CoinbaseTx.Bytes())
	require.NoError(t, err)
	cb1.LockTime = 1
	block1 := replayFixtureBlock(t, genesis.Header, cb1, parent, child)
	cb2, err := bt.NewTxFromBytes(genesis.CoinbaseTx.Bytes())
	require.NoError(t, err)
	cb2.LockTime = 2
	block2 := replayFixtureBlock(t, block1.Header, cb2, grandchild)
	for i, block := range []*model.Block{block1, block2} {
		require.NoError(t, chain.AddBlock(ctx, block, "", blockchainoptions.WithMinedSet(true)))
		require.NoError(t, chain.SetBlockProcessedAt(ctx, block.Hash()))
		height := uint32(i + 1)
		_, err = store.Create(ctx, block.CoinbaseTx, height)
		require.NoError(t, err)
		_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{block.CoinbaseTx.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: height, BlockHeight: height, OnLongestChain: true})
		require.NoError(t, err)
	}
	_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{parent.TxIDChainHash(), child.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: 1, BlockHeight: 1, OnLongestChain: true})
	require.NoError(t, err)
	_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{grandchild.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: 2, BlockHeight: 2, OnLongestChain: true})
	require.NoError(t, err)
	for page := 0; page <= (len(child.Outputs)-1)/settings.UtxoStore.UtxoBatchSize; page++ {
		childKey, err := as.NewKey("test", "recovery_e2e", uaerospike.CalculateKeySourceInternal(child.TxIDChainHash(), uint32(page)))
		require.NoError(t, err)
		_, err = store.GetClient().Delete(nil, childKey)
		require.NoError(t, err)
	}
	_, _, err = store.SpendAndCreate(ctx, child, 2)
	require.NoError(t, err)
	source := &replayChainFixture{tip: rr.Tip{Hash: block2.Hash().String(), Height: 2}, child: child, parent: parent, inclusion: block1.Header}
	backend := rr.NewEvidenceBackend(nativeBackend, source, source.tip, nil)
	start := func(header *model.BlockHeader, height uint32) (*BlockAssembler, func()) {
		serviceCtx, stop := context.WithCancel(ctx)
		announcements := make(chan subtreeprocessor.NewSubtreeRequest, 100)
		go func() {
			for {
				select {
				case request := <-announcements:
					if request.ErrChan != nil {
						select {
						case request.ErrChan <- nil:
						case <-serviceCtx.Done():
							return
						}
					}
				case <-serviceCtx.Done():
					return
				}
			}
		}()
		assembler, err := NewBlockAssembler(serviceCtx, logger, settings, gocore.NewStat("replay-e2e"), store, memory.New(), chain, announcements)
		require.NoError(t, err)
		assembler.setBestBlockHeader(header, height)
		assembler.subtreeProcessor.InitCurrentBlockHeader(header)
		require.NoError(t, assembler.SetState(serviceCtx))
		require.NoError(t, assembler.Start(serviceCtx))
		var once sync.Once
		cleanup := func() {
			once.Do(func() { stop(); assembler.wg.Wait(); assembler.subtreeProcessor.Stop(context.Background()) })
		}
		t.Cleanup(cleanup)
		return assembler, cleanup
	}
	assembler, stop := start(block2.Header, 2)
	containsChild := func(b *BlockAssembler) bool {
		var ids []string
		_, err := b.RecoveryTransactions(ctx, false, func(id string) error { ids = append(ids, id); return nil })
		require.NoError(t, err)
		for _, id := range ids {
			if id == child.TxID() {
				return true
			}
		}
		return false
	}
	require.True(t, containsChild(assembler), "actual startup retains recreated confirmed transaction")
	for _, options := range []struct{ full, inputs bool }{{false, false}, {true, false}, {false, true}} {
		done := make(chan error, 1)
		assembler.resetCh <- resetRequest{FullReset: options.full, ValidateInputs: options.inputs, ErrCh: done}
		require.NoError(t, <-done)
		if paginated && options.inputs {
			// Existing input-only reads omit external inputs. This reset drops
			// the candidate without repairing the store; startup below reloads it.
			require.False(t, containsChild(assembler))
			_, err := nativeBackend.Snapshot(ctx, child)
			require.NoError(t, err, "input validation did not repair the external replay")
		} else {
			require.Eventually(t, func() bool { return containsChild(assembler) }, 5*time.Second, 10*time.Millisecond, "reset full=%v inputs=%v retains recreated transaction", options.full, options.inputs)
		}
	}
	stop()
	assembler, stop = start(block2.Header, 2)
	require.True(t, containsChild(assembler), "second real startup also retains replay")
	recoveryDir := filepath.Join(t.TempDir(), "recovery")
	require.NoError(t, os.Mkdir(recoveryDir, 0700))
	manifest := filepath.Join(recoveryDir, "manifest.db")
	journal := filepath.Join(recoveryDir, "journal.db")
	audit, err := rr.Discover(ctx, backend, source, replayAssemblyAdapter{assembler}, manifest, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, audit.Scanned)
	require.EqualValues(t, 1, audit.FullySpent)
	stop()
	_, err = rr.Apply(ctx, backend, source, manifest, journal, rr.ApplyOptions{Maintenance: true, Tip: source.tip})
	require.NoError(t, err)
	assembler, stop = start(block2.Header, 2)
	_, err = rr.Verify(ctx, backend, source, replayAssemblyAdapter{assembler}, manifest, journal, true)
	require.ErrorIs(t, err, rr.ErrPending)
	require.False(t, containsChild(assembler))
	stop()
	// A later canonical tip and a fresh legitimate candidate prove verification is
	// not relying on a reset acknowledgement or empty candidate alone.
	cb3, err := bt.NewTxFromBytes(genesis.CoinbaseTx.Bytes())
	require.NoError(t, err)
	cb3.LockTime = 3
	block3 := replayFixtureBlock(t, block2.Header, cb3)
	require.NoError(t, chain.AddBlock(ctx, block3, "", blockchainoptions.WithMinedSet(true)))
	require.NoError(t, chain.SetBlockProcessedAt(ctx, block3.Hash()))
	_, err = store.Create(ctx, cb3, 3)
	require.NoError(t, err)
	_, err = store.SetMinedMulti(ctx, []*chainhash.Hash{cb3.TxIDChainHash()}, utxo.MinedBlockInfo{BlockID: 3, BlockHeight: 3, OnLongestChain: true})
	require.NoError(t, err)
	source.tip = rr.Tip{Hash: block3.Hash().String(), Height: 3}
	backend = rr.NewEvidenceBackend(nativeBackend, source, source.tip, nil)
	require.NoError(t, store.SetBlockHeight(3))
	legitimate := makeChild(parent, 1, 2500)
	_, _, err = store.SpendAndCreate(ctx, legitimate, 3)
	require.NoError(t, err)
	assembler, stop = start(block3.Header, 3)
	defer stop()
	var candidateIDs []string
	candidate, err := assembler.RecoveryTransactions(ctx, true, func(id string) error { candidateIDs = append(candidateIDs, id); return nil })
	require.NoError(t, err)
	require.Contains(t, candidateIDs, legitimate.TxID())
	require.NotContains(t, candidateIDs, child.TxID())
	require.NotEmpty(t, candidate.CandidateID)
	verified, err := rr.Verify(ctx, backend, source, replayAssemblyAdapter{assembler}, manifest, journal, false)
	require.NoError(t, err)
	require.Equal(t, "verified", verified.Stage)
	_, _, err = store.SpendAndCreate(ctx, child, 3)
	require.ErrorContains(t, err, "invalid spend")
	if paginated {
		archived, err := store.GetTxFromExternalStore(ctx, *child.TxIDChainHash())
		require.NoError(t, err)
		require.Equal(t, child.TxID(), archived.TxID(), "repair preserves external transaction archive")
	}
	for page := 0; page <= (len(child.Outputs)-1)/settings.UtxoStore.UtxoBatchSize; page++ {
		absent, err := backend.Read(ctx, uaerospike.CalculateKeySourceInternal(child.TxIDChainHash(), uint32(page)))
		require.NoError(t, err)
		require.Nil(t, absent)
	}
}
