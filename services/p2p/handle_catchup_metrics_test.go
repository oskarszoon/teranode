package p2p

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	terrors "github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/p2p/p2p_api"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newCatchupTestServer creates a Server with a mock registry for catchup metrics tests.
func newCatchupTestServer(t *testing.T, reg *mockPeerRegistryClient) *Server {
	t.Helper()
	s := &Server{
		gCtx:   context.Background(),
		logger: mocklogger.NewTestLogger(),
	}
	if reg != nil {
		s.centralRegistry = reg
	}
	return s
}

func TestHandleCatchupMetrics_RecordCatchupAttempt(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-1", uint32(0), uint64(0), uint64(0), false, false, false, int64(0)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.RecordCatchupAttempt(context.Background(), &p2p_api.RecordCatchupAttemptRequest{PeerId: "peer-1"})
		require.NoError(t, err)
		require.True(t, resp.Ok)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.RecordCatchupAttempt(context.Background(), &p2p_api.RecordCatchupAttemptRequest{PeerId: "peer-1"})
		require.Error(t, err)
		require.False(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_RecordCatchupSuccess(t *testing.T) {
	t.Run("success with duration", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-2", uint32(0), uint64(0), uint64(0), true, false, false, int64(1500)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.RecordCatchupSuccess(context.Background(), &p2p_api.RecordCatchupSuccessRequest{
			PeerId:     "peer-2",
			DurationMs: 1500,
		})
		require.NoError(t, err)
		require.True(t, resp.Ok)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.RecordCatchupSuccess(context.Background(), &p2p_api.RecordCatchupSuccessRequest{PeerId: "peer-2"})
		require.Error(t, err)
		require.False(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_RecordCatchupFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-3", uint32(0), uint64(0), uint64(0), false, true, false, int64(0)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.RecordCatchupFailure(context.Background(), &p2p_api.RecordCatchupFailureRequest{PeerId: "peer-3"})
		require.NoError(t, err)
		require.True(t, resp.Ok)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.RecordCatchupFailure(context.Background(), &p2p_api.RecordCatchupFailureRequest{PeerId: "peer-3"})
		require.Error(t, err)
		require.False(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_RecordCatchupMalicious(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-4", uint32(0), uint64(0), uint64(0), false, false, true, int64(0)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.RecordCatchupMalicious(context.Background(), &p2p_api.RecordCatchupMaliciousRequest{PeerId: "peer-4"})
		require.NoError(t, err)
		require.True(t, resp.Ok)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.RecordCatchupMalicious(context.Background(), &p2p_api.RecordCatchupMaliciousRequest{PeerId: "peer-4"})
		require.Error(t, err)
		require.False(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_ReportValidSubtree(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-5", uint32(0), uint64(0), uint64(0), true, false, false, int64(0)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidSubtree(context.Background(), &p2p_api.ReportValidSubtreeRequest{
			PeerId:      "peer-5",
			SubtreeHash: "aabbccdd",
		})
		require.NoError(t, err)
		require.True(t, resp.Success)
		require.Equal(t, "subtree validation recorded", resp.Message)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.ReportValidSubtree(context.Background(), &p2p_api.ReportValidSubtreeRequest{
			PeerId:      "peer-5",
			SubtreeHash: "aabbccdd",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "central registry not initialized", resp.Message)
	})

	t.Run("error from registry still returns success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-5", uint32(0), uint64(0), uint64(0), true, false, false, int64(0)).Return(terrors.NewServiceError("registry down"))
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidSubtree(context.Background(), &p2p_api.ReportValidSubtreeRequest{
			PeerId:      "peer-5",
			SubtreeHash: "aabbccdd",
		})
		// Handler logs the error but still returns success
		require.NoError(t, err)
		require.True(t, resp.Success)
		reg.AssertExpectations(t)
	})

	t.Run("empty peer ID returns validation error", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidSubtree(context.Background(), &p2p_api.ReportValidSubtreeRequest{
			PeerId:      "",
			SubtreeHash: "aabbccdd",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "peer ID is required", resp.Message)
	})

	t.Run("empty subtree hash returns validation error", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidSubtree(context.Background(), &p2p_api.ReportValidSubtreeRequest{
			PeerId:      "peer-5",
			SubtreeHash: "",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "subtree hash is required", resp.Message)
	})
}

func TestHandleCatchupMetrics_ReportValidBlock(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-6", uint32(0), uint64(0), uint64(0), true, false, false, int64(0)).Return(nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidBlock(context.Background(), &p2p_api.ReportValidBlockRequest{
			PeerId:    "peer-6",
			BlockHash: "deadbeef",
		})
		require.NoError(t, err)
		require.True(t, resp.Success)
		require.Equal(t, "block validation recorded", resp.Message)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.ReportValidBlock(context.Background(), &p2p_api.ReportValidBlockRequest{
			PeerId:    "peer-6",
			BlockHash: "deadbeef",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "central registry not initialized", resp.Message)
	})

	t.Run("error from registry still returns success", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "peer-6", uint32(0), uint64(0), uint64(0), true, false, false, int64(0)).Return(terrors.NewServiceError("connection lost"))
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidBlock(context.Background(), &p2p_api.ReportValidBlockRequest{
			PeerId:    "peer-6",
			BlockHash: "deadbeef",
		})
		require.NoError(t, err)
		require.True(t, resp.Success)
		reg.AssertExpectations(t)
	})

	t.Run("empty peer ID returns validation error", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidBlock(context.Background(), &p2p_api.ReportValidBlockRequest{
			PeerId:    "",
			BlockHash: "deadbeef",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "peer ID is required", resp.Message)
	})

	t.Run("empty block hash returns validation error", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		s := newCatchupTestServer(t, reg)

		resp, err := s.ReportValidBlock(context.Background(), &p2p_api.ReportValidBlockRequest{
			PeerId:    "peer-6",
			BlockHash: "",
		})
		require.Error(t, err)
		require.False(t, resp.Success)
		require.Equal(t, "block hash is required", resp.Message)
	})
}

