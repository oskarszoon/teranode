package blockvalidation

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/legacy/peer_api"
	"github.com/bsv-blockchain/teranode/util/tracing"
)

// legacyCatchupClientI is the narrow interface BlockValidation needs to delegate
// wire-protocol catchup to the legacy service.
type legacyCatchupClientI interface {
	DelegateCatchup(ctx context.Context, peerAddr string, targetHeight uint32, progressCh chan<- *peer_api.CatchupProgress) error
}

// catchupViaLegacy delegates a catchup operation for a wire-protocol peer to the
// legacy service. BlockValidation manages the catchup lock, FSM transitions, and
// peer metrics; legacy handles the actual wire-protocol sync.
func (u *Server) catchupViaLegacy(ctx context.Context, peerID, peerAddr string, targetBlock *model.Block) (err error) {
	ctx, _, deferFn := tracing.Tracer("blockvalidation").Start(ctx, "catchupViaLegacy",
		tracing.WithParentStat(u.stats),
		tracing.WithLogMessage(u.logger, "[catchupViaLegacy] starting wire-protocol catchup to height %d via peer %s (%s)", targetBlock.Height, peerID, peerAddr),
	)
	defer deferFn()

	if u.legacyCatchupClient == nil {
		return errors.NewServiceUnavailableError("legacy catchup client not configured")
	}

	// Create a minimal CatchupContext for the lock/FSM helpers.
	catchupCtx := &CatchupContext{
		blockUpTo: targetBlock,
		baseURL:   peerAddr,
		peerID:    peerID,
		startTime: time.Now(),
	}

	// Acquire the catchup lock — only one catchup at a time.
	if lockErr := u.acquireCatchupLock(catchupCtx); lockErr != nil {
		return lockErr
	}
	defer u.releaseCatchupLock(catchupCtx, &err)

	// Transition FSM to CATCHINGBLOCKS so other services know we're syncing.
	var size atomic.Int64
	if fsmErr := u.setFSMCatchingBlocks(ctx, catchupCtx, &size); fsmErr != nil {
		return fsmErr
	}
	defer u.restoreFSMState(ctx, catchupCtx)

	// Set the initial total block count for the dashboard progress display.
	startHeight := uint32(0)
	if u.utxoStore != nil {
		startHeight = u.utxoStore.GetBlockHeight()
	}
	totalBlocks := int64(targetBlock.Height - startHeight)
	u.blocksFetched.Store(totalBlocks)

	progressCh := make(chan *peer_api.CatchupProgress, 32)

	// Run the delegated catchup in a goroutine so we can read progress.
	errCh := make(chan error, 1)
	go func() {
		errCh <- u.legacyCatchupClient.DelegateCatchup(ctx, peerAddr, targetBlock.Height, progressCh)
	}()

	for progress := range progressCh {
		switch progress.Phase {
		case peer_api.CatchupProgress_DOWNLOADING_HEADERS:
			u.logger.Debugf("[catchupViaLegacy] Downloading headers (target: %d)", progress.TargetHeight)

		case peer_api.CatchupProgress_DOWNLOADING_BLOCKS:
			validated := int64(progress.CurrentHeight - startHeight)
			u.blocksValidated.Store(validated)
			catchupCtx.currentHeight = progress.CurrentHeight
			u.logger.Debugf("[catchupViaLegacy] Block %d processed (%d remaining)", progress.CurrentHeight, progress.BlocksRemaining)

		case peer_api.CatchupProgress_COMPLETE:
			u.blocksValidated.Store(totalBlocks)
			catchupCtx.currentHeight = progress.CurrentHeight
			u.logger.Infof("[catchupViaLegacy] Catchup complete at height %d", progress.CurrentHeight)

		case peer_api.CatchupProgress_FAILED:
			err = categorizeWireCatchupError(progress.ErrorCategory, progress.ErrorMessage)
			u.logger.Warnf("[catchupViaLegacy] Catchup failed: %s (category: %s)", progress.ErrorMessage, progress.ErrorCategory)
		}
	}

	// Wait for the goroutine to finish.
	if rpcErr := <-errCh; rpcErr != nil && err == nil {
		err = errors.NewNetworkError("[catchupViaLegacy] gRPC stream error", rpcErr)
	}

	return err
}

// categorizeWireCatchupError maps the legacy service's error category string
// to the appropriate teranode error type so BlockValidation's cooldown logic
// handles it correctly.
func categorizeWireCatchupError(category, message string) error {
	switch category {
	case "validation":
		return errors.NewBlockInvalidError(message)
	case "pruned":
		return errors.ErrBlockIncomplete
	case "peer_misbehavior":
		return errors.NewNetworkPeerMaliciousError(message)
	default: // "network" and anything else
		return errors.NewNetworkError(message)
	}
}

// SetLegacyCatchupClient sets the client used to delegate wire-protocol catchup
// to the legacy service. Must be called before Start.
func (u *Server) SetLegacyCatchupClient(c legacyCatchupClientI) {
	u.legacyCatchupClient = c
}
