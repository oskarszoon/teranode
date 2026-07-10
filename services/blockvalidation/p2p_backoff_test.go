package blockvalidation

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/bsv-blockchain/teranode/stores/blob"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/jarcoal/httpmock"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// catchupPeersP2PMock is a minimal P2PClientI returning a fixed peer set; all other
// methods are no-ops. Used to drive selectBestPeersForCatchup in tests.
type catchupPeersP2PMock struct {
	peers []*p2p.PeerInfo
	err   error
	calls atomic.Int32
}

func (m *catchupPeersP2PMock) GetPeersForCatchup(context.Context) ([]*p2p.PeerInfo, error) {
	m.calls.Add(1)
	return m.peers, m.err
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

func TestCatchupPeerSnapshot_IsLazyAndLoadsOnce(t *testing.T) {
	var calls atomic.Int32
	snapshot := &catchupPeerSnapshot{
		load: func() ([]*p2p.PeerInfo, bool, error) {
			calls.Add(1)
			return []*p2p.PeerInfo{mkTestPeer("full", "full", 100)}, true, nil
		},
	}
	require.Zero(t, calls.Load())

	type snapshotResult struct {
		peers         []*p2p.PeerInfo
		primaryPruned bool
		err           error
	}
	results := make(chan snapshotResult, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peers, primaryPruned, err := snapshot.get()
			results <- snapshotResult{peers: peers, primaryPruned: primaryPruned, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		require.True(t, result.primaryPruned)
		require.Len(t, result.peers, 1)
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestCatchupPeerSnapshot_ReportsCachedErrorOnce(t *testing.T) {
	var loads atomic.Int32
	var reports atomic.Int32
	snapshot := &catchupPeerSnapshot{
		load: func() ([]*p2p.PeerInfo, bool, error) {
			loads.Add(1)
			return nil, false, errors.NewServiceError("registry unavailable")
		},
		onError: func(error) { reports.Add(1) },
	}

	for range 3 {
		_, _, err := snapshot.get()
		require.Error(t, err)
	}
	require.Equal(t, int32(1), loads.Load())
	require.Equal(t, int32(1), reports.Load())
}

func TestSelectAlternativePeers_CapsAndSkipsTargets(t *testing.T) {
	alt1 := mkTestPeer("alt-1", "full", 100)
	alt1Duplicate := mkTestPeer("alt-1-duplicate", "full", 100)
	alt1Duplicate.DataHubURL = alt1.DataHubURL
	emptyURL := mkTestPeer("empty", "full", 100)
	emptyURL.DataHubURL = ""

	got := selectAlternativePeers([]*p2p.PeerInfo{
		mkTestPeer("assigned-copy", "full", 100),
		alt1,
		alt1Duplicate,
		emptyURL,
		mkTestPeer("alt-2", "full", 100),
		mkTestPeer("alt-3", "full", 100),
	}, "assigned", "http://assigned-copy", 2)

	require.Len(t, got, 2)
	require.Equal(t, "http://alt-1", got[0].DataHubURL)
	require.Equal(t, "http://alt-2", got[1].DataHubURL)
}

func TestSelectAlternativePeers_DefaultsNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			got := selectAlternativePeers([]*p2p.PeerInfo{
				mkTestPeer("alt-1", "full", 100),
				mkTestPeer("alt-2", "full", 100),
				mkTestPeer("alt-3", "full", 100),
				mkTestPeer("alt-4", "full", 100),
			}, "assigned", "http://assigned", limit)
			require.Len(t, got, 3)
		})
	}
}

func TestAlternativePeerCapacity(t *testing.T) {
	tests := []struct {
		name        string
		maxAttempts int
		peerCount   int
		want        int
	}{
		{name: "huge limit", maxAttempts: math.MaxInt, peerCount: 1, want: 1},
		{name: "zero defaults to three", maxAttempts: 0, peerCount: 4, want: 3},
		{name: "negative defaults to three", maxAttempts: -1, peerCount: 4, want: 3},
		{name: "positive limit", maxAttempts: 2, peerCount: 4, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, alternativePeerCapacity(tt.maxAttempts, tt.peerCount))
		})
	}
}

func seedLocalSubtreeForPeerLookupTest(t *testing.T, server *Server, ctx context.Context) *model.Block {
	t.Helper()
	subtree, err := subtreepkg.NewIncompleteTreeByLeafCount(1)
	require.NoError(t, err)
	require.NoError(t, subtree.AddCoinbaseNode())
	subtreeBytes, err := subtree.Serialize()
	require.NoError(t, err)
	hash := subtree.RootHash()
	require.NoError(t, server.subtreeStore.Set(ctx, hash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes))
	require.NoError(t, server.subtreeStore.Set(ctx, hash[:], fileformat.FileTypeSubtreeData, []byte{0x00}))
	return &model.Block{Height: 100, Subtrees: []*chainhash.Hash{hash}}
}

