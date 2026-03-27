package blockvalidation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/expiringmap"
	testutil "github.com/bsv-blockchain/teranode/util/test"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockCentralPeerRegistry implements blockchain.PeerRegistryClientI for testing.
type mockCentralPeerRegistry struct {
	mock.Mock
}

func (m *mockCentralPeerRegistry) RegisterPeer(_ context.Context, info *blockchain.PeerInfo) error {
	args := m.Called(info)
	return args.Error(0)
}

func (m *mockCentralPeerRegistry) UpdatePeerMetrics(_ context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error {
	args := m.Called(peerID, height, bytesSentDelta, bytesRecvDelta, recordSuccess, recordFailure, recordMalicious, responseTimeMs)
	return args.Error(0)
}

func (m *mockCentralPeerRegistry) RemovePeer(_ context.Context, peerID string) error {
	args := m.Called(peerID)
	return args.Error(0)
}

func (m *mockCentralPeerRegistry) ListPeers(_ context.Context, _ *blockchain_api.TransportType, _ float64, _ uint32, _ bool) ([]*blockchain.PeerInfo, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*blockchain.PeerInfo), args.Error(1)
}

func (m *mockCentralPeerRegistry) GetPeer(_ context.Context, peerID string) (*blockchain.PeerInfo, bool, error) {
	args := m.Called(peerID)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*blockchain.PeerInfo), args.Bool(1), args.Error(2)
}

func (m *mockCentralPeerRegistry) AddBanScore(_ context.Context, _ string, _ string, _ int32) (int32, bool, error) {
	return 0, false, nil
}

