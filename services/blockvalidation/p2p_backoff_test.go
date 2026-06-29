package blockvalidation

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/jarcoal/httpmock"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// catchupPeersP2PMock is a minimal P2PClientI returning a fixed peer set; all other
// methods are no-ops. Used to drive selectBestPeersForCatchup in tests.
type catchupPeersP2PMock struct{ peers []*p2p.PeerInfo }

func (m *catchupPeersP2PMock) GetPeersForCatchup(context.Context) ([]*p2p.PeerInfo, error) {
	return m.peers, nil
}
func (m *catchupPeersP2PMock) RecordCatchupAttempt(context.Context, string) error        { return nil }
func (m *catchupPeersP2PMock) RecordCatchupSuccess(context.Context, string, int64) error { return nil }
func (m *catchupPeersP2PMock) RecordCatchupFailure(context.Context, string) error        { return nil }
func (m *catchupPeersP2PMock) RecordCatchupMalicious(context.Context, string) error      { return nil }
func (m *catchupPeersP2PMock) UpdateCatchupError(context.Context, string, string) error  { return nil }
func (m *catchupPeersP2PMock) UpdateCatchupReputation(context.Context, string, float64) error {
	return nil
}
func (m *catchupPeersP2PMock) GetPeer(context.Context, string) (*p2p.PeerInfo, error) {
	return nil, nil
}
func (m *catchupPeersP2PMock) ReportValidBlock(context.Context, string, string) error   { return nil }
func (m *catchupPeersP2PMock) ReportValidSubtree(context.Context, string, string) error { return nil }
func (m *catchupPeersP2PMock) IsPeerMalicious(context.Context, string) (bool, string, error) {
	return false, "", nil
}
func (m *catchupPeersP2PMock) IsPeerUnhealthy(context.Context, string) (bool, string, float32, error) {
	return false, "", 0, nil
}
func (m *catchupPeersP2PMock) RecordBytesDownloaded(context.Context, string, uint64) error {
	return nil
}

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
	require.Equal(t, expectedData, result)
	require.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "429 must trigger a backoff+retry, not a hard failure")
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
	require.Equal(t, target.Header.Hash().String(), got[0].Header.Hash().String())
	require.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "429 must trigger a backoff+retry")
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
	require.Equal(t, 8.0, float64(limA.Limit()))
	require.Equal(t, 8, limA.Burst())

	// Same baseURL → same limiter instance (shared bucket).
	require.Same(t, limA, suite.Server.peerFetchLimiter("http://peer-a"))

	// Different baseURL → independent bucket.
	limB := suite.Server.peerFetchLimiter("http://peer-b")
	require.NotNil(t, limB)
	require.NotSame(t, limA, limB)
}

func TestPeerFetchLimiter_DisabledWhenNonPositive(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	suite.Server.settings.BlockValidation.PerPeerFetchRate = 0
	require.Nil(t, suite.Server.peerFetchLimiter("http://peer-a"), "rate <= 0 disables the limiter (no pacing)")
}

// TestAwaitPeerFetchSlot_RateWaitErrorIsLocal proves a rate-limiter wait that can't
// complete within the context deadline surfaces as a LOCAL error (not a peer fault),
// so the reputation gate (Server.go) doesn't blame the peer for our own pacing stall.
// x/time/rate.Wait returns a plain non-context error in that case, which awaitPeerFetchSlot
// must re-wrap.
func TestAwaitPeerFetchSlot_RateWaitErrorIsLocal(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	// rate = burst = 1: the first token is free, the next needs ~1s.
	suite.Server.settings.BlockValidation.PerPeerFetchRate = 1
	require.NoError(t, suite.Server.awaitPeerFetchSlot(context.Background(), "http://peer-a"), "first token is free")

	// A deadline far shorter than the 1s refill forces rate.Wait to fail.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	err := suite.Server.awaitPeerFetchSlot(ctx, "http://peer-a")
	require.Error(t, err)
	require.True(t, errors.IsLocalError(err), "rate-wait failure must be a local error; got %T: %v", err, err)

	// The reputation gate sees the error only AFTER callers wrap it. It must still
	// classify local through the production wrap chains (single-block: ProcessingError;
	// subtree: ServiceError -> ServiceError) — a bare ErrContextCanceled would not.
	require.True(t, errors.IsLocalError(errors.NewProcessingError("failed to get block", err)),
		"must stay local when wrapped in ProcessingError (tip/block path)")
	require.True(t, errors.IsLocalError(errors.NewServiceError("outer", errors.NewServiceError("inner", err))),
		"must stay local when double-wrapped in ServiceError (subtree path)")
}

