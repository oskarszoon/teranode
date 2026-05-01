package blockvalidation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// --- pure helper coverage ---

func TestShortHash(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"0123456789abcdef", "0123456789abcdef"}, // 16 chars — unchanged
		{"00000000aabbccddeeff112233445566", "00000000...33445566"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, shortHash(tt.in))
	}
}

func TestFormatInt(t *testing.T) {
	require.Equal(t, "0", formatInt(0))
	require.Equal(t, "42", formatInt(42))
	require.Equal(t, "-7", formatInt(-7))
}

func TestFormatFloat(t *testing.T) {
	require.Equal(t, "0.0", formatFloat(0, 1))
	require.Equal(t, "3.14", formatFloat(3.14159, 2))
	require.Equal(t, "100.000", formatFloat(100, 3))
}

func TestFormatProgress(t *testing.T) {
	require.Equal(t, "0/0", formatProgress(0, 0))
	require.Equal(t, "0/100 (0.0%)", formatProgress(0, 100))
	require.Equal(t, "50/100 (50.0%)", formatProgress(50, 100))
	require.Equal(t, "100/100 (100.0%)", formatProgress(100, 100))
}

func TestFormatDuration(t *testing.T) {
	require.Equal(t, "0ms", formatDuration(0))
	require.Equal(t, "999ms", formatDuration(999))
	require.Equal(t, "1s", formatDuration(1_000))
	require.Equal(t, "59s", formatDuration(59_000))
	require.Equal(t, "1m", formatDuration(60_000))
	require.Equal(t, "59m", formatDuration(59*60_000))
	require.Equal(t, "1h", formatDuration(3_600_000))
	require.Equal(t, "5h", formatDuration(5*3_600_000))
}

func TestFormatCatchupStatusSummary_NotCatching(t *testing.T) {
	require.Equal(t, "No active catchup", formatCatchupStatusSummary(nil))
	require.Equal(t, "No active catchup", formatCatchupStatusSummary(&CatchupStatus{IsCatchingUp: false}))
}

func TestFormatCatchupStatusSummary_Active_NoTotal(t *testing.T) {
	s := &CatchupStatus{
		IsCatchingUp:    true,
		PeerID:          "peer-1",
		TargetBlockHash: "00000000aabbccddeeff112233445566",
		BlocksValidated: 5,
		TotalBlocks:     0,
		DurationMs:      1500,
	}
	got := formatCatchupStatusSummary(s)
	require.Contains(t, got, "Catching up from peer peer-1")
	require.Contains(t, got, "00000000...33445566")
	require.Contains(t, got, "1s")
	// No total → no progress fragment
	require.NotContains(t, got, "(")
}

func TestFormatCatchupStatusSummary_Active_WithProgress(t *testing.T) {
	s := &CatchupStatus{
		IsCatchingUp:    true,
		PeerID:          "peer-2",
		TargetBlockHash: "deadbeef",
		BlocksValidated: 50,
		TotalBlocks:     100,
		DurationMs:      120_000,
	}
	got := formatCatchupStatusSummary(s)
	require.Contains(t, got, "peer-2")
	require.Contains(t, got, "50/100 (50.0%)")
	require.Contains(t, got, "2m")
}

// --- Server-state-dependent paths ---

func newCatchupStatusServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		logger: ulogger.TestLogger{},
	}
}

func TestGetCatchupStatusInternal_NotCatchingUp_NoPrevious(t *testing.T) {
	s := newCatchupStatusServer(t)
	require.False(t, s.isCatchingUp.Load())

	got := s.getCatchupStatusInternal()
	require.False(t, got.IsCatchingUp)
	require.Nil(t, got.PreviousAttempt)
}

func TestGetCatchupStatusInternal_NotCatchingUp_WithPrevious(t *testing.T) {
	s := newCatchupStatusServer(t)
	prev := &PreviousAttempt{
		PeerID:       "peer-failed",
		ErrorMessage: "timeout",
		ErrorType:    "network_error",
	}
	s.activeCatchupCtxMu.Lock()
	s.previousCatchupAttempt = prev
	s.activeCatchupCtxMu.Unlock()

	got := s.getCatchupStatusInternal()
	require.False(t, got.IsCatchingUp)
	require.NotNil(t, got.PreviousAttempt)
	require.Equal(t, "peer-failed", got.PreviousAttempt.PeerID)
}

func TestGetCatchupStatusInternal_CatchingUp_NilContext(t *testing.T) {
	// Race-condition guard: IsCatchingUp=true but ctx=nil → reset IsCatchingUp.
	s := newCatchupStatusServer(t)
	s.isCatchingUp.Store(true)

	got := s.getCatchupStatusInternal()
	require.False(t, got.IsCatchingUp)
}

