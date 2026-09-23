package blockvalidation

import (
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/require"
)

func TestCatchupQueueBoundsDistinctPeerAlternatives(t *testing.T) {
	server := &Server{
		catchupCh:           make(chan processBlockCatchup, 1),
		processBlockNotify:  ttlcache.New[chainhash.Hash, bool](),
		catchupAlternatives: ttlcache.New[chainhash.Hash, []processBlockCatchup](),
	}
	block := testhelpers.CreateTestBlockChain(t, 2)[1]
	require.True(t, server.enqueueCatchup(processBlockCatchup{block: block, peerID: "primary"}))
	for i := range maxCatchupAlternatives + 10 {
		require.True(t, server.enqueueCatchup(processBlockCatchup{
			block: block, peerID: fmt.Sprintf("alternative-%d", i), baseURL: fmt.Sprintf("http://peer-%d", i),
		}))
	}
	alternatives := server.catchupAlternatives.Get(*block.Hash())
	require.NotNil(t, alternatives)
	require.Len(t, alternatives.Value(), maxCatchupAlternatives, "paused targets must not retain unbounded block copies from distinct announcements")
	for i, alternative := range alternatives.Value() {
		require.Equal(t, fmt.Sprintf("alternative-%d", i), alternative.peerID)
	}
	require.Len(t, server.catchupQueued, 1)
	require.Len(t, server.catchupCh, 1)
	require.NotNil(t, server.processBlockNotify.Get(*block.Hash()))
}
