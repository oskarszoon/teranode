//go:build aerospike

// Fixture generator for the issue #1379 reproduction harness. Builds a block of
// transactions that the node has never seen, over parents that are already in
// the UTXO store as mined.
//
// The generator gives exact control over the in-block dependency level
// histogram, which the chain generator in check_block_subtrees_large_test.go
// cannot: that one builds long chains per worker, so its max dependency depth
// equals its chain length. The measured mainnet shape is the opposite — one wide
// level plus a long thin tail — and level count is one of the bisect axes, so it
// has to be a parameter rather than a side effect.
package subtreevalidation

import (
	"context"
	"encoding/binary"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/bscript"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-bt/v2/unlocker"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// seedSatoshis funds every chain. Outputs carry the full input value (zero fee),
// matching the existing generator in check_block_subtrees_large_test.go:566 —
// processTransactionsInLevels sets WithSkipPolicyChecks(true), so no min-fee
// policy applies and a zero fee keeps the satoshi arithmetic constant down a
// 25-level chain.
const seedSatoshis = uint64(100_000)

// unseenFixtureConfig parameterises the fixture. Each field is a bisect axis
// from plans/1379-unseen-tx-throughput-design.md.
type unseenFixtureConfig struct {
	// levelSizes[k] is the number of txs whose deepest in-block ancestor chain is
	// k long. levelSizes[0] txs spend seeded (already-stored, mined) parents;
	// levelSizes[k] txs spend an output of a level k-1 tx.
	levelSizes []int

	// parentFillerBytes adds an OP_RETURN of this size to each seeded parent. Set
	// above ~32 KB to push the parent past MaxTxSizeInStoreInBytes
	// (stores/utxo/aerospike/aerospike.go:92) so it is stored externally, which is
	// what exercises the serial GetTxFromExternalStore loop in BatchDecorate
	// (stores/utxo/aerospike/get.go:697-742).
	parentFillerBytes int

	// inputsPerTx is how many outputs each generated tx spends. Input 0 always
	// comes from the tx's 1:1 parent-level transaction, which is what fixes the
	// level histogram; any additional inputs come from extra freshly-seeded mined
	// parents. So raising this multiplies the distinct-parent fan-out per level —
	// the work prefetchLevelParents does and the per-parent reads the validator
	// makes — without perturbing the level shape. Defaults to 1.
	inputsPerTx int

	// extendedInSubtreeData controls the serialization form of the txs written
	// into FileTypeSubtreeData. Default (false) writes NON-extended txs, which is
	// what the legacy path actually carries: a block from an SV node contains raw
	// transactions with no parent satoshis or scripts.
	//
	// This is not cosmetic, it selects an entirely different code path. An extended
	// tx makes prefetchLevelParents skip fields.Tx (check_block_subtrees.go:1293
	// only sets needTx when some tx is NOT extended), which makes
	// needsFullExternalTx false in BatchDecorate (get.go:722-742), which means
	// externally-stored parents are never fetched at all — and the validator
	// likewise skips its parent read (Validator.go:765). Writing extended txs here
	// silently measures the cheap path.
	extendedInSubtreeData bool

	// conflictingTxs is how many level-0 block transactions are genuine
	// double-spends: before the block tx is built, a different "squatter" tx is
	// created in the store and already spends the same outpoint. The block tx's
	// spend then fails with ErrSpent, and because processTransactionsInLevels sets
	// WithCreateConflicting(true) the validator takes the conflicting-create path
	// and blessMissingTransaction follows up with
	// checkCounterConflictingOnCurrentChain (SubtreeValidation.go:447-451).
	//
	// This is the only block-content marker that separated the slow mainnet blocks
	// from fast ones (189 and 13 conflicting warnings vs 0), and it was entirely
	// absent from the fixture.
	conflictingTxs int

	// conflictChainDepth is how many unconfirmed descendants each squatter has.
	// The squatter's chain is what GetConflictingChildren walks and what
	// MarkConflictingRecursively would BFS over, so this is the cost knob that
	// scales with descendant depth rather than with block size.
	conflictChainDepth int

	// txsPerSubtree splits the block across subtrees. The default TxBatchSize is
	// 1048576 (settings/settings.go:616), so for fixtures of this size all
	// subtrees land in a single batch — same as mainnet.
	txsPerSubtree int
}

// mainnet959979Shape is the measured dependency-level histogram of mainnet block
// 959979, the smaller of the two slow blocks in issue #1379: 6,258 txs across 25
// levels, 3,323 of them with an in-block parent.
//
// Levels 0-5 (2936, 394, 262, 228, 194, 186) are measured values from the issue.
// The 19-level tail is SYNTHESISED to make the total come to 6,258 — the issue
// reports only the head of the histogram plus a median of 119 txs/level. The
// synthesised tail is flat, which puts the histogram median at 108 rather than
// the reported 119. That is a known, deliberate approximation: level *width*
// below ~2048 does not change the per-level concurrency (the cap is
// SpendBatcherSize*2 = 2048, check_block_subtrees.go:1156), so a flat tail and a
// jittered one exercise the same code path.
func mainnet959979Shape() []int {
	head := []int{2936, 394, 262, 228, 194, 186}

	const total = 6258
	const tailLevels = 19

	used := 0
	for _, n := range head {
		used += n
	}

	remaining := total - used
	sizes := append([]int(nil), head...)

	base := remaining / tailLevels
	extra := remaining % tailLevels

	for i := 0; i < tailLevels; i++ {
		n := base
		if i < extra {
			n++
		}

		sizes = append(sizes, n)
	}

	return sizes
}

// unseenFixture is the generated block plus the facts a test needs to assert the
// fixture is actually what it claims to be.
type unseenFixture struct {
	blockBytes    []byte
	subtreeHashes []*chainhash.Hash
	txs           []*bt.Tx
	txCount       int
	seededParents int
}

// generateUnseenFixture builds the fixture and seeds ONLY the parents into the
// UTXO store.
//
// The seeded parents are created with mined block info so their BlockHeights is
// non-empty — the state a parent confirmed in an earlier block is in. Without
// that, the validator stamps the unconfirmedParentHeight sentinel and the run
// exercises the legacy unconfirmed-parent path instead of the steady-state one.
//
// The block's own transactions are never written to the store or the txMeta
// cache. That is the unseen precondition the whole reproduction depends on, and
// TestUnseenTxThroughput asserts it held rather than trusting this function.
func generateUnseenFixture(t *testing.T, h *perfHarness, cfg unseenFixtureConfig) *unseenFixture {
	t.Helper()

	require.NotEmpty(t, cfg.levelSizes, "levelSizes must describe at least one level")

	// A level cannot be wider than its parent level: each tx spends a distinct
	// output of a distinct parent-level tx, so a wider child level would have to
	// double-spend. Those would be admitted as conflicting rather than failing
	// loudly, silently changing what is being measured — so reject the shape here.
	for k := 1; k < len(cfg.levelSizes); k++ {
		require.LessOrEqual(t, cfg.levelSizes[k], cfg.levelSizes[k-1],
			"level %d (%d txs) is wider than level %d (%d txs); each tx needs a distinct parent-level output, so this shape would double-spend",
			k, cfg.levelSizes[k], k-1, cfg.levelSizes[k-1])
	}

	ctx := context.Background()

	privKey, err := bec.NewPrivateKey()
	require.NoError(t, err)

	lockingScript, err := bscript.NewP2PKHFromPubKeyBytes(privKey.PubKey().Compressed())
	require.NoError(t, err)

	unlockerGetter := &unlocker.Getter{PrivateKey: privKey}

	// Level 0: one distinct seeded parent per tx. One parent per tx rather than a
	// few fan-out parents is the faithful choice — on mainnet these txs spend
	// thousands of distinct earlier outputs, and prefetchLevelParents dedups
	// distinct parents per level, so funding a whole level from a handful of
	// parents would collapse exactly the fan-out the prefetch axis is testing.
	inputsPerTx := cfg.inputsPerTx
	if inputsPerTx < 1 {
		inputsPerTx = 1
	}

	levelTxs := make([][]*bt.Tx, len(cfg.levelSizes))
	allTxs := make([]*bt.Tx, 0, sumInts(cfg.levelSizes))

	// parentIdx hands out unique synthetic outpoints across all goroutines so no
	// two seeded parents can collide (a collision would be a double spend admitted
	// as conflicting, silently changing what is measured).
	var parentIdx atomic.Int64

	// seedParent builds a mined parent and writes it to the store, returning it for
	// a child to spend.
	seedParent := func(ctx context.Context) (*bt.Tx, error) {
		idx := int(parentIdx.Add(1))

		parentTx, err := buildSeededParent(idx, lockingScript, unlockerGetter, cfg.parentFillerBytes)
		if err != nil {
			return nil, err
		}

		// Mined info makes BlockHeights non-empty: a parent confirmed in an earlier
		// block, the steady-state case. Block ID 1 is inside the set
		// perfBlockchainClient.GetBlockHeaderIDs reports, so
		// blessMissingTransaction's already-mined-on-our-chain check
		// (SubtreeValidation.go:439) sees a consistent view.
		if _, err = h.utxoStore.Create(ctx, parentTx, h.blockHeight-1,
			utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{
				BlockID:        1,
				BlockHeight:    h.blockHeight - 1,
				SubtreeIdx:     0,
				OnLongestChain: true,
			})); err != nil {
			return nil, err
		}

		return parentTx, nil
	}

	// Level 0 is built and seeded in parallel. Sequentially it is the dominant
	// setup cost: every Create waits out the store batcher's 10 ms timer
	// (utxostore_storeBatcherDurationMillis, settings.conf:1308) because a lone
	// Create never fills the 2048-deep batch, so thousands of parents would cost
	// tens of seconds. This is fixture construction, outside the measured window,
	// but there is no reason to pay it.
	levelTxs[0] = make([]*bt.Tx, cfg.levelSizes[0])

	seedGroup, seedCtx := errgroup.WithContext(ctx)
	seedGroup.SetLimit(runtime.NumCPU() * 4)

	for i := 0; i < cfg.levelSizes[0]; i++ {
		i := i

		seedGroup.Go(func() error {
			primary, err := seedParent(seedCtx)
			if err != nil {
				return err
			}

			// Make this tx a double-spend: plant a different tx that already spends
			// the same outpoint, plus its unconfirmed descendant chain.
			if i < cfg.conflictingTxs {
				if err = seedConflictingSquatter(seedCtx, h, primary, lockingScript,
					unlockerGetter, cfg.conflictChainDepth); err != nil {
					return err
				}
			}

			extras, err := seedExtraParents(seedCtx, seedParent, inputsPerTx-1)
			if err != nil {
				return err
			}

			tx, err := spendOutputs(append([]*bt.Tx{primary}, extras...), lockingScript, unlockerGetter)
			if err != nil {
				return err
			}

			levelTxs[0][i] = tx

			return nil
		})
	}

	require.NoError(t, seedGroup.Wait(), "failed to build and seed level 0")

	allTxs = append(allTxs, levelTxs[0]...)

	// Levels are sequential relative to each other (level k spends level k-1's
	// outputs) but the txs within a level are independent, so signing fans out.
	for k := 1; k < len(cfg.levelSizes); k++ {
		levelTxs[k] = make([]*bt.Tx, cfg.levelSizes[k])

		levelGroup, levelCtx := errgroup.WithContext(ctx)
		levelGroup.SetLimit(runtime.NumCPU() * 4)

		for i := 0; i < cfg.levelSizes[k]; i++ {
			i, k := i, k

			levelGroup.Go(func() error {
				// Index i into the parent level, not i modulo its width: the width check
				// above guarantees i is in range, and a modulo would silently produce
				// double spends the moment that invariant broke.
				primary := levelTxs[k-1][i]

				extras, err := seedExtraParents(levelCtx, seedParent, inputsPerTx-1)
				if err != nil {
					return err
				}

				tx, err := spendOutputs(append([]*bt.Tx{primary}, extras...), lockingScript, unlockerGetter)
				if err != nil {
					return err
				}

				levelTxs[k][i] = tx

				return nil
			})
		}

		require.NoError(t, levelGroup.Wait(), "failed to build level %d", k)

		allTxs = append(allTxs, levelTxs[k]...)
	}

	seeded := int(parentIdx.Load())

	// allTxs is in level order, which is a topological order — the same order a
	// miner would have to place them in for the block to be valid.
	blockBytes, subtreeHashes := buildUnseenBlockFromTxs(t, h, allTxs, cfg.txsPerSubtree, cfg.extendedInSubtreeData)

	return &unseenFixture{
		blockBytes:    blockBytes,
		subtreeHashes: subtreeHashes,
		txs:           allTxs,
		txCount:       len(allTxs),
		seededParents: seeded,
	}
}