func TestGetCatchupStatusInternal_CatchingUp_HeadersPhase(t *testing.T) {
	s := newCatchupStatusServer(t)
	s.isCatchingUp.Store(true)

	hash := chainhash.HashH([]byte("target"))
	target := model.NewSyntheticBlock(1234, &hash)

	startTime := time.Now().Add(-5 * time.Second)
	s.activeCatchupCtxMu.Lock()
	s.activeCatchupCtx = &CatchupContext{
		peerID:    "peer-A",
		baseURL:   "http://peer-a.example.com",
		blockUpTo: target,
		startTime: startTime,
		forkDepth: 3,
	}
	s.activeCatchupCtxMu.Unlock()

	got := s.getCatchupStatusInternal()
	require.True(t, got.IsCatchingUp)
	require.Equal(t, "peer-A", got.PeerID)
	require.Equal(t, "http://peer-a.example.com", got.PeerURL)
	require.Equal(t, target.Hash().String(), got.TargetBlockHash)
	require.Equal(t, uint32(1234), got.TargetBlockHeight)
	require.Equal(t, uint32(3), got.ForkDepth)
	require.Equal(t, "downloading_headers", got.Phase)
	require.GreaterOrEqual(t, got.DurationMs, int64(4_000)) // ~5s elapsed
}

func TestGetCatchupStatusInternal_CatchingUp_DelegatedFallback(t *testing.T) {
	// Delegated (wire) catchup: TotalBlocks=0 (no blockHeaders) but BlocksFetched>0.
	// The internal helper should use BlocksFetched as the total.
	s := newCatchupStatusServer(t)
	s.isCatchingUp.Store(true)
	s.blocksFetched.Store(500)
	s.blocksValidated.Store(100)

	hash := chainhash.HashH([]byte("target2"))
	target := model.NewSyntheticBlock(2000, &hash)

	commonHash := chainhash.HashH([]byte("ancestor"))
	commonMeta := &model.BlockHeaderMeta{Height: 999}

	s.activeCatchupCtxMu.Lock()
	s.activeCatchupCtx = &CatchupContext{
		peerID:             "peer-B",
		baseURL:            "wire://peer-b",
		blockUpTo:          target,
		startTime:          time.Now(),
		currentHeight:      999,
		commonAncestorHash: &commonHash,
		commonAncestorMeta: commonMeta,
	}
	s.activeCatchupCtxMu.Unlock()

	got := s.getCatchupStatusInternal()
	require.True(t, got.IsCatchingUp)
	require.Equal(t, 500, got.TotalBlocks)
	require.Equal(t, int64(500), got.BlocksFetched)
	require.Equal(t, int64(100), got.BlocksValidated)
	require.Equal(t, uint32(999), got.CurrentHeight)
	require.Equal(t, commonHash.String(), got.CommonAncestorHash)
	require.Equal(t, uint32(999), got.CommonAncestorHeight)
	require.Equal(t, "validating_blocks", got.Phase)
}

func TestGetCatchupStatusInternal_CatchingUp_FinalizingPhase(t *testing.T) {
	s := newCatchupStatusServer(t)
	s.isCatchingUp.Store(true)
	s.blocksFetched.Store(10)
	s.blocksValidated.Store(10)

	hash := chainhash.HashH([]byte("target3"))
	target := model.NewSyntheticBlock(3000, &hash)

	s.activeCatchupCtxMu.Lock()
	s.activeCatchupCtx = &CatchupContext{
		peerID:    "peer-C",
		baseURL:   "http://peer-c",
		blockUpTo: target,
		startTime: time.Now(),
	}
	s.activeCatchupCtxMu.Unlock()

	got := s.getCatchupStatusInternal()
	require.True(t, got.IsCatchingUp)
	// TotalBlocks=10 (from BlocksFetched fallback), BlocksValidated=10 → finalizing.
	require.Equal(t, "finalizing", got.Phase)
}

func TestGetCatchupStatusSummary_NotCatching(t *testing.T) {
	s := newCatchupStatusServer(t)
	require.Equal(t, "No active catchup", s.GetCatchupStatusSummary())
}

func TestGetCatchupStatusSummary_Active(t *testing.T) {
	s := newCatchupStatusServer(t)
	s.isCatchingUp.Store(true)
	s.blocksFetched.Store(0)
	s.blocksValidated.Store(0)

	hash := chainhash.HashH([]byte("target4"))
	target := model.NewSyntheticBlock(4000, &hash)

	s.activeCatchupCtxMu.Lock()
	s.activeCatchupCtx = &CatchupContext{
		peerID:    "peer-D",
		baseURL:   "http://peer-d",
		blockUpTo: target,
		startTime: time.Now().Add(-2500 * time.Millisecond),
	}
	s.activeCatchupCtxMu.Unlock()

	summary := s.GetCatchupStatusSummary()
	require.Contains(t, summary, "Catching up from peer peer-D")
}

// keep imports referenced even if optimisation flags fire
var _ = atomic.LoadInt32
var _ = context.Background