func TestHandleCatchupMetrics_GetPeersForCatchup(t *testing.T) {
	t.Run("returns peers from registry", func(t *testing.T) {
		blockHash, err := chainhash.NewHashFromStr("0000000000000000000000000000000000000000000000000000000000001234")
		require.NoError(t, err)
		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:                   "peer-a",
				Height:               800000,
				BlockHash:            blockHash,
				DataHubURL:           "http://peer-a.example.com",
				ReputationScore:      85.0,
				InteractionSuccesses: 90,
				InteractionFailures:  10,
			},
			{
				ID:                   "peer-b",
				Height:               799999,
				DataHubURL:           "http://peer-b.example.com",
				ReputationScore:      60.0,
				InteractionSuccesses: 30,
				InteractionFailures:  20,
			},
		}, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.GetPeersForCatchup(context.Background(), &p2p_api.GetPeersForCatchupRequest{})
		require.NoError(t, err)
		require.Len(t, resp.Peers, 2)

		// First peer
		require.Equal(t, "peer-a", resp.Peers[0].Id)
		require.Equal(t, uint32(800000), resp.Peers[0].Height)
		require.Equal(t, blockHash.String(), resp.Peers[0].BlockHash)
		require.Equal(t, "http://peer-a.example.com", resp.Peers[0].DataHubUrl)
		require.InDelta(t, 85.0, resp.Peers[0].CatchupReputationScore, 0.01)
		require.Equal(t, int64(100), resp.Peers[0].CatchupAttempts) // 90 + 10
		require.Equal(t, int64(90), resp.Peers[0].CatchupSuccesses)
		require.Equal(t, int64(10), resp.Peers[0].CatchupFailures)

		// Second peer has nil BlockHash
		require.Equal(t, "peer-b", resp.Peers[1].Id)
		require.Equal(t, "", resp.Peers[1].BlockHash)

		reg.AssertExpectations(t)
	})

	t.Run("empty list from registry", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.GetPeersForCatchup(context.Background(), &p2p_api.GetPeersForCatchupRequest{})
		require.NoError(t, err)
		require.Empty(t, resp.Peers)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns error with empty peers", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.GetPeersForCatchup(context.Background(), &p2p_api.GetPeersForCatchupRequest{})
		require.Error(t, err)
		require.NotNil(t, resp.Peers)
		require.Empty(t, resp.Peers)
	})

	t.Run("registry error returns empty peers without error", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return(nil, terrors.NewServiceError("database timeout"))
		s := newCatchupTestServer(t, reg)

		resp, err := s.GetPeersForCatchup(context.Background(), &p2p_api.GetPeersForCatchupRequest{})
		// Handler logs the error and returns empty list without propagating the error
		require.NoError(t, err)
		require.NotNil(t, resp.Peers)
		require.Empty(t, resp.Peers)
		reg.AssertExpectations(t)
	})
}

func TestHandleCatchupMetrics_IsPeerMalicious(t *testing.T) {
	t.Run("banned peer is malicious", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("IsPeerBanned", "peer-bad").Return(true, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerMalicious(context.Background(), &p2p_api.IsPeerMaliciousRequest{PeerId: "peer-bad"})
		require.NoError(t, err)
		require.True(t, resp.IsMalicious)
		require.Equal(t, "peer is banned", resp.Reason)
		reg.AssertExpectations(t)
	})

	t.Run("not banned peer is not malicious", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("IsPeerBanned", "peer-good").Return(false, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerMalicious(context.Background(), &p2p_api.IsPeerMaliciousRequest{PeerId: "peer-good"})
		require.NoError(t, err)
		require.False(t, resp.IsMalicious)
		require.Empty(t, resp.Reason)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns not malicious", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.IsPeerMalicious(context.Background(), &p2p_api.IsPeerMaliciousRequest{PeerId: "peer-x"})
		require.NoError(t, err)
		require.False(t, resp.IsMalicious)
	})

	t.Run("empty peer ID returns not malicious", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.IsPeerMalicious(context.Background(), &p2p_api.IsPeerMaliciousRequest{PeerId: ""})
		require.NoError(t, err)
		require.False(t, resp.IsMalicious)
		require.Equal(t, "empty peer ID", resp.Reason)
	})
}

