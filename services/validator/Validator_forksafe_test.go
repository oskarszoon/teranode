package validator

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/ulogger"
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