// buildSeededParent constructs a parent transaction to pre-store. Its own input
// points at a synthetic outpoint that does not exist: nothing ever validates
// this tx (it is written straight to the store, not through the validator), and
// its grandparent is never consulted because the parent resolves with
// BlockHeights set. The input is still built through FromUTXOs so the tx is
// extended, which utxoStore.Create requires in order to compute fees.
//
// Returns an error rather than calling require: it runs on errgroup worker
// goroutines, and testify's FailNow is only safe on the test goroutine.
func buildSeededParent(idx int, lockingScript *bscript.Script, ug *unlocker.Getter, fillerBytes int) (*bt.Tx, error) {
	var fakePrevTxID chainhash.Hash

	binary.LittleEndian.PutUint64(fakePrevTxID[:8], uint64(idx)+1)
	// Tag the high bytes so a synthetic outpoint can never collide with a real
	// generated txid if this fixture is ever mixed with real fixtures.
	copy(fakePrevTxID[24:], []byte("1379seed"))

	tx := bt.NewTx()

	if err := tx.FromUTXOs(&bt.UTXO{
		TxIDHash:      &fakePrevTxID,
		Vout:          0,
		LockingScript: lockingScript,
		Satoshis:      seedSatoshis,
	}); err != nil {
		return nil, err
	}

	if err := tx.AddP2PKHOutputFromScript(lockingScript, seedSatoshis); err != nil {
		return nil, err
	}

	if fillerBytes > 0 {
		// Pushes extendedTxSize past MaxTxSizeInStoreInBytes so Aerospike stores
		// this parent externally. Unspendable, and never spent — only output 0 is.
		if err := tx.AddOpReturnOutput(make([]byte, fillerBytes)); err != nil {
			return nil, err
		}
	}

	if err := tx.FillAllInputs(context.Background(), ug); err != nil {
		return nil, err
	}

	return tx, nil
}

