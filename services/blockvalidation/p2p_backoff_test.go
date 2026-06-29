package blockvalidation

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/jarcoal/httpmock"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeParallelFetchP2P is a minimal P2PClientForParallelFetch for selection tests.
type fakeParallelFetchP2P struct{ peers []*p2p.PeerInfo }

func (f *fakeParallelFetchP2P) GetPeersForCatchup(context.Context) ([]*p2p.PeerInfo, error) {
	return f.peers, nil
}
func (f *fakeParallelFetchP2P) RecordBytesDownloaded(context.Context, string, uint64) error {
	return nil
}

func mkTestPeer(id, storage string, height uint32) *p2p.PeerInfo {
	return &p2p.PeerInfo{
		ID:              peer.ID(id),
		Storage:         storage,
		Height:          height,
		DataHubURL:      "http://" + id,
		ReputationScore: 100,
	}
}

// TestFetchSubtreeFromPeer_BacksOffOn429 proves the subtree fetch path retries
// (rather than failing) when a peer's asset endpoint rate-limits with HTTP 429.
// Before the fix fetchSubtreeFromPeer used the non-retrying DoHTTPRequestBounded
// and a single 429 was a hard failure — the cause of the IBD wedge in #1174.
func TestFetchSubtreeFromPeer_BacksOffOn429(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	subtreeHash := &chainhash.Hash{0x01, 0x02, 0x03}
	expectedData := []byte("mock subtree data after backoff")

	httpmock.ActivateNonDefault(util.HTTPClient())
	defer httpmock.DeactivateAndReset()

	var attempts int32
	httpmock.RegisterResponder("GET",
		fmt.Sprintf("http://test-peer/subtree/%s", subtreeHash.String()),
		func(req *http.Request) (*http.Response, error) {
			if atomic.AddInt32(&attempts, 1) < 2 {
				return httpmock.NewStringResponse(http.StatusTooManyRequests, `{"message":"rate limit exceeded"}`), nil
			}
			return httpmock.NewBytesResponse(http.StatusOK, expectedData), nil
		})

	result, err := suite.Server.fetchSubtreeFromPeer(suite.Ctx, subtreeHash, "test-peer-id", "http://test-peer")
	require.NoError(t, err)
	assert.Equal(t, expectedData, result)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "429 must trigger a backoff+retry, not a hard failure")
}

// TestFetchBlocksBatch_BacksOffOn429 proves the block-batch fetch path retries on 429.
// Before the fix fetchBlocksBatch used the non-retrying DoHTTPRequest.
func TestFetchBlocksBatch_BacksOffOn429(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 2)
	target := blocks[1]
	blockBytes, err := target.Bytes()
	require.NoError(t, err)

	httpmock.ActivateNonDefault(util.HTTPClient())
	defer httpmock.DeactivateAndReset()

	var attempts int32
	httpmock.RegisterResponder("GET",
		fmt.Sprintf("http://test-peer/blocks/%s?n=%d", target.Header.Hash().String(), 1),
		func(req *http.Request) (*http.Response, error) {
			if atomic.AddInt32(&attempts, 1) < 2 {
				return httpmock.NewStringResponse(http.StatusTooManyRequests, `{"message":"rate limit exceeded"}`), nil
			}
			return httpmock.NewBytesResponse(http.StatusOK, blockBytes), nil
		})

	got, err := suite.Server.fetchBlocksBatch(suite.Ctx, target.Header.Hash(), 1, "test-peer-id", "http://test-peer")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, target.Header.Hash().String(), got[0].Header.Hash().String())
	assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "429 must trigger a backoff+retry")
}

// TestPeerFetchLimiter_KeyedByBaseURL proves the per-peer heavy-fetch rate limiter
// is created from settings, keyed per baseURL (so one peer's limit can't throttle
// another), and disabled when the rate is non-positive.
func TestPeerFetchLimiter_KeyedByBaseURL(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	suite.Server.settings.BlockValidation.PerPeerFetchRate = 8

	limA := suite.Server.peerFetchLimiter("http://peer-a")
	require.NotNil(t, limA)
	assert.Equal(t, 8.0, float64(limA.Limit()))
	assert.Equal(t, 8, limA.Burst())

	// Same baseURL → same limiter instance (shared bucket).
	assert.Same(t, limA, suite.Server.peerFetchLimiter("http://peer-a"))

	// Different baseURL → independent bucket.
	limB := suite.Server.peerFetchLimiter("http://peer-b")
	require.NotNil(t, limB)
	assert.NotSame(t, limA, limB)
}

func TestPeerFetchLimiter_DisabledWhenNonPositive(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	suite.Server.settings.BlockValidation.PerPeerFetchRate = 0
	assert.Nil(t, suite.Server.peerFetchLimiter("http://peer-a"), "rate <= 0 disables the limiter (no pacing)")
}

// TestGetPeersAtMaxHeight_SkipsPrunedPeers proves archival-aware selection: pruned
// peers (which 404 on archival subtree data and re-wedge IBD per #1174) are excluded,
// while full and legacy/empty-storage peers remain eligible.
func TestGetPeersAtMaxHeight_SkipsPrunedPeers(t *testing.T) {
	client := &fakeParallelFetchP2P{peers: []*p2p.PeerInfo{
		mkTestPeer("full-1", "full", 100),
		mkTestPeer("pruned-1", "pruned", 100),
		mkTestPeer("legacy-1", "", 100),
	}}

	peers, err := GetPeersAtMaxHeight(context.Background(), ulogger.TestLogger{}, client, "")
	require.NoError(t, err)

	// Key on DataHubURL (raw) — peer.ID.String() base58-encodes, so don't assume it equals the id.
	urls := map[string]bool{}
	for _, p := range peers {
		urls[p.DataHubURL] = true
		assert.NotEqual(t, "pruned", p.Storage, "pruned peers must be skipped")
	}
	assert.True(t, urls["http://full-1"], "full peers stay eligible")
	assert.True(t, urls["http://legacy-1"], "empty/unknown storage stays eligible (don't exclude old archival peers)")
	assert.False(t, urls["http://pruned-1"], "pruned peers must be skipped")
}

// TestSelectAlternativePeer_SkipsPrunedPeers proves the peer-switch path won't rotate
// onto a pruned peer.
func TestSelectAlternativePeer_SkipsPrunedPeers(t *testing.T) {
	client := &fakeParallelFetchP2P{peers: []*p2p.PeerInfo{
		mkTestPeer("pruned-1", "pruned", 100),
	}}

	_, err := SelectAlternativePeer(context.Background(), ulogger.TestLogger{}, client, "current", 100)
	require.Error(t, err, "a pruned-only peer set yields no eligible alternative")
}

// TestDistributeSubtreesAcrossPeers_SkipsPrunedPeers proves pruned peers are never
// assigned subtree fetches when spreading load.
func TestDistributeSubtreesAcrossPeers_SkipsPrunedPeers(t *testing.T) {
	client := &fakeParallelFetchP2P{peers: []*p2p.PeerInfo{
		mkTestPeer("pruned-1", "pruned", 100),
	}}

	res, err := DistributeSubtreesAcrossPeers(context.Background(), ulogger.TestLogger{}, client, "primary", "http://primary", 4)
	require.NoError(t, err)
	for _, p := range res {
		assert.NotEqual(t, "http://pruned-1", p.BaseURL, "pruned peer must not be assigned subtrees")
		assert.Equal(t, "http://primary", p.BaseURL, "only the (non-pruned) primary should remain")
	}
}
