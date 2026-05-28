package validator

import (
	"context"
	"net/url"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	utxofixtures "github.com/bsv-blockchain/teranode/stores/utxo/tests"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestPickMainChainHeight covers the helper that resolves which BlockHeights[i]
// corresponds to a BlockID on the current main chain.
//
// Fixes #964 and #965 hinge on this behavior.
func TestPickMainChainHeight(t *testing.T) {
	type tc struct {
		name         string
		blockIDs     []uint32
		blockHeights []uint32
		setup        func(m *blockchain.Mock)
		wantHeight   uint32
		wantFound    bool
		wantErrMsg   string
	}

	cases := []tc{
		{
			name:         "empty arrays",
			blockIDs:     []uint32{},
			blockHeights: []uint32{},
			setup:        func(m *blockchain.Mock) {},
			wantHeight:   0,
			wantFound:    false,
		},
		{
			name:         "single on main",
			blockIDs:     []uint32{5},
			blockHeights: []uint32{100},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(true, nil).Once()
			},
			wantHeight: 100,
			wantFound:  true,
		},
		{
			name:         "single orphan",
			blockIDs:     []uint32{5},
			blockHeights: []uint32{100},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(false, nil).Once()
			},
			wantHeight: 0,
			wantFound:  false,
		},
		{
			name:         "two ids idx0 orphan idx1 main",
			blockIDs:     []uint32{5, 6},
			blockHeights: []uint32{100, 101},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(false, nil).Once()
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{6}).Return(true, nil).Once()
			},
			wantHeight: 101,
			wantFound:  true,
		},
		{
			name:         "two ids idx0 main short-circuits",
			blockIDs:     []uint32{5, 6},
			blockHeights: []uint32{100, 101},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(true, nil).Once()
				// id 6 must NOT be called once 5 matches.
			},
			wantHeight: 100,
			wantFound:  true,
		},
		{
			name:         "all orphan",
			blockIDs:     []uint32{5, 6, 7},
			blockHeights: []uint32{100, 101, 102},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(false, nil).Once()
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{6}).Return(false, nil).Once()
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{7}).Return(false, nil).Once()
			},
			wantHeight: 0,
			wantFound:  false,
		},
		{
			name:         "client error propagates",
			blockIDs:     []uint32{5},
			blockHeights: []uint32{100},
			setup: func(m *blockchain.Mock) {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{5}).Return(false, errors.NewProcessingError("boom")).Once()
			},
			wantHeight: 0,
			wantFound:  false,
			wantErrMsg: "boom",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			m := &blockchain.Mock{}
			c.setup(m)

			v := &Validator{
				logger:           ulogger.TestLogger{},
				blockchainClient: m,
			}

			txMeta := &meta.Data{
				BlockIDs:     c.blockIDs,
				BlockHeights: c.blockHeights,
			}

			h, found, err := v.pickMainChainHeight(context.Background(), txMeta)

			if c.wantErrMsg != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), c.wantErrMsg)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, c.wantHeight, h)
			require.Equal(t, c.wantFound, found)
			m.AssertExpectations(t)
		})
	}
}

// TestGetUtxoBlockHeightAndExtendForParentTx_PicksMainChainHeight verifies
// the #964 fix: when a parent tx has been observed in multiple blocks
// (main + orphan), the height returned for that parent must be the main-chain
// block's height regardless of which block was inserted first.
//
// The SQL store sorts block_ids ASC on read (stores/utxo/sql/sql.go:1499),
// so what the iteration order in pickMainChainHeight exercises is determined
// by the numeric ID values, not insertion order. Cases below pin specific
// IDs to force each subtest down a distinct code path:
//   - "iterates_past_orphan_to_main": orphan ID 10 < main ID 11, so the loop
//     hits the orphan first (returns false) and then the main (returns true).
//   - "short_circuits_on_first_main": main ID 10 < orphan ID 11, so the loop
//     short-circuits on i=0; the orphan mock is intentionally NOT registered
//     so any accidental call would fail with an unexpected-call error.
func TestGetUtxoBlockHeightAndExtendForParentTx_PicksMainChainHeight(t *testing.T) {
	tracing.SetupMockTracer()

	cases := []struct {
		name           string
		mainBlockID    uint32
		orphanBlockID  uint32
		mainHeight     uint32
		orphanHeight   uint32
		expectedHeight uint32
		// registerOrphanMock controls whether the orphan-id CheckBlockIsInCurrentChain
		// expectation is registered. For the short-circuit case it stays false so the
		// mock would fail on an unexpected call to the orphan id.
		registerOrphanMock bool
	}{
		{
			name:               "iterates_past_orphan_to_main",
			orphanBlockID:      10,
			mainBlockID:        11,
			orphanHeight:       110,
			mainHeight:         111,
			expectedHeight:     111,
			registerOrphanMock: true,
		},
		{
			name:               "short_circuits_on_first_main",
			mainBlockID:        10,
			orphanBlockID:      11,
			mainHeight:         110,
			orphanHeight:       111,
			expectedHeight:     110,
			registerOrphanMock: false,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			logger := ulogger.NewErrorTestLogger(t)
			tSettings := test.CreateBaseTestSettings(t)

			storeURL, err := url.Parse("sqlitememory:///forksafe_site1_" + c.name)
			require.NoError(t, err)
			utxoStore, err := sql.New(ctx, logger, tSettings, storeURL)
			require.NoError(t, err)

			parentTx := utxofixtures.ParentTx
			_, err = utxoStore.Create(ctx, parentTx, 100)
			require.NoError(t, err)

			_, err = utxoStore.SetMinedMulti(ctx, []*chainhash.Hash{parentTx.TxIDChainHash()}, utxostore.MinedBlockInfo{
				BlockID:     c.orphanBlockID,
				BlockHeight: c.orphanHeight,
				SubtreeIdx:  0,
			})
			require.NoError(t, err)
			_, err = utxoStore.SetMinedMulti(ctx, []*chainhash.Hash{parentTx.TxIDChainHash()}, utxostore.MinedBlockInfo{
				BlockID:     c.mainBlockID,
				BlockHeight: c.mainHeight,
				SubtreeIdx:  0,
			})
			require.NoError(t, err)

			m := &blockchain.Mock{}
			// Main-chain id always queried once: either as the only iteration step
			// (short-circuit) or after the orphan returns false.
			m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{c.mainBlockID}).Return(true, nil).Once()
			if c.registerOrphanMock {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{c.orphanBlockID}).Return(false, nil).Once()
			}

			v := &Validator{
				logger:           logger,
				settings:         tSettings,
				utxoStore:        utxoStore,
				blockchainClient: m,
			}

			childTx := utxofixtures.Tx

			heights, err := v.getUtxoBlockHeightsAndExtendTx(ctx, childTx, childTx.TxIDChainHash().String(), &Options{})
			require.NoError(t, err)
			require.NotEmpty(t, heights)
			for i, h := range heights {
				require.Equal(t, c.expectedHeight, h, "input %d: expected main-chain height %d, got %d", i, c.expectedHeight, h)
			}
			m.AssertExpectations(t)
		})
	}
}

