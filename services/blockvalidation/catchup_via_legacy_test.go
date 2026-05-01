package blockvalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// --- categorizeWireCatchupError tests (pure function, all switch arms) ---

func TestCategorizeWireCatchupError_Validation(t *testing.T) {
	err := categorizeWireCatchupError("validation", "bad block")
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrBlockInvalid),
		"validation category must produce a BlockInvalid error")
}

func TestCategorizeWireCatchupError_Pruned(t *testing.T) {
	err := categorizeWireCatchupError("pruned", "no data")
	require.Error(t, err)
	require.ErrorIs(t, err, errors.ErrBlockIncomplete)
}

func TestCategorizeWireCatchupError_PeerMisbehavior(t *testing.T) {
	err := categorizeWireCatchupError("peer_misbehavior", "lying about chain")
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrNetworkPeerMalicious),
		"peer_misbehavior must produce a NetworkPeerMalicious error")
}

func TestCategorizeWireCatchupError_Network(t *testing.T) {
	err := categorizeWireCatchupError("network", "timeout")
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrNetworkError),
		"network category must produce a Network error")
}

func TestCategorizeWireCatchupError_DefaultUnknown(t *testing.T) {
	// Anything not matching a named arm falls through to the network default.
	err := categorizeWireCatchupError("totally-unknown", "oops")
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrNetworkError),
		"unknown category must fall through to Network default")
}

// --- SetLegacyCatchupClient + nil-client guard ---

// stubLegacyCatchupClient is a minimal legacyCatchupClientI used by setter test.
type stubLegacyCatchupClient struct{}

func (s *stubLegacyCatchupClient) DelegateCatchup(_ context.Context, _ string, _ uint32, _ chan<- *peer_api.CatchupProgress) error {
	return nil
}

func TestSetLegacyCatchupClient(t *testing.T) {
	s := &Server{logger: ulogger.TestLogger{}}
	require.Nil(t, s.legacyCatchupClient)

	c := &stubLegacyCatchupClient{}
	s.SetLegacyCatchupClient(c)
	require.NotNil(t, s.legacyCatchupClient)
}

// TestCatchupViaLegacy_NilClient covers the early-return guard at the top of
// catchupViaLegacy when no legacyCatchupClient is configured.
func TestCatchupViaLegacy_NilClient(t *testing.T) {
	s := &Server{
		logger: ulogger.TestLogger{},
		// legacyCatchupClient deliberately left nil
	}

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(100, &hash)

	err := s.catchupViaLegacy(context.Background(), "peer-id", "wire://peer", target)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrServiceUnavailable),
		"nil legacy catchup client must yield ErrServiceUnavailable")
}
