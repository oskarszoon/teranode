package recoverreplayedtransactions

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// Invalid apply arguments must be rejected before opening production stores.
func TestRunRejectsUnsafeOptionsBeforeConnecting(t *testing.T) {
	for _, mode := range []string{"apply", "resume"} {
		err := Run(context.Background(), ulogger.TestLogger{}, &settings.Settings{}, Options{Mode: mode, Manifest: "manifest.db", Journal: "journal.db", RPC: "https://example.invalid", Timeout: time.Minute, RequestsPerSecond: 1, Concurrency: 1}, io.Discard, io.Discard)
		require.Error(t, err)
		require.Contains(t, err.Error(), "maintenance")
	}
}

func TestExitCodesRemainDistinctWhenWrapped(t *testing.T) {
	require.Equal(t, 0, ExitCode(nil))
	require.Equal(t, 1, ExitCode(commandError("ordinary failure")))
	require.Equal(t, 2, ExitCode(commandError("audit: %w", replayrecovery.ErrIncomplete)))
	require.Equal(t, 3, ExitCode(commandError("verify: %w", replayrecovery.ErrPending)))
}

func TestOptionsRejectConflictingPathsAndModes(t *testing.T) {
	o := Options{Mode: "discover", Manifest: "same.db", Journal: "same.db", RPC: "https://example.invalid", Timeout: time.Minute, RequestsPerSecond: 1, Concurrency: 1}
	require.Error(t, o.validate())
	o.Journal = "journal.db"
	o.Mode = "purge"
	require.Error(t, o.validate())
	o.Mode = "discover"
	o.HistoryIndex = "same.db"
	require.Error(t, o.validate())
}
