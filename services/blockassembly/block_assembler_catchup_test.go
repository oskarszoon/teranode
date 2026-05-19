package blockassembly

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly/subtreeprocessor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// genesisHeader fetches the genesis block header from the blockchain client that the
// block assembler is already using. Must be called before any blocks are added.
func genesisHeader(t *testing.T, items *baTestItems) *model.BlockHeader {
	t.Helper()
	h, _, err := items.blockchainClient.GetBestBlockHeader(t.Context())
	require.NoError(t, err)
	return h
}

// buildChain appends n blocks to parent and returns them in ascending order.
func buildChain(parent *model.BlockHeader, n int, startNonce uint32) []*model.BlockHeader {
	headers := make([]*model.BlockHeader, n)
	prev := parent
	for i := 0; i < n; i++ {
		h := &model.BlockHeader{
			Version:        1,
			HashPrevBlock:  prev.Hash(),
			HashMerkleRoot: &chainhash.Hash{},
			Nonce:          startNonce + uint32(i),
			Bits:           *bits,
		}
		headers[i] = h
		prev = h
	}
	return headers
}

// addChain adds a slice of headers to the test blockchain store.
func addChain(t *testing.T, items *baTestItems, headers []*model.BlockHeader) {
	t.Helper()
	for _, h := range headers {
		require.NoError(t, items.addBlock(t.Context(), h))
	}
}

// injectMockStp replaces the subtree processor on the assembler with the given mock and
// registers the lifecycle stubs that setupBlockAssemblyTest's cleanup will call (Stop).
func injectMockStp(t *testing.T, items *baTestItems, mockStp *subtreeprocessor.MockSubtreeProcessor) {
	t.Helper()
	// Stop is called by t.Cleanup registered in setupBlockAssemblyTest.
	mockStp.On("Stop", mock.Anything).Return()
	items.blockAssembler.subtreeProcessor = mockStp
}

