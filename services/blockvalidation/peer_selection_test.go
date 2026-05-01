package blockvalidation

import (
	"context"
	"testing"

	chainhashPkg "github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	modelPkg "github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/p2p"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/require"
)

// mockP2PClientForSelection implements P2PClientI with no-op methods except
// GetPeersForCatchup, which the selectBestPeersForCatchup tests drive.
type mockP2PClientForSelection struct {
	peers []*p2p.PeerInfo
	err   error
}

func (m *mockP2PClientForSelection) GetPeersForCatchup(_ context.Context) ([]*p2p.PeerInfo, error) {
	return m.peers, m.err
}

// All other P2PClientI methods are unused by selectBestPeersForCatchup; stub them.
func (m *mockP2PClientForSelection) RecordCatchupAttempt(context.Context, string) error {
	return nil
}
func (m *mockP2PClientForSelection) RecordCatchupSuccess(context.Context, string, int64) error {
	return nil
}
func (m *mockP2PClientForSelection) RecordCatchupFailure(context.Context, string) error { return nil }
func (m *mockP2PClientForSelection) RecordCatchupMalicious(context.Context, string) error {
	return nil
}
func (m *mockP2PClientForSelection) UpdateCatchupError(context.Context, string, string) error {
	return nil
}
func (m *mockP2PClientForSelection) UpdateCatchupReputation(context.Context, string, float64) error {
	return nil
}
func (m *mockP2PClientForSelection) GetPeer(context.Context, string) (*p2p.PeerInfo, error) {
	return nil, nil
}
func (m *mockP2PClientForSelection) ReportValidBlock(context.Context, string, string) error {
	return nil
}
func (m *mockP2PClientForSelection) ReportValidSubtree(context.Context, string, string) error {
	return nil
}
func (m *mockP2PClientForSelection) IsPeerMalicious(context.Context, string) (bool, string, error) {
	return false, "", nil
}
func (m *mockP2PClientForSelection) IsPeerUnhealthy(context.Context, string) (bool, string, float32, error) {
	return false, "", 0, nil
}
func (m *mockP2PClientForSelection) RecordBytesDownloaded(context.Context, string, uint64) error {
	return nil
}

// newSelectionTestServer returns a minimal Server with only logger + p2pClient set.
func newSelectionTestServer(client P2PClientI) *Server {
	return &Server{
		logger:    ulogger.TestLogger{},
		p2pClient: client,
	}
}

func mustPeerID(t *testing.T) peer.ID {
	t.Helper()
	id, err := peer.Decode("12D3KooWNvSZnPi3RrhrTwEY4LuHBeB6K6idsA5DZtEt3yBHk5CQ")
	require.NoError(t, err)
	return id
}

func mustPeerID2(t *testing.T) peer.ID {
	t.Helper()
	id, err := peer.Decode("12D3KooWR79aidxhS1XMjYzBqtpivvvgpDzUe9pAyGnc1Zf8dX4y")
	require.NoError(t, err)
	return id
}

func TestSelectBestPeersForCatchup_NilClient(t *testing.T) {
	s := newSelectionTestServer(nil)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Nil(t, peers)
}

func TestSelectBestPeersForCatchup_ClientError(t *testing.T) {
	client := &mockP2PClientForSelection{
		err: errors.NewServiceError("boom"),
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.Error(t, err)
	require.Nil(t, peers)
}

func TestSelectBestPeersForCatchup_EmptyResult(t *testing.T) {
	client := &mockP2PClientForSelection{peers: []*p2p.PeerInfo{}}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Nil(t, peers)
}

func TestSelectBestPeersForCatchup_FiltersByHeight(t *testing.T) {
	pid1 := mustPeerID(t)
	pid2 := mustPeerID2(t)

	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid1, Height: 50, DataHubURL: "http://low.example.com"}, // below target — filtered
			{ID: pid2, Height: 200, DataHubURL: "http://ok.example.com"}, // above — kept
		},
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.Equal(t, pid2.String(), peers[0].ID)
	require.Equal(t, uint32(200), peers[0].Height)
}

func TestSelectBestPeersForCatchup_FiltersOutListenOnly(t *testing.T) {
	pid1 := mustPeerID(t)
	pid2 := mustPeerID2(t)

	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid1, Height: 200, DataHubURL: ""},                      // listen-only — filtered
			{ID: pid2, Height: 200, DataHubURL: "http://ok.example.com"}, // has URL — kept
		},
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.Equal(t, pid2.String(), peers[0].ID)
}