func (m *mockCentralPeerRegistry) IsPeerBanned(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (m *mockCentralPeerRegistry) ListBannedPeers(_ context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockCentralPeerRegistry) ClearBannedPeers(_ context.Context) error { return nil }

func (m *mockCentralPeerRegistry) Close() error { return nil }

// errFSMNotAvailable is a sentinel error returned by the mock blockchain client
// for CatchUpBlocks so that catchup() logs a warning but does not defer
// restoreFSMState (which would require additional mock expectations).
var errFSMNotAvailable = context.DeadlineExceeded

// newPollerTestServer creates a minimal Server suitable for testing the central
// registry poller logic. It includes enough infrastructure for catchup() to
// fail cleanly (returning an error) rather than panicking on nil fields.
//
// The blockchain mock is configured so that:
//   - CatchUpBlocks returns an error (avoids the restoreFSMState defer path)
//   - GetBlockExists returns an error (makes catchup fail early in fetchHeaders)
func newPollerTestServer(t *testing.T, mockRegistry *mockCentralPeerRegistry, mockUTXO *utxo.MockUtxostore) *Server {
	t.Helper()

	initPrometheusMetrics()

	tSettings := testutil.CreateBaseTestSettings(t)
	logger := ulogger.TestLogger{}

	mockBC := &blockchain.Mock{}
	// CatchUpBlocks returning an error makes the code log a warning and skip
	// the deferred restoreFSMState call. GetBlockExists returning an error
	// then causes catchup to fail cleanly via fetchHeaders.
	mockBC.On("CatchUpBlocks", mock.Anything).Return(errFSMNotAvailable).Maybe()
	mockBC.On("GetBlockExists", mock.Anything, mock.Anything).Return(false, context.DeadlineExceeded).Maybe()

	bv := &BlockValidation{
		logger:           logger,
		settings:         tSettings,
		blockchainClient: mockBC,
		blockExistsCache: expiringmap.New[chainhash.Hash, bool](2 * time.Minute),
	}

	return &Server{
		logger:                logger,
		settings:              tSettings,
		centralPeerRegistry:   mockRegistry,
		utxoStore:             mockUTXO,
		blockchainClient:      mockBC,
		blockValidation:       bv,
		catchupPeerCooldowns:  make(map[string]time.Time),
		catchupPeerFailCounts: make(map[string]int),
		isCatchingUp:          atomic.Bool{},
		catchupAttempts:       atomic.Int64{},
		catchupSuccesses:      atomic.Int64{},
		catchupStatsMu:        sync.RWMutex{},
		stats:                 gocore.NewStat("test-poller"),
	}
}

// ---------------------------------------------------------------------------
// Tests for nextCooldownForPeer
// ---------------------------------------------------------------------------

func TestCentralRegistry_NextCooldownForPeer(t *testing.T) {
	tests := []struct {
		name             string
		consecutiveCalls int
		expected         time.Duration
	}{
		{
			name:             "first failure gives 30s cooldown",
			consecutiveCalls: 1,
			expected:         30 * time.Second,
		},
		{
			name:             "second failure gives 60s cooldown",
			consecutiveCalls: 2,
			expected:         60 * time.Second,
		},
		{
			name:             "third failure gives 120s cooldown",
			consecutiveCalls: 3,
			expected:         120 * time.Second,
		},
		{
			name:             "fourth failure gives 240s cooldown",
			consecutiveCalls: 4,
			expected:         240 * time.Second,
		},
		{
			name:             "fifth failure caps at 5 minutes",
			consecutiveCalls: 5,
			expected:         5 * time.Minute,
		},
		{
			name:             "tenth failure still capped at 5 minutes",
			consecutiveCalls: 10,
			expected:         5 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				catchupPeerFailCounts: make(map[string]int),
			}

			var cooldown time.Duration
			for i := 0; i < tc.consecutiveCalls; i++ {
				cooldown = s.nextCooldownForPeer("peer-A")
			}
			require.Equal(t, tc.expected, cooldown)
		})
	}

	t.Run("independent peers have independent counters", func(t *testing.T) {
		s := &Server{
			catchupPeerFailCounts: make(map[string]int),
		}

		// Peer A fails twice -> 60s
		s.nextCooldownForPeer("peer-A")
		cooldownA := s.nextCooldownForPeer("peer-A")

		// Peer B fails once -> 30s
		cooldownB := s.nextCooldownForPeer("peer-B")

		require.Equal(t, 60*time.Second, cooldownA)
		require.Equal(t, 30*time.Second, cooldownB)
	})

	t.Run("nil map is initialised on first call", func(t *testing.T) {
		s := &Server{
			catchupPeerFailCounts: nil,
		}
		cooldown := s.nextCooldownForPeer("peer-X")
		require.Equal(t, 30*time.Second, cooldown)
		require.NotNil(t, s.catchupPeerFailCounts)
	})
}

// ---------------------------------------------------------------------------
// Tests for selectBestPeersFromCentralRegistry
// ---------------------------------------------------------------------------