// TestProcessNewBlockAnnouncement_CatchupVsReorg covers the routing logic introduced
// in issue #898: a gap ≥ 2 with no blocks to roll back must take the catch-up path,
// not the reorg path.
func TestProcessNewBlockAnnouncement_CatchupVsReorg(t *testing.T) {
	initPrometheusMetrics()

	// gap=0: already at tip — must return early, no MoveForwardBlock or Reorg.
	t.Run("gap=0 no-op", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		injectMockStp(t, items, mockStp)

		// BA is at genesis; blockchain tip is also genesis → gap=0.
		items.blockAssembler.setBestBlockHeader(genesis, 0)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		mockStp.AssertNotCalled(t, "MoveForwardBlock", mock.Anything)
		mockStp.AssertNotCalled(t, "Reorg", mock.Anything, mock.Anything)
	})

	// gap=1: default branch — single MoveForwardBlock, not the catch-up or reorg helpers.
	t.Run("gap=1 moving up", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		chain := buildChain(genesis, 1, 200)
		addChain(t, items, chain)

		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		mockStp.On("MoveForwardBlock", mock.Anything).Return(nil)
		injectMockStp(t, items, mockStp)

		// BA is at genesis; blockchain tip is chain[0] → gap=1 (default branch).
		items.blockAssembler.setBestBlockHeader(genesis, 0)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		mockStp.AssertCalled(t, "MoveForwardBlock", mock.Anything)
		mockStp.AssertNotCalled(t, "Reorg", mock.Anything, mock.Anything)
	})

	// gap=2, moveBack=0: pure catch-up — two MoveForwardBlock calls in order, no Reorg.
	t.Run("gap=2 catch-up moveBack=0", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		chain := buildChain(genesis, 2, 300)
		addChain(t, items, chain)

		var callOrder []string
		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		mockStp.On("MoveForwardBlock", mock.MatchedBy(func(b *model.Block) bool {
			callOrder = append(callOrder, b.Header.Hash().String())
			return true
		})).Return(nil)
		injectMockStp(t, items, mockStp)

		// BA is at genesis; tip is chain[1] → gap=2, moveBack=0.
		items.blockAssembler.setBestBlockHeader(genesis, 0)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		require.Len(t, callOrder, 2, "MoveForwardBlock must be called twice for a 2-block catch-up")
		require.Equal(t, chain[0].Hash().String(), callOrder[0], "blocks must advance in order")
		require.Equal(t, chain[1].Hash().String(), callOrder[1])
		mockStp.AssertNotCalled(t, "Reorg", mock.Anything, mock.Anything)
	})

	// large catch-up (moveBack=0, moveForward=150): must NOT trigger full reset even when
	// moveForward >= CoinbaseMaturity. This is the core regression fix for issue #898.
	t.Run("large catch-up moveBack=0 moveForward=150 no reset", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		chain := buildChain(genesis, 150, 400)
		addChain(t, items, chain)

		resetCalled := false
		callCount := 0
		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		mockStp.On("MoveForwardBlock", mock.Anything).Return(nil).Run(func(_ mock.Arguments) {
			callCount++
		})
		mockStp.On("Reset", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(subtreeprocessor.ResetResponse{}).
			Run(func(_ mock.Arguments) { resetCalled = true })
		injectMockStp(t, items, mockStp)

		items.blockAssembler.setBestBlockHeader(genesis, 0)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		require.False(t, resetCalled, "full reset must NOT fire on a catch-up regardless of block count")
		require.Equal(t, 150, callCount, "each of the 150 blocks must advance individually")
		mockStp.AssertNotCalled(t, "Reorg", mock.Anything, mock.Anything)
	})

	// Real reorg: moveBack=1, moveForward=2 — fork chain is longer so it wins on chainwork.
	// Must call Reorg, not MoveForwardBlock.
	t.Run("real reorg moveBack=1", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		// BA is parked at a1 (height 1, from genesis).
		// Fork chain: genesis → b1 → b2 (height 2) has more chainwork and becomes best.
		a1 := &model.BlockHeader{
			Version: 1, HashPrevBlock: genesis.Hash(),
			HashMerkleRoot: &chainhash.Hash{}, Nonce: 501, Bits: *bits,
		}
		b1 := &model.BlockHeader{
			Version: 1, HashPrevBlock: genesis.Hash(),
			HashMerkleRoot: &chainhash.Hash{}, Nonce: 502, Bits: *bits,
		}
		b2 := &model.BlockHeader{
			Version: 1, HashPrevBlock: b1.Hash(),
			HashMerkleRoot: &chainhash.Hash{}, Nonce: 503, Bits: *bits,
		}

		require.NoError(t, items.addBlock(t.Context(), a1))
		require.NoError(t, items.addBlock(t.Context(), b1))
		require.NoError(t, items.addBlock(t.Context(), b2)) // b2 becomes best (more chainwork)

		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		mockStp.On("Reorg", mock.Anything, mock.Anything).Return(nil)
		injectMockStp(t, items, mockStp)

		// BA is at a1 (height 1); blockchain tip is b2 (height 2).
		items.blockAssembler.setBestBlockHeader(a1, 1)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		mockStp.AssertCalled(t, "Reorg", mock.Anything, mock.Anything)
		mockStp.AssertNotCalled(t, "MoveForwardBlock", mock.Anything)
	})

	// Large reorg (moveBack >= CoinbaseMaturity, currentHeight > 1000): reset path fires.
	// Tests handleReorg directly because building 1000+ real store blocks in a unit test is
	// impractical; routing through processNewBlockAnnouncement is covered by "real reorg" above.
	// CoinbaseMaturity=1 in test settings, so a single moveBack block is enough.
	t.Run("large reorg triggers reset", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		// Fork: genesis → a1 and genesis → b1. BA is parked at a1 (height 1001 via
		// setBestBlockHeader so currentHeight > 1000). handleReorg(ctx, b1, 1) will see
		// moveBack=[a1] (len=1 >= CoinbaseMaturity=1) and trigger the large-reorg path.
		a1 := buildChain(genesis, 1, 600)[0]
		b1 := buildChain(genesis, 1, 700)[0]
		require.NoError(t, items.addBlock(t.Context(), a1))
		require.NoError(t, items.addBlock(t.Context(), b1))

		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		mockStp.On("WaitForPendingBlocks", mock.Anything).Return(nil)
		mockStp.On("Reset", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(subtreeprocessor.ResetResponse{})
		injectMockStp(t, items, mockStp)

		// Set BA height > 1000 to arm the large-reorg guard.
		items.blockAssembler.setBestBlockHeader(a1, 1001)

		err := items.blockAssembler.handleReorg(t.Context(), b1, 1)

		// The large-reorg path calls b.reset() then always returns ErrBlockAssemblyReset.
		require.True(t, errors.Is(err, errors.ErrBlockAssemblyReset),
			"large reorg must return ErrBlockAssemblyReset, got: %v", err)
	})

	// Catch-up where one MoveForwardBlock errors mid-way: must stop at the failing block
	// and not call MoveForwardBlock for any subsequent blocks.
	t.Run("catch-up mid-error stops", func(t *testing.T) {
		items := setupBlockAssemblyTest(t)
		genesis := genesisHeader(t, items)

		chain := buildChain(genesis, 3, 800)
		addChain(t, items, chain)

		callCount := 0
		mockStp := &subtreeprocessor.MockSubtreeProcessor{}
		// First call succeeds.
		mockStp.On("MoveForwardBlock", mock.MatchedBy(func(b *model.Block) bool {
			return b.Header.Hash().IsEqual(chain[0].Hash())
		})).Return(nil).Run(func(_ mock.Arguments) { callCount++ })
		// Second call (and any subsequent) errors.
		mockStp.On("MoveForwardBlock", mock.Anything).Return(errors.NewProcessingError("simulated block error")).
			Run(func(_ mock.Arguments) { callCount++ })
		injectMockStp(t, items, mockStp)

		// BA is at genesis; tip is chain[2] → 3-block catch-up.
		items.blockAssembler.setBestBlockHeader(genesis, 0)

		items.blockAssembler.processNewBlockAnnouncement(t.Context())

		// First call succeeds, second call fails, third call must never happen.
		require.Equal(t, 2, callCount, "MoveForwardBlock must stop at the first error")

		// BA tip must be aligned with the subtree processor — both at the last-good block.
		currentHeader, currentHeight := items.blockAssembler.CurrentBlock()
		require.Equal(t, chain[0].Hash().String(), currentHeader.Hash().String(), "BA tip should be the last successfully processed block")
		require.Equal(t, uint32(1), currentHeight, "BA height should be the last successfully processed height")
	})
}
