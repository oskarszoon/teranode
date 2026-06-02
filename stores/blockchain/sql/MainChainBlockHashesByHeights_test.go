package sql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMainChainBlockHashesByHeights_FastPath(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3)
	ctx := context.Background()

	res, ok, err := s.MainChainBlockHashesByHeights(ctx, block3.Hash(), 3, []uint32{3, 2, 1})
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, res, 3)
	require.Equal(t, block3.Hash().String(), res[3].String())
	require.Equal(t, block2.Hash().String(), res[2].String())
	require.Equal(t, block1.Hash().String(), res[1].String())
}

func TestMainChainBlockHashesByHeights_ForkTipFallsBack(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3, blockAlternative2)
	ctx := context.Background()

	res, ok, err := s.MainChainBlockHashesByHeights(ctx, blockAlternative2.Hash(), 2, []uint32{2, 1})
	require.NoError(t, err)
	require.False(t, ok, "a fork-tip start hash must signal fallback")
	require.Nil(t, res)
}

func TestMainChainBlockHashesByHeights_RebuildingFallsBack(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3)
	ctx := context.Background()

	s.mainChainRebuilding.Add(1)
	defer s.mainChainRebuilding.Add(-1)

	res, ok, err := s.MainChainBlockHashesByHeights(ctx, block3.Hash(), 3, []uint32{3, 2, 1})
	require.NoError(t, err)
	require.False(t, ok, "mid-rebuild must signal fallback")
	require.Nil(t, res)
}

func TestMainChainBlockHashesByHeights_MissingHeightFallsBack(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3)
	ctx := context.Background()

	res, ok, err := s.MainChainBlockHashesByHeights(ctx, block3.Hash(), 3, []uint32{3, 99})
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, res)
}

func TestMainChainBlockHashesByHeights_EmptyInputsFallBack(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3)
	ctx := context.Background()

	res, ok, err := s.MainChainBlockHashesByHeights(ctx, block3.Hash(), 3, nil)
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, res)
}

func TestMainChainBlockHashesByHeights_AgreesWithCTEWalk(t *testing.T) {
	s := newOnMainChainTestStore(t)
	storeBlocks(t, s, block1, block2, block3)
	ctx := context.Background()

	heights := []uint32{3, 2, 1}
	res, ok, err := s.MainChainBlockHashesByHeights(ctx, block3.Hash(), 3, heights)
	require.NoError(t, err)
	require.True(t, ok)

	for _, h := range heights {
		blk, _, err := s.GetBlockInChainByHeightHash(ctx, h, block3.Hash())
		require.NoError(t, err, "height %d", h)
		require.Equal(t, blk.Header.Hash().String(), res[h].String(), "fast path must match CTE walk at height %d", h)
	}
}