func TestFetchSubtreeDataForBlock_PeerLookupMode(t *testing.T) {
	t.Run("non-parallel healthy primary stays lazy", func(t *testing.T) {
		suite := NewCatchupTestSuite(t)
		defer suite.Cleanup()
		client := &catchupPeersP2PMock{}
		suite.Server.p2pClient = client
		suite.Server.settings.BlockValidation.CatchupParallelFetchEnabled = false
		block := seedLocalSubtreeForPeerLookupTest(t, suite.Server, suite.Ctx)

		_, err := suite.Server.fetchSubtreeDataForBlock(suite.Ctx, block, "primary", "http://primary")
		require.NoError(t, err)
		require.Zero(t, client.calls.Load())
	})

	t.Run("parallel distribution loads once", func(t *testing.T) {
		suite := NewCatchupTestSuite(t)
		defer suite.Cleanup()
		client := &catchupPeersP2PMock{}
		suite.Server.p2pClient = client
		suite.Server.settings.BlockValidation.CatchupParallelFetchEnabled = true
		block := seedLocalSubtreeForPeerLookupTest(t, suite.Server, suite.Ctx)

		_, err := suite.Server.fetchSubtreeDataForBlock(suite.Ctx, block, "primary", "http://primary")
		require.NoError(t, err)
		require.Equal(t, int32(1), client.calls.Load())
	})
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

func TestFilterMaxHeightPeers_ComputesTipFromEligibleArchivalPeers(t *testing.T) {
	peers := []*p2p.PeerInfo{
		mkTestPeer("pruned-tip", "pruned", 100),
		mkTestPeer("full-two-behind", "full", 98),
		mkTestPeer("legacy-three-behind", "", 97),
	}

	got := filterMaxHeightPeers(peers, "")
	require.Len(t, got, 2)
	urls := []string{got[0].DataHubURL, got[1].DataHubURL}
	require.ElementsMatch(t, []string{"http://full-two-behind", "http://legacy-three-behind"}, urls)
}

// TestClassifyDownloadErr proves the subtree_data error classifier: a dlCtx cancel is local
// (shutdown — don't blame the peer); a dlCtx or read-error deadline is a non-local network
// timeout (peer stalled — fail over + ding); a plain read error is left for the caller to
// wrap as ProcessingError (genuine peer bad-data).
func TestClassifyDownloadErr(t *testing.T) {
	h := &chainhash.Hash{0x01}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	e := classifyDownloadErr(canceled, h, errors.NewProcessingError("some read error"))
	require.NotNil(t, e)
	require.True(t, errors.IsLocalError(e), "dlCtx cancel (shutdown) must be local; got %v", e)

	deadlined, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	e = classifyDownloadErr(deadlined, h, errors.NewProcessingError("x"))
	require.NotNil(t, e)
	require.False(t, errors.IsLocalError(e), "dlCtx deadline (peer stall) must NOT be local; got %v", e)
	require.True(t, errors.IsNetworkError(e))

	e = classifyDownloadErr(context.Background(), h, errors.NewProcessingError("read failed", context.DeadlineExceeded))
	require.NotNil(t, e)
	require.False(t, errors.IsLocalError(e), "read-error deadline (peer stall) must NOT be local; got %v", e)
	require.True(t, errors.IsNetworkError(e))

	require.Nil(t, classifyDownloadErr(context.Background(), h, errors.NewProcessingError("bad subtree data")),
		"genuine peer bad-data must fall through to the caller's ProcessingError")
}

// TestFetchAndStoreSubtreeData_StorageExistsErrorHalts proves a local storage failure on the
// existence read classifies as ErrStorageError (halts loudly) rather than a ProcessingError
// that fails over across every peer during a blob-backend outage.
func TestFetchAndStoreSubtreeData_StorageExistsErrorHalts(t *testing.T) {
	suite := NewCatchupTestSuite(t)
	defer suite.Cleanup()

	mockStore := &blob.MockStore{}
	mockStore.On("Exists", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(false, errors.NewProcessingError("blob backend down"))
	suite.Server.subtreeStore = mockStore

	block := testhelpers.CreateTestBlockChain(t, 1)[0]
	sh := &chainhash.Hash{0x02}
	err := suite.Server.fetchAndStoreSubtreeData(suite.Ctx, suite.Ctx, block, sh, nil, "peer", "http://peer")
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrStorageError), "a storage existence-read failure must classify as ErrStorageError; got %T: %v", err, err)
	require.True(t, errors.IsLocalError(err), "and therefore local (no peer blame)")
}

// TestDistributeSubtreesAcrossPeers_SkipsPrunedAlts proves pruned peers are never assigned
// subtree fetches (the caller passes only non-pruned alts via catchupAltPeers).
func TestDistributeSubtreesAcrossPeers_SkipsPrunedAlts(t *testing.T) {
	// alts is already the non-pruned set (pruned peers filtered by catchupAltPeers), so pass
	// only a non-pruned primary and empty alts — result must be all primary, no panic.
	res := DistributeSubtreesAcrossPeers(ulogger.TestLogger{}, "primary", "http://primary", false, nil, 4)
	require.Len(t, res, 4)
	for _, p := range res {
		require.Equal(t, "http://primary", p.BaseURL)
	}
}

// TestDistributeSubtreesAcrossPeers_PrunedPrimaryDropped proves a pruned primary is dropped
// from the round-robin seed when a non-pruned alternative exists.
func TestDistributeSubtreesAcrossPeers_PrunedPrimaryDropped(t *testing.T) {
	alts := []*p2p.PeerInfo{mkTestPeer("full-1", "full", 100)}
	res := DistributeSubtreesAcrossPeers(ulogger.TestLogger{}, "pruned-primary", "http://pruned-primary", true, alts, 4)
	require.Len(t, res, 4)
	for _, p := range res {
		require.Equal(t, "http://full-1", p.BaseURL, "pruned primary must not be assigned; only the non-pruned alt")
	}
}

// TestDistributeSubtreesAcrossPeers_AllPrunedKeepsPrimary proves the all-pruned fallback:
// a pruned primary with no alternatives is still seeded (round-robin has a target, no div-0).
func TestDistributeSubtreesAcrossPeers_AllPrunedKeepsPrimary(t *testing.T) {
	res := DistributeSubtreesAcrossPeers(ulogger.TestLogger{}, "pruned-primary", "http://pruned-primary", true, nil, 3)
	require.Len(t, res, 3)
	for _, p := range res {
		require.Equal(t, "http://pruned-primary", p.BaseURL, "all-pruned segment must fall back to the primary, not panic")
	}
}
