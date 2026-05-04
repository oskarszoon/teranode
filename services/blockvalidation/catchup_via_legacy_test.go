package blockvalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/mock"
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

// scriptedLegacyCatchupClient streams a fixed sequence of progress events then
// closes the channel and returns the configured rpcErr. Used to drive
// catchupViaLegacy through the success/progress/failure branches deterministically.
type scriptedLegacyCatchupClient struct {
	progress []*peer_api.CatchupProgress
	rpcErr   error
}

func (s *scriptedLegacyCatchupClient) DelegateCatchup(_ context.Context, _ string, _ uint32, ch chan<- *peer_api.CatchupProgress) error {
	for _, p := range s.progress {
		ch <- p
	}
	close(ch)
	return s.rpcErr
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

// TestCatchupViaLegacy_HappyPath_AllPhases covers lines past the lock-acquire,
// FSM transition, progress-stream loop (DOWNLOADING_HEADERS, DOWNLOADING_BLOCKS,
// COMPLETE), and successful gRPC return. Drives all three non-failure phases
// in sequence and asserts the final blocksValidated counter matches the
// derived total.
func TestCatchupViaLegacy_HappyPath_AllPhases(t *testing.T) {
	bc := &blockchain.Mock{}
	// setFSMCatchingBlocks: state already CATCHINGBLOCKS -> short-circuits the
	// CatchUpBlocks transition. restoreFSMState then sees CATCHINGBLOCKS and calls
	// Run() to return to RUNNING; mock both.
	stateCatching := blockchain.FSMStateCATCHINGBLOCKS
	bc.On("GetFSMCurrentState", mock.Anything).Return(&stateCatching, nil)
	bc.On("Run", mock.Anything, mock.Anything).Return(nil).Maybe()

	scripted := &scriptedLegacyCatchupClient{
		progress: []*peer_api.CatchupProgress{
			{Phase: peer_api.CatchupProgress_DOWNLOADING_HEADERS, TargetHeight: 100},
			{Phase: peer_api.CatchupProgress_DOWNLOADING_BLOCKS, CurrentHeight: 50, BlocksRemaining: 50},
			{Phase: peer_api.CatchupProgress_COMPLETE, CurrentHeight: 100},
		},
	}

	s := &Server{
		logger:              ulogger.TestLogger{},
		blockchainClient:    bc,
		legacyCatchupClient: scripted,
	}

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(100, &hash)

	err := s.catchupViaLegacy(context.Background(), "peer-id", "wire://peer", target)
	require.NoError(t, err)

	// COMPLETE branch sets blocksValidated to totalBlocks (target.Height - 0).
	require.Equal(t, int64(100), s.blocksValidated.Load())
}

// TestCatchupViaLegacy_FailedPhase covers the FAILED branch in the progress
// loop and ensures the categorized error is returned.
func TestCatchupViaLegacy_FailedPhase(t *testing.T) {
	bc := &blockchain.Mock{}
	stateCatching := blockchain.FSMStateCATCHINGBLOCKS
	bc.On("GetFSMCurrentState", mock.Anything).Return(&stateCatching, nil)
	bc.On("Run", mock.Anything, mock.Anything).Return(nil).Maybe()

	scripted := &scriptedLegacyCatchupClient{
		progress: []*peer_api.CatchupProgress{
			{Phase: peer_api.CatchupProgress_FAILED, ErrorCategory: "validation", ErrorMessage: "bad block"},
		},
	}

	s := &Server{
		logger:              ulogger.TestLogger{},
		blockchainClient:    bc,
		legacyCatchupClient: scripted,
	}

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(100, &hash)

	err := s.catchupViaLegacy(context.Background(), "peer-id", "wire://peer", target)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrBlockInvalid),
		"FAILED progress with category=validation must yield BlockInvalid")
}

// TestCatchupViaLegacy_GRPCStreamError covers the path where the progress
// stream completes with no FAILED event but the gRPC call itself returns an
// error. The function should wrap that as a NetworkError.
func TestCatchupViaLegacy_GRPCStreamError(t *testing.T) {
	bc := &blockchain.Mock{}
	stateCatching := blockchain.FSMStateCATCHINGBLOCKS
	bc.On("GetFSMCurrentState", mock.Anything).Return(&stateCatching, nil)
	bc.On("Run", mock.Anything, mock.Anything).Return(nil).Maybe()

	scripted := &scriptedLegacyCatchupClient{
		progress: nil, // close channel immediately
		rpcErr:   errors.NewServiceError("connection lost"),
	}

	s := &Server{
		logger:              ulogger.TestLogger{},
		blockchainClient:    bc,
		legacyCatchupClient: scripted,
	}

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(100, &hash)

	err := s.catchupViaLegacy(context.Background(), "peer-id", "wire://peer", target)
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrNetworkError),
		"non-FAILED stream finishing with rpc error must be wrapped as NetworkError")
}

// TestCatchupViaLegacy_LockAlreadyHeld covers the path past the nil-client
// guard: the function enters, builds CatchupContext, then fails to acquire
// the catchup lock because another catchup is already in progress.
func TestCatchupViaLegacy_LockAlreadyHeld(t *testing.T) {
	s := &Server{
		logger:              ulogger.TestLogger{},
		legacyCatchupClient: &stubLegacyCatchupClient{},
	}
	// Pretend another catchup already holds the lock.
	s.isCatchingUp.Store(true)

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(100, &hash)

	err := s.catchupViaLegacy(context.Background(), "peer-id", "wire://peer", target)
	require.Error(t, err)
	// acquireCatchupLock returns NewCatchupInProgressError; we don't bind it to
	// a specific sentinel here because the constructor is private. Just verify
	// we got a non-nil error and the function exited cleanly.
	require.Contains(t, err.Error(), "another catchup")
}
