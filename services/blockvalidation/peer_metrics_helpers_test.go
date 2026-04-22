package blockvalidation

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockPeerRegistry implements blockchain.PeerRegistryClientI with full mock
// support for all methods, including AddBanScore and IsPeerBanned which the
// existing mockCentralPeerRegistry in central_registry_poller_test.go stubs out.
type mockPeerRegistry struct {
	mock.Mock
}

func (m *mockPeerRegistry) RegisterPeer(_ context.Context, info *blockchain.PeerInfo) error {
	args := m.Called(info)
	return args.Error(0)
}

func (m *mockPeerRegistry) UpdatePeerMetrics(_ context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error {
	args := m.Called(peerID, height, bytesSentDelta, bytesRecvDelta, recordSuccess, recordFailure, recordMalicious, responseTimeMs)
	return args.Error(0)
}

func (m *mockPeerRegistry) RemovePeer(_ context.Context, peerID string) error {
	args := m.Called(peerID)
	return args.Error(0)
}

func (m *mockPeerRegistry) ListPeers(_ context.Context, _ *blockchain_api.TransportType, _ float64, _ uint32, _ bool) ([]*blockchain.PeerInfo, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*blockchain.PeerInfo), args.Error(1)
}

func (m *mockPeerRegistry) GetPeer(_ context.Context, peerID string) (*blockchain.PeerInfo, bool, error) {
	args := m.Called(peerID)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*blockchain.PeerInfo), args.Bool(1), args.Error(2)
}

func (m *mockPeerRegistry) AddBanScore(_ context.Context, peerID string, reason string, points int32) (int32, bool, error) {
	args := m.Called(peerID, reason, points)
	return args.Get(0).(int32), args.Bool(1), args.Error(2)
}

func (m *mockPeerRegistry) IsPeerBanned(_ context.Context, peerID string) (bool, error) {
	args := m.Called(peerID)
	return args.Bool(0), args.Error(1)
}

func (m *mockPeerRegistry) ListBannedPeers(_ context.Context) ([]string, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]string), args.Error(1)
}

func (m *mockPeerRegistry) ClearBannedPeers(_ context.Context) error {
	args := m.Called()
	return args.Error(0)
}

func (m *mockPeerRegistry) Close() error {
	args := m.Called()
	return args.Error(0)
}

// newMetricsTestServer returns a minimal Server with only the fields needed
// for the peer_metrics_helpers functions: logger and centralPeerRegistry.
func newMetricsTestServer(reg blockchain.PeerRegistryClientI) *Server {
	return &Server{
		logger:              ulogger.TestLogger{},
		centralPeerRegistry: reg,
	}
}

// ---------------------------------------------------------------------------
// Tests for reportCatchupAttempt
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportCatchupAttempt(t *testing.T) {
	t.Run("happy path calls UpdatePeerMetrics with correct args", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-1",  // peerID
			uint32(0), // height
			uint64(0), // bytesSentDelta
			uint64(0), // bytesRecvDelta
			false,     // recordSuccess
			false,     // recordFailure
			false,     // recordMalicious
			int64(0),  // responseTimeMs
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		s.reportCatchupAttempt(context.Background(), "peer-1")

		reg.AssertExpectations(t)
	})

	t.Run("nil registry does not panic", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupAttempt(context.Background(), "peer-1")
		})
	})

	t.Run("empty peerID returns early without calling registry", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		s.reportCatchupAttempt(context.Background(), "")
		// No expectations set -- any call would cause AssertExpectations to fail.
		reg.AssertExpectations(t)
	})

	t.Run("registry error is logged but not propagated", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-err", uint32(0), uint64(0), uint64(0),
			false, false, false, int64(0),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "connection refused")).Once()

		s := newMetricsTestServer(reg)
		require.NotPanics(t, func() {
			s.reportCatchupAttempt(context.Background(), "peer-err")
		})
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for reportCatchupSuccess
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportCatchupSuccess(t *testing.T) {
	t.Run("happy path passes duration in milliseconds", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		duration := 2500 * time.Millisecond
		reg.On("UpdatePeerMetrics",
			"peer-1",
			uint32(0), uint64(0), uint64(0),
			true,        // recordSuccess
			false,       // recordFailure
			false,       // recordMalicious
			int64(2500), // responseTimeMs
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		s.reportCatchupSuccess(context.Background(), "peer-1", duration)

		reg.AssertExpectations(t)
	})

	t.Run("nil registry does not panic", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupSuccess(context.Background(), "peer-1", time.Second)
		})
	})

	t.Run("empty peerID returns early", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		s.reportCatchupSuccess(context.Background(), "", 5*time.Second)
		reg.AssertExpectations(t)
	})

	t.Run("registry error is logged but not propagated", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-err", uint32(0), uint64(0), uint64(0),
			true, false, false, int64(1000),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "timeout")).Once()

		s := newMetricsTestServer(reg)
		require.NotPanics(t, func() {
			s.reportCatchupSuccess(context.Background(), "peer-err", time.Second)
		})
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for reportCatchupFailure
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportCatchupFailure(t *testing.T) {
	t.Run("happy path calls UpdatePeerMetrics with failure flag", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-1",
			uint32(0), uint64(0), uint64(0),
			false,    // recordSuccess
			true,     // recordFailure
			false,    // recordMalicious
			int64(0), // responseTimeMs
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		s.reportCatchupFailure(context.Background(), "peer-1")

		reg.AssertExpectations(t)
	})

	t.Run("nil registry does not panic", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupFailure(context.Background(), "peer-1")
		})
	})

	t.Run("empty peerID returns early", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		s.reportCatchupFailure(context.Background(), "")
		reg.AssertExpectations(t)
	})

	t.Run("registry error is logged but not propagated", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-err", uint32(0), uint64(0), uint64(0),
			false, true, false, int64(0),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "unavailable")).Once()

		s := newMetricsTestServer(reg)
		require.NotPanics(t, func() {
			s.reportCatchupFailure(context.Background(), "peer-err")
		})
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for reportCatchupError
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportCatchupError(t *testing.T) {
	t.Run("happy path logs without panic", func(t *testing.T) {
		s := newMetricsTestServer(nil) // registry not used by this function
		require.NotPanics(t, func() {
			s.reportCatchupError(context.Background(), "peer-1", "bad header")
		})
	})

	t.Run("empty peerID returns early", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupError(context.Background(), "", "some error")
		})
	})

	t.Run("empty errorMsg returns early", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupError(context.Background(), "peer-1", "")
		})
	})

	t.Run("both empty peerID and errorMsg returns early", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupError(context.Background(), "", "")
		})
	})
}