func TestSelectBestPeersForCatchup_PopulatesAllFields(t *testing.T) {
	pid := mustPeerID(t)
	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{
				ID:                   pid,
				Height:               150,
				DataHubURL:           "http://peer.example.com",
				Storage:              "full",
				ReputationScore:      77.5,
				InteractionAttempts:  10,
				InteractionSuccesses: 8,
				InteractionFailures:  2,
			},
		},
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, peers, 1)

	got := peers[0]
	require.Equal(t, pid.String(), got.ID)
	require.Equal(t, "full", got.Storage)
	require.Equal(t, "http://peer.example.com", got.DataHubURL)
	require.Equal(t, uint32(150), got.Height)
	require.Equal(t, 77.5, got.CatchupReputationScore)
	require.Equal(t, int64(10), got.CatchupAttempts)
	require.Equal(t, int64(8), got.CatchupSuccesses)
	require.Equal(t, int64(2), got.CatchupFailures)
	// TransportType defaults to 0 (HTTP) — see comment on PeerForCatchup.TransportType.
	require.Equal(t, int32(0), got.TransportType)
}

func TestSelectBestPeersForCatchup_AllFiltered(t *testing.T) {
	pid1 := mustPeerID(t)
	pid2 := mustPeerID2(t)

	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid1, Height: 50, DataHubURL: "http://low.example.com"}, // height filter
			{ID: pid2, Height: 200, DataHubURL: ""},                      // listen-only filter
		},
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, peers, 0)
}

func TestSelectBestPeersForCatchup_LogsSuccessRate(t *testing.T) {
	// Triggers the success-rate-zero branch (CatchupAttempts == 0) and the
	// non-zero branch on the same call so both log paths are covered.
	pid1 := mustPeerID(t)
	pid2 := mustPeerID2(t)

	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid1, Height: 200, DataHubURL: "http://a.example.com"}, // attempts=0
			{
				ID: pid2, Height: 200, DataHubURL: "http://b.example.com",
				InteractionAttempts:  4,
				InteractionSuccesses: 3,
			},
		},
	}
	s := newSelectionTestServer(client)

	peers, err := s.selectBestPeersForCatchup(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, peers, 2)
}

// --- tryAlternativePeersForCatchup tests ---
//
// We cover the paths that DO NOT actually invoke u.catchup (it requires a fully
// configured Server with utxoStore, FSM, etc). The reachable branches are:
//   - selectBestPeersForCatchup returns no peers       -> return false
//   - all returned peers match excludePeerID           -> return false
//   - all returned peers are flagged malicious          -> return false

func TestTryAlternativePeers_NoPeersAvailable(t *testing.T) {
	client := &mockP2PClientForSelection{peers: []*p2p.PeerInfo{}}
	s := newSelectionTestServer(client)

	hash := newTestBlockHash()
	block := newSyntheticBlock(t, 100, hash)

	got := s.tryAlternativePeersForCatchup(context.Background(), block, "any-peer")
	require.False(t, got)
}

func TestTryAlternativePeers_AllPeersExcluded(t *testing.T) {
	pid := mustPeerID(t)
	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid, Height: 200, DataHubURL: "http://x"},
		},
	}
	s := newSelectionTestServer(client)

	hash := newTestBlockHash()
	block := newSyntheticBlock(t, 100, hash)

	// Exclude the only peer -> loop skips it -> return false.
	got := s.tryAlternativePeersForCatchup(context.Background(), block, pid.String())
	require.False(t, got)
}

func TestTryAlternativePeers_AllPeersMalicious(t *testing.T) {
	pid := mustPeerID(t)
	client := &mockP2PClientForSelection{
		peers: []*p2p.PeerInfo{
			{ID: pid, Height: 200, DataHubURL: "http://x"},
		},
	}

	reg := &mockPeerRegistry{}
	reg.On("IsPeerBanned", pid.String()).Return(true, nil)

	s := newSelectionTestServer(client)
	s.centralPeerRegistry = reg

	hash := newTestBlockHash()
	block := newSyntheticBlock(t, 100, hash)

	got := s.tryAlternativePeersForCatchup(context.Background(), block, "different-peer")
	require.False(t, got)

	reg.AssertExpectations(t)
}

func TestTryAlternativePeers_SelectionError(t *testing.T) {
	// selectBestPeersForCatchup returns (nil, err) -- function logs and proceeds
	// with empty list, returning false.
	client := &mockP2PClientForSelection{err: errors.NewServiceError("kaboom")}
	s := newSelectionTestServer(client)

	hash := newTestBlockHash()
	block := newSyntheticBlock(t, 100, hash)

	got := s.tryAlternativePeersForCatchup(context.Background(), block, "any")
	require.False(t, got)
}

// helpers
func newTestBlockHash() *chainhashPkg.Hash {
	h := chainhashPkg.HashH([]byte("test-block"))
	return &h
}

func newSyntheticBlock(t *testing.T, height uint32, hash *chainhashPkg.Hash) *modelPkg.Block {
	t.Helper()
	return modelPkg.NewSyntheticBlock(height, hash)
}