// seedConflictingSquatter creates a transaction that already spends parent's
// output 0 and records that spend in the store, so a later block transaction
// spending the same outpoint conflicts.
//
// The squatter is given TWO outputs while the block transaction has one, so the two
// differ and therefore have different txids while spending the identical outpoint —
// without that they would be the same transaction and there would be no conflict at
// all.
//
// The squatter and its descendants are created WITHOUT mined info, i.e. unconfirmed.
// That matters: checkCounterConflictingOnCurrentChain only rejects the block
// transaction when a counter-conflicting tx is mined on the current chain
// (process_conflicting.go, BlockIDs check), so leaving them unconfirmed keeps the
// block valid and exercises the conflict-handling cost rather than a rejection.
func seedConflictingSquatter(ctx context.Context, h *perfHarness, parent *bt.Tx,
	lockingScript *bscript.Script, ug *unlocker.Getter, chainDepth int) error {
	half := parent.Outputs[0].Satoshis / 2

	squatter := bt.NewTx()

	if err := squatter.FromUTXOs(&bt.UTXO{
		TxIDHash:      parent.TxIDChainHash(),
		Vout:          0,
		LockingScript: parent.Outputs[0].LockingScript,
		Satoshis:      parent.Outputs[0].Satoshis,
	}); err != nil {
		return err
	}

	// Two outputs — this is what makes the squatter a different tx from the block's
	// spend of the same outpoint.
	if err := squatter.AddP2PKHOutputFromScript(lockingScript, half); err != nil {
		return err
	}

	if err := squatter.AddP2PKHOutputFromScript(lockingScript, parent.Outputs[0].Satoshis-half); err != nil {
		return err
	}

	if err := squatter.FillAllInputs(context.Background(), ug); err != nil {
		return err
	}

	// Unconfirmed in the store, and holding the spend on parent:0.
	if _, err := h.utxoStore.Create(ctx, squatter, h.blockHeight-1); err != nil {
		return err
	}

	if _, err := h.utxoStore.Spend(ctx, squatter, h.blockHeight-1); err != nil {
		return err
	}

	// Unconfirmed descendant chain, so the conflict has children to walk.
	current := squatter

	for d := 0; d < chainDepth; d++ {
		child, err := spendOutputs([]*bt.Tx{current}, lockingScript, ug)
		if err != nil {
			return err
		}

		if _, err = h.utxoStore.Create(ctx, child, h.blockHeight-1); err != nil {
			return err
		}

		if _, err = h.utxoStore.Spend(ctx, child, h.blockHeight-1); err != nil {
			return err
		}

		current = child
	}

	return nil
}