// ---------------------------------------------------------------------------
// Tests for reportCatchupMalicious
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportCatchupMalicious(t *testing.T) {
	t.Run("happy path calls UpdatePeerMetrics and AddBanScore", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-evil",
			uint32(0), uint64(0), uint64(0),
			false, false, true, // recordMalicious
			int64(0),
		).Return(nil).Once()
		reg.On("AddBanScore",
			"peer-evil",
			"catchup_malicious",
			int32(50),
		).Return(int32(50), false, nil).Once()

		s := newMetricsTestServer(reg)
		s.reportCatchupMalicious(context.Background(), "peer-evil", "invalid proof of work")

		reg.AssertExpectations(t)
	})

	t.Run("nil registry logs warning but does not panic", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		require.NotPanics(t, func() {
			s.reportCatchupMalicious(context.Background(), "peer-evil", "bad data")
		})
	})

	t.Run("empty peerID returns early without calling registry", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		s.reportCatchupMalicious(context.Background(), "", "some reason")
		reg.AssertExpectations(t)
	})

	t.Run("UpdatePeerMetrics error is logged but AddBanScore still called", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-evil", uint32(0), uint64(0), uint64(0),
			false, false, true, int64(0),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "metrics unavailable")).Once()
		reg.On("AddBanScore",
			"peer-evil", "catchup_malicious", int32(50),
		).Return(int32(50), false, nil).Once()

		s := newMetricsTestServer(reg)
		require.NotPanics(t, func() {
			s.reportCatchupMalicious(context.Background(), "peer-evil", "forged block")
		})
		reg.AssertExpectations(t)
	})

	t.Run("AddBanScore error is logged but does not propagate", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"peer-evil", uint32(0), uint64(0), uint64(0),
			false, false, true, int64(0),
		).Return(nil).Once()
		reg.On("AddBanScore",
			"peer-evil", "catchup_malicious", int32(50),
		).Return(int32(0), false, errors.New(errors.ERR_SERVICE_ERROR, "ban store full")).Once()

		s := newMetricsTestServer(reg)
		require.NotPanics(t, func() {
			s.reportCatchupMalicious(context.Background(), "peer-evil", "invalid merkle")
		})
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for isPeerMalicious
// ---------------------------------------------------------------------------

