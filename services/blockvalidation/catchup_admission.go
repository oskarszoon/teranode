package blockvalidation

import (
	"context"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	catchupAdmissionTimeout       = 5 * time.Second
	catchupAdmissionRetryInterval = time.Second
	catchupAdmissionMaxFailures   = 5
)

// waitForCatchupAdmission retains the caller's current unit until the blockchain
// authority accepts it. Its state snapshot is serialized with durable STOP;
// an accepted unit may finish after STOP, but its successor needs new admission.
// Cached FSM state must not be consulted here: cached IDLE can be synthetic, and
// cached CATCHINGBLOCKS can predate an operator's STOP.
func (u *Server) waitForCatchupAdmission(ctx context.Context) error {
	waiting := false
	err := waitForCatchupAdmission(ctx, func(rpcCtx context.Context) error {
		err := u.blockchainClient.AdmitCatchupWork(rpcCtx)
		if err != nil && !waiting {
			u.logger.Infof("[catchup] Waiting for authoritative work admission: %v", err)
			waiting = true
		} else if err == nil && waiting {
			u.logger.Infof("[catchup] Authoritative work admission restored")
		}
		return err
	}, catchupAdmissionTimeout, catchupAdmissionRetryInterval)
	if err != nil && ctx.Err() == nil {
		u.logger.Warnf("[catchup] Authoritative work admission failed: %v", err)
	}
	return err
}

// Each failed or ambiguous RPC outcome leaves the unit unstarted. In particular,
// a timeout may have reached the server, but never grants permission locally.
// Retry waits use the service context, not the short-lived RPC context. A
// confirmed pause waits for explicit resume or cancellation. Transient failures
// have a bounded budget; permanent failures return immediately. Neither is proof
// of peer misbehavior. A pause does not erase earlier transient failures.
func waitForCatchupAdmission(ctx context.Context, admit func(context.Context) error, rpcTimeout, retryInterval time.Duration) error {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
		err := admit(rpcCtx)
		if err == nil {
			err = rpcCtx.Err()
		}
		cancel()
		if parentErr := ctx.Err(); parentErr != nil {
			return parentErr
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, blockchain.ErrCatchupPaused) {
			if !transientCatchupAdmissionError(err) {
				return errors.NewServiceError("catchup authority rejected work admission", err)
			}
			failures++
			if failures >= catchupAdmissionMaxFailures {
				return errors.NewServiceError("catchup admission failed after %d transient failures", failures, err)
			}
		}
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RPC deadlines can expire while the service context remains usable. Canceled
// service contexts are checked by the caller before this classification.
func transientCatchupAdmissionError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.IsRetryableError(err) || errors.IsTransientLocalError(err) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	default:
		return false
	}
}