// seedExtraParents seeds n additional mined parents for a tx's non-primary
// inputs.
func seedExtraParents(ctx context.Context, seed func(context.Context) (*bt.Tx, error), n int) ([]*bt.Tx, error) {
	if n <= 0 {
		return nil, nil
	}

	out := make([]*bt.Tx, 0, n)

	for i := 0; i < n; i++ {
		parent, err := seed(ctx)
		if err != nil {
			return nil, err
		}

		out = append(out, parent)
	}

	return out, nil
}

// spendOutputs builds a tx spending output 0 of every given parent, signed for
// real so GoBDK script verification does genuine work. Zero fee: the single
// output carries the summed input value.
//
// Returns an error rather than calling require, for the same goroutine-safety
// reason as buildSeededParent.
func spendOutputs(parents []*bt.Tx, lockingScript *bscript.Script, ug *unlocker.Getter) (*bt.Tx, error) {
	tx := bt.NewTx()

	total := uint64(0)

	for _, parent := range parents {
		if err := tx.FromUTXOs(&bt.UTXO{
			TxIDHash:      parent.TxIDChainHash(),
			Vout:          0,
			LockingScript: parent.Outputs[0].LockingScript,
			Satoshis:      parent.Outputs[0].Satoshis,
		}); err != nil {
			return nil, err
		}

		total += parent.Outputs[0].Satoshis
	}

	if err := tx.AddP2PKHOutputFromScript(lockingScript, total); err != nil {
		return nil, err
	}

	if err := tx.FillAllInputs(context.Background(), ug); err != nil {
		return nil, err
	}

	return tx, nil
}