func TestPeerMetrics_IsPeerMalicious(t *testing.T) {
	t.Run("banned peer returns true", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("IsPeerBanned", "peer-banned").Return(true, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerMalicious(context.Background(), "peer-banned")
		require.True(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("not banned peer returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("IsPeerBanned", "peer-good").Return(false, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerMalicious(context.Background(), "peer-good")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns false", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		result := s.isPeerMalicious(context.Background(), "peer-1")
		require.False(t, result)
	})

	t.Run("empty peerID returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		result := s.isPeerMalicious(context.Background(), "")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("registry error returns false and logs warning", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("IsPeerBanned", "peer-err").Return(false, errors.New(errors.ERR_SERVICE_ERROR, "rpc error")).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerMalicious(context.Background(), "peer-err")
		require.False(t, result)
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for isPeerBad
// ---------------------------------------------------------------------------

func TestPeerMetrics_IsPeerBad(t *testing.T) {
	t.Run("peer with reputation below 20 returns true", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-bad").Return(&blockchain.PeerInfo{
			ID:              "peer-bad",
			ReputationScore: 10.0,
		}, true, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-bad")
		require.True(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("peer with reputation exactly 20 returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-borderline").Return(&blockchain.PeerInfo{
			ID:              "peer-borderline",
			ReputationScore: 20.0,
		}, true, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-borderline")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("peer with reputation above 20 returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-good").Return(&blockchain.PeerInfo{
			ID:              "peer-good",
			ReputationScore: 85.0,
		}, true, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-good")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("unknown peer returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-unknown").Return((*blockchain.PeerInfo)(nil), false, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-unknown")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("nil registry returns false", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		result := s.isPeerBad(context.Background(), "peer-1")
		require.False(t, result)
	})

	t.Run("empty peerID returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("registry error returns false", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-err").Return((*blockchain.PeerInfo)(nil), false, errors.New(errors.ERR_SERVICE_ERROR, "db error")).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-err")
		require.False(t, result)
		reg.AssertExpectations(t)
	})

	t.Run("peer with zero reputation returns true", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("GetPeer", "peer-zero").Return(&blockchain.PeerInfo{
			ID:              "peer-zero",
			ReputationScore: 0.0,
		}, true, nil).Once()

		s := newMetricsTestServer(reg)
		result := s.isPeerBad(context.Background(), "peer-zero")
		require.True(t, result)
		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for reportValidBlockForPeers
// ---------------------------------------------------------------------------

func TestPeerMetrics_ReportValidBlockForPeers(t *testing.T) {
	t.Run("primary peer and two contributing peers", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		// Primary peer credited
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()
		// Contributing peer A credited
		reg.On("UpdatePeerMetrics",
			"contrib-A",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()
		// Contributing peer B credited
		reg.On("UpdatePeerMetrics",
			"contrib-B",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		contributing := map[string]struct{}{
			"contrib-A": {},
			"contrib-B": {},
		}
		s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", contributing)

		reg.AssertExpectations(t)
	})

	t.Run("primary peer in contributing set is not double-credited", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		// Primary peer credited only once (from the primary path)
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()
		// Contributing peer credited
		reg.On("UpdatePeerMetrics",
			"contrib-A",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		contributing := map[string]struct{}{
			"primary-peer": {}, // same as primary -- should be skipped in the loop
			"contrib-A":    {},
		}
		s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", contributing)

		reg.AssertExpectations(t)
	})

	t.Run("nil registry does not panic", func(t *testing.T) {
		s := newMetricsTestServer(nil)
		contributing := map[string]struct{}{
			"contrib-A": {},
		}
		require.NotPanics(t, func() {
			s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", contributing)
		})
	})

	t.Run("empty primary peer still credits contributing peers", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"contrib-A",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		contributing := map[string]struct{}{
			"contrib-A": {},
		}
		s.reportValidBlockForPeers(context.Background(), "", "0000abcd", contributing)

		reg.AssertExpectations(t)
	})

	t.Run("nil contributing peers only credits primary", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", nil)

		reg.AssertExpectations(t)
	})

	t.Run("registry error on primary does not prevent contributing peers", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "primary update failed")).Once()
		reg.On("UpdatePeerMetrics",
			"contrib-A",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		contributing := map[string]struct{}{
			"contrib-A": {},
		}
		require.NotPanics(t, func() {
			s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", contributing)
		})
		reg.AssertExpectations(t)
	})

	t.Run("registry error on contributing peer is logged but not propagated", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()
		reg.On("UpdatePeerMetrics",
			"contrib-A",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(errors.New(errors.ERR_SERVICE_ERROR, "contrib update failed")).Once()

		s := newMetricsTestServer(reg)
		contributing := map[string]struct{}{
			"contrib-A": {},
		}
		require.NotPanics(t, func() {
			s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", contributing)
		})
		reg.AssertExpectations(t)
	})

	t.Run("empty contributing map only credits primary", func(t *testing.T) {
		reg := &mockPeerRegistry{}
		reg.On("UpdatePeerMetrics",
			"primary-peer",
			uint32(0), uint64(0), uint64(0),
			true, false, false, int64(0),
		).Return(nil).Once()

		s := newMetricsTestServer(reg)
		s.reportValidBlockForPeers(context.Background(), "primary-peer", "0000abcd", map[string]struct{}{})

		reg.AssertExpectations(t)
	})
}
