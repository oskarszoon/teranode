package p2p

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAddConnectedPeer_RegistersInCentralRegistry covers addConnectedPeer's
// dispatch through the registry-update worker (sync mode when no channel set).
func TestAddConnectedPeer_RegistersInCentralRegistry(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("RegisterPeer", mock.MatchedBy(func(info *blockchain.PeerInfo) bool {
		return info.ClientName == "client-A" && info.Height == 42
	})).Return(nil)

	s.SetCentralPeerRegistry(reg)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.addConnectedPeer(peerID, "client-A", 42, nil, "http://example.com")

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

// TestAddConnectedPeer_NilRegistry_NoOp confirms the nil-guard prevents calls.
func TestAddConnectedPeer_NilRegistry_NoOp(t *testing.T) {
	s := newMinimalServer(t)
	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	require.NotPanics(t, func() {
		s.addConnectedPeer(peerID, "client", 1, nil, "")
	})
}

// TestUpdateStorage_RegistersStorageField covers the updateStorage helper.
func TestUpdateStorage_RegistersStorageField(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("RegisterPeer", mock.MatchedBy(func(info *blockchain.PeerInfo) bool {
		return info.Storage == "full"
	})).Return(nil)

	s.SetCentralPeerRegistry(reg)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.updateStorage(peerID, "full")

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

// TestUpdateStorage_EmptyMode_NoOp confirms empty mode short-circuits.
func TestUpdateStorage_EmptyMode_NoOp(t *testing.T) {
	s := newMinimalServer(t)
	reg := &mockPeerRegistryClient{}
	s.SetCentralPeerRegistry(reg)

	peerID, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.updateStorage(peerID, "")
	// No expectations set: any registry call would fail AssertExpectations.
	reg.AssertExpectations(t)
}

// TestAddProtocolViolation_AddsBanScore covers the AddBanScore path with
// the "protocol_violation" reason and the canonical 20-point cost.
func TestAddProtocolViolation_AddsBanScore(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("AddBanScore", "peer-X", "protocol_violation", int32(20)).
		Return(int32(20), false, nil)

	s.SetCentralPeerRegistry(reg)

	s.addProtocolViolation("peer-X")

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

// TestAddProtocolViolation_RegistryError_LoggedNotPropagated ensures error
// from AddBanScore is logged but does not bubble up (best-effort path).
func TestAddProtocolViolation_RegistryError_LoggedNotPropagated(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("AddBanScore", "peer-Y", "protocol_violation", int32(20)).
		Return(int32(0), false, errors.NewServiceError("registry down"))

	s.SetCentralPeerRegistry(reg)

	require.NotPanics(t, func() {
		s.addProtocolViolation("peer-Y")
	})

	waitForMockCalls(t, reg, 1)
	reg.AssertExpectations(t)
}

// TestGetPeerIDFromDataHubURL_Match covers the happy path.
func TestGetPeerIDFromDataHubURL_Match(t *testing.T) {
	s := newMinimalServer(t)

	want := "12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ"
	reg := &mockPeerRegistryClient{}
	reg.On("ListPeers").Return([]*blockchain.PeerInfo{
		{ID: "other", DataHubURL: "http://other.example.com"},
		{ID: want, DataHubURL: "http://target.example.com"},
	}, nil)

	s.SetCentralPeerRegistry(reg)

	got := s.getPeerIDFromDataHubURL("http://target.example.com")
	require.Equal(t, want, got)
}

// TestGetPeerIDFromDataHubURL_NoMatch returns empty string.
func TestGetPeerIDFromDataHubURL_NoMatch(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("ListPeers").Return([]*blockchain.PeerInfo{
		{ID: "x", DataHubURL: "http://other.example.com"},
	}, nil)

	s.SetCentralPeerRegistry(reg)
	require.Equal(t, "", s.getPeerIDFromDataHubURL("http://missing.example.com"))
}

// TestGetPeerIDFromDataHubURL_NilRegistry returns empty without panic.
func TestGetPeerIDFromDataHubURL_NilRegistry(t *testing.T) {
	s := newMinimalServer(t)
	require.Equal(t, "", s.getPeerIDFromDataHubURL("http://x"))
}

// TestGetPeerIDFromDataHubURL_RegistryError returns empty.
func TestGetPeerIDFromDataHubURL_RegistryError(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("ListPeers").Return(nil, errors.NewServiceError("kaboom"))

	s.SetCentralPeerRegistry(reg)
	require.Equal(t, "", s.getPeerIDFromDataHubURL("http://x"))
}

// TestGetPeer_Found covers conversion path on success.
func TestGetPeer_Found(t *testing.T) {
	s := newMinimalServer(t)

	pidStr := "12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ"
	reg := &mockPeerRegistryClient{}
	reg.On("GetPeer", pidStr).Return(&blockchain.PeerInfo{
		ID:         pidStr,
		ClientName: "tn",
		Height:     7,
	}, true, nil)

	s.SetCentralPeerRegistry(reg)
	pid, err := peer.Decode(pidStr)
	require.NoError(t, err)

	info, ok := s.getPeer(pid)
	require.True(t, ok)
	require.NotNil(t, info)
	require.Equal(t, uint32(7), info.Height)
	require.Equal(t, "tn", info.ClientName)
}

// TestGetPeer_NotFound returns nil/false.
func TestGetPeer_NotFound(t *testing.T) {
	s := newMinimalServer(t)

	pidStr := "12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ"
	reg := &mockPeerRegistryClient{}
	reg.On("GetPeer", pidStr).Return(nil, false, nil)

	s.SetCentralPeerRegistry(reg)
	pid, err := peer.Decode(pidStr)
	require.NoError(t, err)

	info, ok := s.getPeer(pid)
	require.False(t, ok)
	require.Nil(t, info)
}

// TestGetPeer_RegistryError returns nil/false (error logged at debug).
func TestGetPeer_RegistryError(t *testing.T) {
	s := newMinimalServer(t)

	pidStr := "12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ"
	reg := &mockPeerRegistryClient{}
	reg.On("GetPeer", pidStr).Return(nil, false, errors.NewServiceError("boom"))

	s.SetCentralPeerRegistry(reg)
	pid, err := peer.Decode(pidStr)
	require.NoError(t, err)

	info, ok := s.getPeer(pid)
	require.False(t, ok)
	require.Nil(t, info)
}

// TestGetPeer_NilRegistry returns nil/false without panic.
func TestGetPeer_NilRegistry(t *testing.T) {
	s := newMinimalServer(t)
	pid, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	info, ok := s.getPeer(pid)
	require.False(t, ok)
	require.Nil(t, info)
}

// TestInjectPeerForTesting covers the test-only sync registration path which
// also asserts the Storage="full" override is applied.
func TestInjectPeerForTesting(t *testing.T) {
	s := newMinimalServer(t)

	reg := &mockPeerRegistryClient{}
	reg.On("RegisterPeer", mock.MatchedBy(func(info *blockchain.PeerInfo) bool {
		return info.Storage == "full" && info.Height == 99
	})).Return(nil)

	s.SetCentralPeerRegistry(reg)

	pid, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	s.InjectPeerForTesting(pid, "client", "http://x", 99, nil)
	reg.AssertExpectations(t)
}

// TestInjectPeerForTesting_NilRegistry does not panic.
func TestInjectPeerForTesting_NilRegistry(t *testing.T) {
	s := newMinimalServer(t)
	pid, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)

	require.NotPanics(t, func() {
		s.InjectPeerForTesting(pid, "client", "http://x", 1, nil)
	})
}

// TestEnqueueRegistryUpdate_SyncFallback covers the path where the worker pool
// channel is nil (e.g. before Start) — the closure is executed inline.
func TestEnqueueRegistryUpdate_SyncFallback(t *testing.T) {
	s := newMinimalServer(t)

	called := false
	s.enqueueRegistryUpdate(func() { called = true })
	require.True(t, called, "closure must run synchronously when channel nil")
}

// TestEnqueueRegistryUpdate_FullChannel_Drops covers the buffered-channel
// "full" branch — the update is dropped with a warning, no panic.
func TestEnqueueRegistryUpdate_FullChannel_Drops(t *testing.T) {
	s := newMinimalServer(t)
	// Capacity-1 channel that is already full ensures the default branch fires.
	s.registryUpdateCh = make(chan func(), 1)
	s.registryUpdateCh <- func() {} // fill it

	require.NotPanics(t, func() {
		s.enqueueRegistryUpdate(func() {
			t.Fatal("dropped closure must not run")
		})
	})
}

// TestCentralPeerToLocalPeerInfo_DecodableID covers the happy path of the
// blockchain.PeerInfo -> local PeerInfo conversion helper.
func TestCentralPeerToLocalPeerInfo_DecodableID(t *testing.T) {
	pidStr := "12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ"
	hash := chainhash.HashH([]byte("block"))
	now := time.Now().UTC().Truncate(time.Second)

	in := &blockchain.PeerInfo{
		ID:                     pidStr,
		ClientName:             "tn",
		Height:                 12,
		BlockHash:              &hash,
		DataHubURL:             "http://x",
		BanScore:               5,
		IsBanned:               false,
		ConnectedAt:            now,
		BytesReceived:          1024,
		LastMessageTime:        now,
		Storage:                "full",
		InteractionAttempts:    3,
		InteractionSuccesses:   2,
		InteractionFailures:    1,
		LastInteractionAttempt: now,
		LastInteractionSuccess: now,
		LastInteractionFailure: now,
		ReputationScore:        72.5,
		MaliciousCount:         0,
		AvgResponseTimeMs:      150,
	}

	out := centralPeerToLocalPeerInfo(in)
	require.NotNil(t, out)
	require.Equal(t, "tn", out.ClientName)
	require.Equal(t, uint32(12), out.Height)
	require.Equal(t, &hash, out.BlockHash)
	require.Equal(t, 5, out.BanScore)
	require.Equal(t, "full", out.Storage)
	require.Equal(t, 72.5, out.ReputationScore)
	require.Equal(t, 150*time.Millisecond, out.AvgResponseTime)
}

// TestCentralPeerToLocalPeerInfo_LegacyAddressID covers the fallback for legacy
// wire peers whose ID is a network address rather than a libp2p peer ID.
func TestCentralPeerToLocalPeerInfo_LegacyAddressID(t *testing.T) {
	in := &blockchain.PeerInfo{
		ID:         "3.123.101.88:18333",
		ClientName: "legacy:/Bitcoin SV:1.2.1/",
		Height:     1_700_000,
	}

	out := centralPeerToLocalPeerInfo(in)
	require.NotNil(t, out)
	require.Equal(t, "legacy:/Bitcoin SV:1.2.1/", out.ClientName)
	require.Equal(t, uint32(1_700_000), out.Height)
	// Raw bytes preserved as peer.ID so callers can identify the peer.
	require.Equal(t, peer.ID("3.123.101.88:18333"), out.ID)
}

// confirm the package import of mocklogger is used when newMinimalServer is
// reused across test files (the linter may flag otherwise).
var _ = mocklogger.NewTestLogger
var _ = blockchain_api.TransportType(0)
var _ context.Context = context.Background()