func TestCentralRegistry_SelectBestPeersFromCentralRegistry(t *testing.T) {
	t.Run("returns nil when centralPeerRegistry is nil", func(t *testing.T) {
		s := &Server{centralPeerRegistry: nil}
		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 100)
		require.NoError(t, err)
		require.Nil(t, peers)
	})

	t.Run("returns peers from registry", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		hash := chainhash.HashH([]byte("block200"))
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:         "peer-high",
				Height:     200,
				DataHubURL: "http://peer-high:8090",
				Storage:    "full",
				BlockHash:  &hash,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		s := newPollerTestServer(t, reg, mockUTXO)

		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 101)
		require.NoError(t, err)
		require.Len(t, peers, 1)
		require.Equal(t, "peer-high", peers[0].ID)
		require.Equal(t, uint32(200), peers[0].Height)

		reg.AssertExpectations(t)
	})

	t.Run("sorts full nodes before pruned nodes", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		hash1 := chainhash.HashH([]byte("block1"))
		hash2 := chainhash.HashH([]byte("block2"))
		hash3 := chainhash.HashH([]byte("block3"))
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:              "pruned-high-rep",
				Height:          500,
				DataHubURL:      "http://pruned:8090",
				Storage:         "pruned",
				ReputationScore: 99.0,
				BlockHash:       &hash1,
			},
			{
				ID:              "full-low-rep",
				Height:          500,
				DataHubURL:      "http://full-low:8090",
				Storage:         "full",
				ReputationScore: 10.0,
				BlockHash:       &hash2,
			},
			{
				ID:              "full-high-rep",
				Height:          500,
				DataHubURL:      "http://full-high:8090",
				Storage:         "full",
				ReputationScore: 50.0,
				BlockHash:       &hash3,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		s := newPollerTestServer(t, reg, mockUTXO)

		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 100)
		require.NoError(t, err)
		require.Len(t, peers, 3)

		// Full nodes first, sorted by reputation descending.
		require.Equal(t, "full-high-rep", peers[0].ID)
		require.Equal(t, "full-low-rep", peers[1].ID)
		// Pruned node last.
		require.Equal(t, "pruned-high-rep", peers[2].ID)

		reg.AssertExpectations(t)
	})

	t.Run("returns empty when registry returns no peers", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		s := newPollerTestServer(t, reg, mockUTXO)

		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 100)
		require.NoError(t, err)
		require.Empty(t, peers)

		reg.AssertExpectations(t)
	})

	t.Run("skips peers with empty baseURL", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		hash := chainhash.HashH([]byte("block"))
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:        "no-url",
				Height:    500,
				Storage:   "full",
				BlockHash: &hash,
				// DataHubURL intentionally empty, TransportType defaults to HTTP.
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		s := newPollerTestServer(t, reg, mockUTXO)

		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 100)
		require.NoError(t, err)
		require.Empty(t, peers)

		reg.AssertExpectations(t)
	})

	t.Run("wire protocol peers use NetworkAddress as baseURL", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		hash := chainhash.HashH([]byte("block"))
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:             "wire-peer",
				Height:         500,
				NetworkAddress: "192.168.1.1:8333",
				TransportType:  blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL,
				Storage:        "full",
				BlockHash:      &hash,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		s := newPollerTestServer(t, reg, mockUTXO)

		peers, err := s.selectBestPeersFromCentralRegistry(context.Background(), 100)
		require.NoError(t, err)
		require.Len(t, peers, 1)
		require.Equal(t, "192.168.1.1:8333", peers[0].DataHubURL)

		reg.AssertExpectations(t)
	})
}

// ---------------------------------------------------------------------------
// Tests for pollCentralRegistry
// ---------------------------------------------------------------------------