func TestHandleCatchupMetrics_IsPeerUnhealthy(t *testing.T) {
	t.Run("low reputation peer is unhealthy", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", "peer-low").Return(&blockchain.PeerInfo{
			ID:              "peer-low",
			ReputationScore: 20.0,
		}, true, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: "peer-low"})
		require.NoError(t, err)
		require.True(t, resp.IsUnhealthy)
		require.Contains(t, resp.Reason, "low reputation score")
		require.InDelta(t, 20.0, float64(resp.ReputationScore), 0.01)
		reg.AssertExpectations(t)
	})

	t.Run("healthy peer", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", "peer-ok").Return(&blockchain.PeerInfo{
			ID:                   "peer-ok",
			ReputationScore:      75.0,
			InteractionSuccesses: 80,
			InteractionFailures:  5,
		}, true, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: "peer-ok"})
		require.NoError(t, err)
		require.False(t, resp.IsUnhealthy)
		require.Empty(t, resp.Reason)
		require.InDelta(t, 75.0, float64(resp.ReputationScore), 0.01)
		reg.AssertExpectations(t)
	})

	t.Run("low success rate peer is unhealthy", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", "peer-flaky").Return(&blockchain.PeerInfo{
			ID:                   "peer-flaky",
			ReputationScore:      50.0, // above 40 threshold
			InteractionSuccesses: 3,
			InteractionFailures:  20,
		}, true, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: "peer-flaky"})
		require.NoError(t, err)
		require.True(t, resp.IsUnhealthy)
		require.Contains(t, resp.Reason, "low success rate")
		reg.AssertExpectations(t)
	})

	t.Run("unknown peer is unhealthy", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", "peer-unknown").Return((*blockchain.PeerInfo)(nil), false, nil)
		s := newCatchupTestServer(t, reg)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: "peer-unknown"})
		require.NoError(t, err)
		require.True(t, resp.IsUnhealthy)
		require.Equal(t, "unknown peer", resp.Reason)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns unhealthy", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: "peer-x"})
		require.NoError(t, err)
		require.True(t, resp.IsUnhealthy)
		require.Equal(t, "unable to determine peer health", resp.Reason)
	})

	t.Run("empty peer ID is unhealthy", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.IsPeerUnhealthy(context.Background(), &p2p_api.IsPeerUnhealthyRequest{PeerId: ""})
		require.NoError(t, err)
		require.True(t, resp.IsUnhealthy)
		require.Equal(t, "empty peer ID", resp.Reason)
	})
}

func TestHandleCatchupMetrics_UpdateCatchupReputation(t *testing.T) {
	t.Run("returns OK (stub)", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.UpdateCatchupReputation(context.Background(), &p2p_api.UpdateCatchupReputationRequest{
			PeerId: "peer-1",
			Score:  88.5,
		})
		require.NoError(t, err)
		require.True(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_UpdateCatchupError(t *testing.T) {
	t.Run("returns OK (stub)", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.UpdateCatchupError(context.Background(), &p2p_api.UpdateCatchupErrorRequest{
			PeerId:   "peer-1",
			ErrorMsg: "timeout connecting to peer",
		})
		require.NoError(t, err)
		require.True(t, resp.Ok)
	})
}

func TestHandleCatchupMetrics_ResetReputation(t *testing.T) {
	t.Run("returns not-ok (stub, not yet implemented)", func(t *testing.T) {
		s := newCatchupTestServer(t, nil)

		resp, err := s.ResetReputation(context.Background(), &p2p_api.ResetReputationRequest{
			PeerId: "peer-1",
		})
		require.NoError(t, err)
		require.False(t, resp.Ok)
		require.Equal(t, int32(0), resp.PeersReset)
	})
}

// Verify that all mock expectations use the correct argument signatures by ensuring
// unused mock expectations cause test failures.
func TestHandleCatchupMetrics_UpdatePeerMetricsErrorIsLoggedNotReturned(t *testing.T) {
	// When UpdatePeerMetrics returns an error, the handlers log it but still return Ok=true.
	// This test covers RecordCatchupAttempt as representative; other handlers behave identically.
	reg := &mockPeerRegistryClient{}
	reg.On("UpdatePeerMetrics", "peer-err", uint32(0), uint64(0), uint64(0), false, false, false, int64(0)).Return(terrors.NewServiceError("transient error"))
	s := newCatchupTestServer(t, reg)

	resp, err := s.RecordCatchupAttempt(context.Background(), &p2p_api.RecordCatchupAttemptRequest{PeerId: "peer-err"})
	require.NoError(t, err)
	require.True(t, resp.Ok)
	reg.AssertExpectations(t)
}

// Verify the mock is unused when not needed — sanity check to avoid false positives.
var _ = mock.Anything
