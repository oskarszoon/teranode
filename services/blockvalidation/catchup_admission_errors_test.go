package blockvalidation

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCatchupAdmission_PermanentErrorsReturnImmediately(t *testing.T) {
	for _, failure := range []error{errors.NewStateError("invalid transition"), errors.NewConfigurationError("invalid configuration"), status.Error(codes.PermissionDenied, "denied"), status.Error(codes.Unimplemented, "old server")} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			attempts := 0
			err := waitForCatchupAdmission(ctx, func(context.Context) error { attempts++; return failure }, time.Second, time.Millisecond)
			require.ErrorIs(t, err, failure)
			require.Equal(t, 1, attempts)
		})
	}
}

func TestCatchupAdmission_TransientErrorsExhaustBudget(t *testing.T) {
	for _, failure := range []error{status.Error(codes.Unavailable, "restarting"), context.DeadlineExceeded, errors.NewStorageError("unavailable")} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			attempts := 0
			err := waitForCatchupAdmission(ctx, func(context.Context) error { attempts++; return failure }, time.Second, time.Millisecond)
			require.ErrorIs(t, err, failure)
			require.Equal(t, 5, attempts)
		})
	}
}

func TestCatchupAdmission_ConfirmedPauseDoesNotConsumeFailureBudget(t *testing.T) {
	attempts := 0
	err := waitForCatchupAdmission(context.Background(), func(context.Context) error {
		attempts++
		if attempts <= 2*catchupAdmissionMaxFailures {
			return blockchain.ErrCatchupPaused
		}
		return nil
	}, time.Second, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, 2*catchupAdmissionMaxFailures+1, attempts)
}

func TestCatchupAdmission_PauseDoesNotEraseTransientFailureBudget(t *testing.T) {
	attempts := 0
	failure := status.Error(codes.Unavailable, "authority unavailable")
	err := waitForCatchupAdmission(context.Background(), func(context.Context) error {
		attempts++
		if attempts%2 == 0 {
			return blockchain.ErrCatchupPaused
		}
		return failure
	}, time.Second, time.Millisecond)
	require.ErrorIs(t, err, failure)
	require.ErrorIs(t, err, errors.ErrServiceError, "authority outages must remain local failures, not peer faults")
	require.Equal(t, 2*catchupAdmissionMaxFailures-1, attempts)
}

func TestCatchupAdmission_CancellationInterruptsLongPause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	err := waitForCatchupAdmission(ctx, func(context.Context) error {
		attempts++
		if attempts == 2*catchupAdmissionMaxFailures {
			cancel()
		}
		return blockchain.ErrCatchupPaused
	}, time.Second, time.Millisecond)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2*catchupAdmissionMaxFailures, attempts)
}