func TestCentralRegistry_PollCentralRegistry(t *testing.T) {
	t.Run("returns false when no peers available", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100))

		s := newPollerTestServer(t, reg, mockUTXO)

		triggered := s.pollCentralRegistry(context.Background())
		require.False(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("returns true when isCatchingUp is already true", func(t *testing.T) {
		hash := chainhash.HashH([]byte("block200"))
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:         "peer-1",
				Height:     200,
				DataHubURL: "http://peer-1:8090",
				Storage:    "full",
				BlockHash:  &hash,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100))

		s := newPollerTestServer(t, reg, mockUTXO)
		s.isCatchingUp.Store(true)

		triggered := s.pollCentralRegistry(context.Background())
		require.True(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("skips peers on cooldown and returns false when all on cooldown", func(t *testing.T) {
		hash := chainhash.HashH([]byte("block200"))
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:              "peer-cooldown",
				Height:          200,
				DataHubURL:      "http://peer-cooldown:8090",
				Storage:         "full",
				ReputationScore: 90.0,
				BlockHash:       &hash,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100))

		s := newPollerTestServer(t, reg, mockUTXO)
		// Put the only peer on cooldown far in the future.
		s.catchupPeerCooldowns["peer-cooldown"] = time.Now().Add(10 * time.Minute)

		triggered := s.pollCentralRegistry(context.Background())
		require.False(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("selects non-cooldown peer when first peer is on cooldown", func(t *testing.T) {
		hash1 := chainhash.HashH([]byte("block1"))
		hash2 := chainhash.HashH([]byte("block2"))
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:              "peer-on-cooldown",
				Height:          200,
				DataHubURL:      "http://peer-on-cooldown:8090",
				Storage:         "full",
				ReputationScore: 90.0,
				BlockHash:       &hash1,
			},
			{
				ID:              "peer-available",
				Height:          200,
				DataHubURL:      "http://peer-available:8090",
				Storage:         "full",
				ReputationScore: 50.0,
				BlockHash:       &hash2,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100)).Maybe()

		// catchup() calls reportCatchupAttempt -> UpdatePeerMetrics
		reg.On("UpdatePeerMetrics",
			"peer-available", mock.Anything, mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).Return(nil).Maybe()

		// catchup() with centralPeerRegistry set calls GetPeer to resolve transport
		reg.On("GetPeer", "peer-available").Return((*blockchain.PeerInfo)(nil), false, nil).Maybe()

		s := newPollerTestServer(t, reg, mockUTXO)
		defer s.blockValidation.blockExistsCache.Stop()

		// Put the first (higher reputation) peer on cooldown.
		s.catchupPeerCooldowns["peer-on-cooldown"] = time.Now().Add(10 * time.Minute)

		// pollCentralRegistry will attempt catchup on "peer-available".
		// catchup will fail internally (GetBlockExists returns error),
		// which is expected -- we verify peer selection and cooldown behaviour.
		triggered := s.pollCentralRegistry(context.Background())
		require.True(t, triggered)

		// The available peer should now have a cooldown set due to the failure.
		cooldownUntil, hasCooldown := s.catchupPeerCooldowns["peer-available"]
		require.True(t, hasCooldown)
		require.True(t, cooldownUntil.After(time.Now()))

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("returns false when peer has nil block hash", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:         "peer-nil-hash",
				Height:     200,
				DataHubURL: "http://peer-nil:8090",
				Storage:    "full",
				BlockHash:  nil,
			},
		}, nil)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100))

		s := newPollerTestServer(t, reg, mockUTXO)

		triggered := s.pollCentralRegistry(context.Background())
		require.False(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("returns false when registry query fails", func(t *testing.T) {
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return(nil, context.DeadlineExceeded)

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100))

		s := newPollerTestServer(t, reg, mockUTXO)

		triggered := s.pollCentralRegistry(context.Background())
		require.False(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})

	t.Run("expired cooldown allows peer to be selected", func(t *testing.T) {
		hash := chainhash.HashH([]byte("block200"))
		reg := &mockCentralPeerRegistry{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:         "peer-expired-cooldown",
				Height:     200,
				DataHubURL: "http://peer-expired:8090",
				Storage:    "full",
				BlockHash:  &hash,
			},
		}, nil)

		// catchup() will report attempt and failure metrics
		reg.On("UpdatePeerMetrics",
			"peer-expired-cooldown", mock.Anything, mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		).Return(nil).Maybe()

		// catchup() will look up peer in registry
		reg.On("GetPeer", "peer-expired-cooldown").Return((*blockchain.PeerInfo)(nil), false, nil).Maybe()

		mockUTXO := &utxo.MockUtxostore{}
		mockUTXO.On("GetBlockHeight").Return(uint32(100)).Maybe()

		s := newPollerTestServer(t, reg, mockUTXO)
		defer s.blockValidation.blockExistsCache.Stop()

		// Set cooldown in the past so it is expired.
		s.catchupPeerCooldowns["peer-expired-cooldown"] = time.Now().Add(-1 * time.Minute)

		triggered := s.pollCentralRegistry(context.Background())
		// Catchup is triggered (will fail internally, but the function returns true).
		require.True(t, triggered)

		reg.AssertExpectations(t)
		mockUTXO.AssertExpectations(t)
	})
}