// TestGetUtxoBlockHeightAndExtendForParentTx_AllOrphanFallback verifies the
// #964 fallback: when every BlockID is off main chain, the height returned is
// blockState.Height+1 (consistent with len(BlockHeights)==0 branch).
func TestGetUtxoBlockHeightAndExtendForParentTx_AllOrphanFallback(t *testing.T) {
	tracing.SetupMockTracer()

	ctx := context.Background()
	logger := ulogger.NewErrorTestLogger(t)
	tSettings := test.CreateBaseTestSettings(t)

	storeURL, err := url.Parse("sqlitememory:///forksafe_site1_orphan_only")
	require.NoError(t, err)
	utxoStore, err := sql.New(ctx, logger, tSettings, storeURL)
	require.NoError(t, err)

	parentTx := utxofixtures.ParentTx
	_, err = utxoStore.Create(ctx, parentTx, 100)
	require.NoError(t, err)

	_, err = utxoStore.SetMinedMulti(ctx, []*chainhash.Hash{parentTx.TxIDChainHash()}, utxostore.MinedBlockInfo{
		BlockID:     10,
		BlockHeight: 110,
		SubtreeIdx:  0,
	})
	require.NoError(t, err)

	m := &blockchain.Mock{}
	m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{uint32(10)}).Return(false, nil).Once()

	v := &Validator{
		logger:           logger,
		settings:         tSettings,
		utxoStore:        utxoStore,
		blockchainClient: m,
	}

	childTx := utxofixtures.Tx

	heights, err := v.getUtxoBlockHeightsAndExtendTx(ctx, childTx, childTx.TxIDChainHash().String(), &Options{})
	require.NoError(t, err)
	require.NotEmpty(t, heights)

	expected := utxoStore.GetBlockState().Height + 1
	for i, h := range heights {
		require.Equal(t, expected, h, "input %d: expected fallback %d, got %d", i, expected, h)
	}
	m.AssertExpectations(t)
}

// TestDAHEvictedBless_MainChainGated verifies the #965 fix: DAH-evicted parent
// must only be blessed if at least one BlockID is on the current main chain.
//
// Drives the bless decision directly via pickMainChainHeight (which Site 2 wraps).
// The site 2 call-site combines pickMainChainHeight's "found" with the existing
// conflicting/locked checks; this test pins the chain-check gate's behavior.
func TestDAHEvictedBless_MainChainGated(t *testing.T) {
	tracing.SetupMockTracer()

	cases := []struct {
		name        string
		blockIDs    []uint32
		heights     []uint32
		mainIDs     map[uint32]bool // id → onMainChain
		expectBless bool
	}{
		{
			name:        "single main-chain block blesses",
			blockIDs:    []uint32{11},
			heights:     []uint32{111},
			mainIDs:     map[uint32]bool{11: true},
			expectBless: true,
		},
		{
			name:        "single orphan block refuses bless",
			blockIDs:    []uint32{10},
			heights:     []uint32{110},
			mainIDs:     map[uint32]bool{10: false},
			expectBless: false,
		},
		{
			name:        "mixed orphan plus main blesses",
			blockIDs:    []uint32{10, 11},
			heights:     []uint32{110, 111},
			mainIDs:     map[uint32]bool{10: false, 11: true},
			expectBless: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()

			m := &blockchain.Mock{}
			for id, onMain := range c.mainIDs {
				m.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{id}).Return(onMain, nil).Once()
			}

			v := &Validator{
				logger:           ulogger.TestLogger{},
				blockchainClient: m,
			}

			txMeta := &meta.Data{
				BlockIDs:     c.blockIDs,
				BlockHeights: c.heights,
			}
			_, onMainChain, chainErr := v.pickMainChainHeight(ctx, txMeta)
			require.NoError(t, chainErr)
			require.Equal(t, c.expectBless, onMainChain)
			m.AssertExpectations(t)
		})
	}
}
