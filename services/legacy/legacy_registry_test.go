package legacy

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/legacy/peer"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockLegacyPeerRegistryClient is a testify mock for blockchain.PeerRegistryClientI.
type mockLegacyPeerRegistryClient struct {
	mock.Mock
}

func (m *mockLegacyPeerRegistryClient) RegisterPeer(_ context.Context, info *blockchain.PeerInfo) error {
	args := m.Called(info)
	return args.Error(0)
}

func (m *mockLegacyPeerRegistryClient) UpdatePeerMetrics(_ context.Context, peerID string, height uint32, bytesSentDelta, bytesRecvDelta uint64, recordSuccess, recordFailure, recordMalicious bool, responseTimeMs int64) error {
	args := m.Called(peerID, height, bytesSentDelta, bytesRecvDelta, recordSuccess, recordFailure, recordMalicious, responseTimeMs)
	return args.Error(0)
}

func (m *mockLegacyPeerRegistryClient) RemovePeer(_ context.Context, peerID string) error {
	args := m.Called(peerID)
	return args.Error(0)
}

func (m *mockLegacyPeerRegistryClient) ListPeers(_ context.Context, _ *blockchain_api.TransportType, _ float64, _ uint32, _ bool) ([]*blockchain.PeerInfo, error) {
	args := m.Called()
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*blockchain.PeerInfo), args.Error(1)
}

func (m *mockLegacyPeerRegistryClient) GetPeer(_ context.Context, peerID string) (*blockchain.PeerInfo, bool, error) {
	args := m.Called(peerID)
	if args.Get(0) == nil {
		return nil, args.Bool(1), args.Error(2)
	}
	return args.Get(0).(*blockchain.PeerInfo), args.Bool(1), args.Error(2)
}

func (m *mockLegacyPeerRegistryClient) AddBanScore(_ context.Context, _ string, _ string, _ int32) (int32, bool, error) {
	return 0, false, nil
}
func (m *mockLegacyPeerRegistryClient) IsPeerBanned(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (m *mockLegacyPeerRegistryClient) ListBannedPeers(_ context.Context) ([]string, error) {
	return nil, nil
}
func (m *mockLegacyPeerRegistryClient) ClearBannedPeers(_ context.Context) error { return nil }
func (m *mockLegacyPeerRegistryClient) Close() error                             { return nil }

// makePeer creates a minimal peer.Peer with the given address for testing.
func makePeer(t *testing.T, addr string) *peer.Peer {
	t.Helper()
	p, err := peer.NewOutboundPeer(mocklogger.NewTestLogger(), &settings.Settings{}, &peer.Config{}, addr)
	require.NoError(t, err)
	return p
}

// waitForLegacyMockCalls polls until the mock has received at least n calls or 500ms elapses.
func waitForLegacyMockCalls(t *testing.T, reg *mockLegacyPeerRegistryClient, n int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(reg.Calls) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d mock calls, got %d", n, len(reg.Calls))
}

func TestLegacyPeerToRegistryInfo_WireProtocolTransport(t *testing.T) {
	p := makePeer(t, "1.2.3.4:8333")
	sp := &serverPeer{Peer: p}

	info := legacyPeerToRegistryInfo(sp)

	require.Equal(t, blockchain_api.TransportType_TRANSPORT_WIRE_PROTOCOL, info.TransportType)
	require.Equal(t, "1.2.3.4:8333", info.ID)
	require.Equal(t, "1.2.3.4:8333", info.NetworkAddress)
}

func TestLegacyPeerToRegistryInfo_HeightFromPeer(t *testing.T) {
	p := makePeer(t, "10.0.0.1:8333")
	p.UpdateLastBlockHeight(42)
	sp := &serverPeer{Peer: p}

	info := legacyPeerToRegistryInfo(sp)

	require.Equal(t, uint32(42), info.Height)
}

func TestSetCentralPeerRegistry_StoredOnServer(t *testing.T) {
	s := &server{}
	reg := &mockLegacyPeerRegistryClient{}

	s.SetCentralPeerRegistry(reg)

	require.Equal(t, reg, s.centralRegistry)
}
