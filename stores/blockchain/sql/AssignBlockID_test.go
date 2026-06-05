package sql

import (
	"context"
	"net/url"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func TestAssignBlockID_IdempotentPerHash(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)

	s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()

	h := chainhash.HashH([]byte("block-A"))

	id1, err := s.AssignBlockID(ctx, &h)
	require.NoError(t, err)
	require.NotZero(t, id1)

	id2, err := s.AssignBlockID(ctx, &h)
	require.NoError(t, err)
	require.Equal(t, id1, id2, "same hash must return the same reserved id")

	h2 := chainhash.HashH([]byte("block-B"))
	id3, err := s.AssignBlockID(ctx, &h2)
	require.NoError(t, err)
	require.NotEqual(t, id1, id3)
}

func TestAssignBlockID_ConcurrentCallersConverge(t *testing.T) {
	tSettings := test.CreateBaseTestSettings(t)
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)

	s, err := New(ulogger.TestLogger{}, storeURL, tSettings)
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()
	h := chainhash.HashH([]byte("block-race"))

	const n = 16
	ids := make([]uint64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id, err := s.AssignBlockID(ctx, &h)
			require.NoError(t, err)
			ids[i] = id
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		require.Equal(t, ids[0], ids[i], "all concurrent callers for one hash must get one id")
	}
}
