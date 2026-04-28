package legacy

import (
	"context"
	"errors"
	"testing"

	"github.com/bsv-blockchain/teranode/services/legacy/netsync"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/stretchr/testify/require"
)

func TestRelayDelegatedCatchupProgress_ReturnsSetupError(t *testing.T) {
	progressCh := make(chan netsync.DelegatedCatchupProgress)
	errCh := make(chan error, 1)
	setupErr := errors.New("failed to send getheaders")
	errCh <- setupErr

	var sent []*peer_api.CatchupProgress
	err := relayDelegatedCatchupProgress(context.Background(), progressCh, errCh, func(msg *peer_api.CatchupProgress) error {
		sent = append(sent, msg)
		return nil
	})

	require.ErrorIs(t, err, setupErr)
	require.Empty(t, sent)
}

func TestRelayDelegatedCatchupProgress_SendsQueuedProgressBeforeError(t *testing.T) {
	progressCh := make(chan netsync.DelegatedCatchupProgress, 1)
	errCh := make(chan error, 1)
	runErr := errors.New("validation failed")
	progressCh <- netsync.DelegatedCatchupProgress{
		Phase:         netsync.DelegatedPhaseFailed,
		TargetHeight:  125,
		ErrorMessage:  runErr.Error(),
		ErrorCategory: "validation",
	}
	errCh <- runErr

	var sent []*peer_api.CatchupProgress
	err := relayDelegatedCatchupProgress(context.Background(), progressCh, errCh, func(msg *peer_api.CatchupProgress) error {
		sent = append(sent, msg)
		return nil
	})

	require.ErrorIs(t, err, runErr)
	require.Len(t, sent, 1)
	require.Equal(t, peer_api.CatchupProgress_FAILED, sent[0].Phase)
	require.Equal(t, uint32(125), sent[0].TargetHeight)
	require.Equal(t, "validation", sent[0].ErrorCategory)
}