// buildUnseenBlockFromTxs writes txs into subtrees as legacy files and returns
// the serialized block referencing them.
//
// Subtrees are written as FileTypeSubtreeToCheck and FileTypeSubtreeData but
// deliberately NOT as FileTypeSubtree: CheckBlockSubtrees early-returns Blessed
// for subtrees that already exist under that type
// (check_block_subtrees.go:438-465), which would skip everything being measured.
func buildUnseenBlockFromTxs(t *testing.T, h *perfHarness, txs []*bt.Tx, txsPerSubtree int, extendedInSubtreeData bool) ([]byte, []*chainhash.Hash) {
	t.Helper()

	require.Positive(t, txsPerSubtree, "txsPerSubtree must be positive")

	ctx := context.Background()
	subtreeHashes := make([]*chainhash.Hash, 0, (len(txs)/txsPerSubtree)+1)

	for start := 0; start < len(txs); start += txsPerSubtree {
		end := subtreepkg.Min(start+txsPerSubtree, len(txs))
		chunk := txs[start:end]

		subtree, err := subtreepkg.NewTreeByLeafCount(subtreepkg.CeilPowerOfTwo(len(chunk)))
		require.NoError(t, err)

		subtreeData := subtreepkg.NewSubtreeData(subtree)

		for idx, tx := range chunk {
			require.NoError(t, subtree.AddNode(*tx.TxIDChainHash(), 1, 0))

			stored := tx

			if !extendedInSubtreeData {
				// Round-trip through standard serialization to drop the extended
				// fields. SerializeBytes emits extended form whenever IsExtended is
				// true, and the re-parsed tx carries no PreviousTxScript so it cannot
				// be. The txid is unchanged, so AddTx's hash check still passes.
				plain, err := bt.NewTxFromBytes(tx.Bytes())
				require.NoError(t, err)
				require.False(t, plain.IsExtended(), "tx written to subtree data must not be extended")

				stored = plain
			}

			require.NoError(t, subtreeData.AddTx(stored, idx))
		}

		subtreeBytes, err := subtree.Serialize()
		require.NoError(t, err)

		subtreeDataBytes, err := subtreeData.Serialize()
		require.NoError(t, err)

		root := subtree.RootHash()

		require.NoError(t, h.subtreeStore.Set(ctx, root[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes))
		require.NoError(t, h.subtreeStore.Set(ctx, root[:], fileformat.FileTypeSubtreeData, subtreeDataBytes))

		subtreeHashes = append(subtreeHashes, root)
	}

	header := &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      1,
		Bits:           model.NBit{},
		Nonce:          0,
	}

	coinbaseTx := &bt.Tx{Version: 1}

	block, err := model.NewBlock(header, coinbaseTx, subtreeHashes, uint64(len(txs)+1), 0, h.blockHeight, 0)
	require.NoError(t, err)

	blockBytes, err := block.Bytes()
	require.NoError(t, err)

	return blockBytes, subtreeHashes
}

func sumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}

	return total
}
