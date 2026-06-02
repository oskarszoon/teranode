package sql

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

// pgFlatInt64 builds a pgtype.FlatArray for the EXPLAIN test's ANY($2) parameter.
func pgFlatInt64(vals ...int64) pgtype.FlatArray[int64] {
	arr := make(pgtype.FlatArray[int64], len(vals))
	copy(arr, vals)
	return arr
}

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

func TestMainChainBlockHashesByHeights_PostgreSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping PostgreSQL tests in short mode")
	}

	t.Run("fast path matches CTE walk", func(t *testing.T) {
		s, cleanup := setupPostgresTestStore(t)
		defer cleanup()
		waitForStartupRebuild(t, s)
		storeTestBlocks(t, s) // stores block1, block2 (heights 1, 2)
		ctx := t.Context()

		heights := []uint32{2, 1}
		res, ok, err := s.MainChainBlockHashesByHeights(ctx, block2.Hash(), 2, heights)
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, res, 2)

		for _, h := range heights {
			blk, _, err := s.GetBlockInChainByHeightHash(ctx, h, block2.Hash())
			require.NoError(t, err, "height %d", h)
			require.Equal(t, blk.Header.Hash().String(), res[h].String(), "height %d", h)
		}
	})

	t.Run("query uses idx_on_main_chain_height", func(t *testing.T) {
		s, cleanup := setupPostgresTestStore(t)
		defer cleanup()
		waitForStartupRebuild(t, s)
		storeTestBlocks(t, s)
		ctx := t.Context()

		rows, err := s.db.QueryContext(ctx, `
			EXPLAIN (FORMAT TEXT)
			SELECT height, hash FROM blocks
			WHERE on_main_chain = true AND height <= $1 AND height = ANY($2)`,
			int64(2), pgFlatInt64(2, 1))
		require.NoError(t, err)
		defer rows.Close()

		var plan strings.Builder
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		require.NoError(t, rows.Err())
		require.Contains(t, plan.String(), "idx_on_main_chain_height",
			"fast-path query must use the partial index; plan was:\n%s", plan.String())
	})

	t.Run("fork tip falls back", func(t *testing.T) {
		s, cleanup := setupPostgresTestStore(t)
		defer cleanup()
		waitForStartupRebuild(t, s)
		storeTestBlocks(t, s)
		ctx := t.Context()

		altBlock2 := createAlternativeBlock2()
		_, _, err := s.StoreBlock(ctx, altBlock2, "test_peer")
		require.NoError(t, err)

		res, ok, err := s.MainChainBlockHashesByHeights(ctx, altBlock2.Hash(), 2, []uint32{2, 1})
		require.NoError(t, err)
		require.False(t, ok)
		require.Nil(t, res)
	})
}