// TestSelectBestPeersForCatchup_PrunedFallback proves the catchup primary selection
// deprioritises pruned peers but falls back to them when no non-pruned peer is
// available, so an all-pruned peer set still gets an attempt instead of stranding.
func TestSelectBestPeersForCatchup_PrunedFallback(t *testing.T) {
	t.Run("prefers non-pruned", func(t *testing.T) {
		suite := NewCatchupTestSuite(t)
		defer suite.Cleanup()
		suite.Server.p2pClient = &catchupPeersP2PMock{peers: []*p2p.PeerInfo{
			mkTestPeer("full-1", "full", 100),
			mkTestPeer("pruned-1", "pruned", 100),
		}}
		peers, err := suite.Server.selectBestPeersForCatchup(context.Background(), 100)
		require.NoError(t, err)
		require.Len(t, peers, 1)
		require.Equal(t, "full", peers[0].Storage)
	})

	t.Run("falls back to pruned when no other", func(t *testing.T) {
		suite := NewCatchupTestSuite(t)
		defer suite.Cleanup()
		suite.Server.p2pClient = &catchupPeersP2PMock{peers: []*p2p.PeerInfo{
			mkTestPeer("pruned-1", "pruned", 100),
		}}
		peers, err := suite.Server.selectBestPeersForCatchup(context.Background(), 100)
		require.NoError(t, err)
		require.Len(t, peers, 1, "must fall back to the pruned peer rather than strand")
		require.Equal(t, "pruned", peers[0].Storage)
	})
}

// TestReleaseCatchupLock_LocalErrorNotBlamedOnPeer proves the catchup reputation gate
// classifies a local error (our per-peer rate-wait budget / shutdown) as local rather
// than a peer/network error — even though its message contains a URL ("http...") that
// would otherwise trip the IsNetworkError substring case and degrade a good peer.
func TestReleaseCatchupLock_LocalErrorNotBlamedOnPeer(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	blocks := testhelpers.CreateTestBlockChain(t, 1)
	cctx := &CatchupContext{
		blockUpTo: blocks[0],
		peerID:    "peer-1",
		baseURL:   "http://peer-1",
		startTime: time.Now(),
	}

	// Production-shape local error: ServiceError wrapping the rate-wait ContextCanceled
	// that embeds context.DeadlineExceeded, message carrying a URL.
	var e error = errors.NewServiceError("[catchup:fetchSubtreeFromPeer] failed to fetch subtree from http://peer-1",
		errors.NewContextCanceledError("[peerFetchLimiter] wait aborted for http://peer-1", context.DeadlineExceeded))
	require.True(t, errors.IsLocalError(e), "precondition: the crafted error must be local")

	suite.Server.releaseCatchupLock(cctx, &e)

	require.NotNil(t, suite.Server.previousCatchupAttempt)
	require.Equal(t, "local_error", suite.Server.previousCatchupAttempt.ErrorType,
		"a local rate-wait/shutdown error must classify as local_error, not network_error (which would degrade the peer)")
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
		require.NotEqual(t, "pruned", p.Storage, "pruned peers must be skipped")
	}
	require.True(t, urls["http://full-1"], "full peers stay eligible")
	require.True(t, urls["http://legacy-1"], "empty/unknown storage stays eligible (don't exclude old archival peers)")
	require.False(t, urls["http://pruned-1"], "pruned peers must be skipped")
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
		require.NotEqual(t, "http://pruned-1", p.BaseURL, "pruned peer must not be assigned subtrees")
		require.Equal(t, "http://primary", p.BaseURL, "only the (non-pruned) primary should remain")
	}
}
