package p2p

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockPeerRegistryClient is a testify mock for blockchain.PeerRegistryClientI.
type mockPeerRegistryClient struct {
	mock.Mock
	callCount atomic.Int32
}

func (m *mockPeerRegistryClient) RegisterPeer(_ context.Context, info *blockchain.PeerInfo) error {
	m.callCount.Add(1)
	args := m.Called(info)
	return args.Error(0)
}

func (m *mockPeerRegistryClient) UpdatePeerMetrics(_ context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error {
	m.callCount.Add(1)
	args := m.Called(peerID, height, bytesSentDelta, bytesRecvDelta, recordSuccess, recordFailure, recordMalicious, responseTimeMs)
	return args.Error(0)
}

func (m *mockPeerRegistryClient) RemovePeer(_ context.Context, peerID string) error {
	m.callCount.Add(1)
	args := m.Called(peerID)
	return args.Error(0)
}

func (m *mockPeerRegistryClient) ListPeers(_ context.Context, _ *blockchain_api.TransportType, _ float64, _ uint32, _ bool) ([]*blockchain.PeerInfo, error) {
	m.callCount.Add(1)
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*blockchain.PeerInfo), args.Error(1)
}

func (m *mockPeerRegistryClient) GetPeer(_ context.Context, peerID string) (*blockchain.PeerInfo, bool, error) {
	m.callCount.Add(1)
	args := m.Called(peerID)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*blockchain.PeerInfo), args.Bool(1), args.Error(2)
}

func (m *mockPeerRegistryClient) AddBanScore(_ context.Context, peerID string, reason string, points int32) (int32, bool, error) {
	m.callCount.Add(1)
	args := m.Called(peerID, reason, points)
	return args.Get(0).(int32), args.Bool(1), args.Error(2)
}
func (m *mockPeerRegistryClient) IsPeerBanned(_ context.Context, peerID string) (bool, error) {
	m.callCount.Add(1)
	args := m.Called(peerID)
	return args.Bool(0), args.Error(1)
}
func (m *mockPeerRegistryClient) ListBannedPeers(_ context.Context) ([]string, error) {
	m.callCount.Add(1)
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]string), args.Error(1)
}
func (m *mockPeerRegistryClient) ClearBannedPeers(_ context.Context) error {
	m.callCount.Add(1)
	args := m.Called()
	return args.Error(0)
}
func (m *mockPeerRegistryClient) Close() error { return nil }

// newMinimalServer creates a bare-minimum Server for testing central registry operations.
func newMinimalServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		gCtx:   context.Background(),
		logger: mocklogger.NewTestLogger(),
	}
}

func TestDualWrite_AddPeer_RegistersInCentralRegistry(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("RegisterPeer", mock.MatchedBy(func(info *blockchain.PeerInfo) bool {
		return info.TransportType == blockchain_api.TransportType_TRANSPORT_HTTP && info.DataHubURL == "http://peer.example.com"
	})).Return(nil)

	s.SetCentralPeerRegistry(reg)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.addPeer(peerID, "test-client", 100, nil, "http://peer.example.com")

	// Allow goroutine to execute.
	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

func TestDualWrite_RemovePeer_RemovesFromCentralRegistry(t *testing.T) {
	s := newMinimalServer(t)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	reg := &mockPeerRegistryClient{}
	reg.On("RemovePeer", peerID.String()).Return(nil)

	s.SetCentralPeerRegistry(reg)
	s.removePeer(peerID)

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

func TestDualWrite_AddPeer_SetsHTTPTransportType(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("RegisterPeer", mock.MatchedBy(func(info *blockchain.PeerInfo) bool {
		return info.TransportType == blockchain_api.TransportType_TRANSPORT_HTTP
	})).Return(nil)

	s.SetCentralPeerRegistry(reg)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.addPeer(peerID, "test-client", 200, nil, "http://peer.example.com")

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

// waitForMockCalls polls until the mock has received at least n calls or 500ms elapses.
// This is necessary because central registry calls are fire-and-forget goroutines.
// Uses an atomic counter to avoid data races when reading the call count concurrently.
func waitForMockCalls(t *testing.T, reg *mockPeerRegistryClient, n int) {
	t.Helper()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if int(reg.callCount.Load()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d mock calls, got %d", n, int(reg.callCount.Load()))
}
