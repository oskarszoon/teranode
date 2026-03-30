package netsync

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	safeconversion "github.com/bsv-blockchain/go-safe-conversion"
	peerpkg "github.com/bsv-blockchain/teranode/services/legacy/peer"
)

// DelegatedCatchupPhase represents the current phase of a delegated catchup.
type DelegatedCatchupPhase int

const (
	DelegatedPhaseHeaders DelegatedCatchupPhase = iota
	DelegatedPhaseBlocks
	DelegatedPhaseComplete
	DelegatedPhaseFailed
)

// DelegatedCatchupProgress reports progress of a delegated catchup operation.
type DelegatedCatchupProgress struct {
	Phase           DelegatedCatchupPhase
	CurrentHeight   uint32
	TargetHeight    uint32
	BlocksRemaining int32
	ErrorMessage    string
	ErrorCategory   string // "network", "validation", "pruned", "peer_misbehavior"
}

// delegatedCatchupState holds the state for a delegated catchup operation.
// These fields are only accessed from the blockHandler goroutine (via
// handleBlockMsg/handleHeadersMsg) except delegatedActive which is atomic.
type delegatedCatchupState struct {
	active       atomic.Bool
	targetHeight int32
	progressCh   chan<- DelegatedCatchupProgress
	doneCh       chan error
}

// RunDelegatedCatchup runs a catchup operation using the specified peer up to
// targetHeight. It is called by the legacy gRPC server on behalf of BlockValidation.
//
// This method blocks until the catchup completes, fails, or the context is cancelled.
// Progress is reported on progressCh. The caller owns FSM transitions.
func (sm *SyncManager) RunDelegatedCatchup(ctx context.Context, peer *peerpkg.Peer, targetHeight uint32, progressCh chan<- DelegatedCatchupProgress) error {
	targetHeightInt32, err := safeconversion.Uint32ToInt32(targetHeight)
	if err != nil {
		return fmt.Errorf("invalid target height %d: %w", targetHeight, err)
	}

	// Set up delegated state. The blockHandler goroutine reads these fields
	// to know it should report progress and stop at the target.
	sm.delegated.targetHeight = targetHeightInt32
	sm.delegated.progressCh = progressCh
	sm.delegated.doneCh = make(chan error, 1)
	sm.delegated.active.Store(true)
	defer func() {
		sm.delegated.active.Store(false)
		sm.delegated.progressCh = nil
		close(sm.delegated.doneCh)
	}()

	// If legacy is already syncing with this peer (startSync kicked in before
	// BlockValidation's request arrived), attach to the running sync rather
	// than starting a duplicate one. The blockHandler hooks will begin
	// reporting progress now that delegated.active is true.
	if sp := sm.loadSyncPeer(); sp != nil {
		sm.logger.Infof("[delegated_catchup] Attaching to existing sync with peer %s (target: %d)",
			sp.String(), targetHeight)

		progressCh <- DelegatedCatchupProgress{
			Phase:        DelegatedPhaseBlocks,
			TargetHeight: targetHeight,
		}

		// Wait for the already-running sync to reach our target or fail.
		select {
		case err := <-sm.delegated.doneCh:
			return err
		case <-ctx.Done():
			sm.logger.Infof("[delegated_catchup] Context cancelled while attached to existing sync")
			return ctx.Err()
		case <-sm.quit:
			return fmt.Errorf("sync manager shutting down")
		}
	}

	// No active sync — start one from scratch.
	bestBlockHeader, bestBlockHeaderMeta, err := sm.blockchainClient.GetBestBlockHeader(ctx)
	if err != nil {
		return fmt.Errorf("failed to get best block header: %w", err)
	}

	bestBlockHeightInt32, err := safeconversion.Uint32ToInt32(bestBlockHeaderMeta.Height)
	if err != nil {
		return fmt.Errorf("failed to convert block height: %w", err)
	}

	if targetHeightInt32 <= bestBlockHeightInt32 {
		// Already at or past target.
		progressCh <- DelegatedCatchupProgress{
			Phase:         DelegatedPhaseComplete,
			CurrentHeight: bestBlockHeaderMeta.Height,
			TargetHeight:  targetHeight,
		}
		return nil
	}

	sm.requestedBlocks.Clear()

	locator, err := sm.blockchainClient.GetBlockLocator(ctx, bestBlockHeader.Hash(), bestBlockHeaderMeta.Height)
	if err != nil {
		return fmt.Errorf("failed to get block locator: %w", err)
	}

	sm.logger.Infof("[delegated_catchup] Starting catchup from height %d to %d using peer %s",
		bestBlockHeaderMeta.Height, targetHeight, peer.String())

	// Use headers-first mode if we have checkpoints ahead of us.
	if sm.nextCheckpoint != nil &&
		bestBlockHeightInt32 < sm.nextCheckpoint.Height &&
		bestBlockHeightInt32 < targetHeightInt32 {

		if err = peer.PushGetHeadersMsg(locator, sm.nextCheckpoint.Hash); err != nil {
			return fmt.Errorf("failed to send getheaders: %w", err)
		}
		sm.headersFirstMode.Store(true)
		sm.logger.Infof("[delegated_catchup] Downloading headers for blocks %d to %d from peer %s",
			bestBlockHeaderMeta.Height+1, sm.nextCheckpoint.Height, peer.String())
	} else {
		if err = peer.PushGetBlocksMsg(locator, &zeroHash); err != nil {
			return fmt.Errorf("failed to send getblocks: %w", err)
		}
	}

	// Register the sync peer.
	peer.SetSyncPeer(true)
	sm.storeSyncPeer(peer, &syncPeerState{
		lastBlockTime:     time.Now(),
		recvBytes:         peer.BytesReceived(),
		recvBytesLastTick: 0,
	})

	// Send initial progress.
	progressCh <- DelegatedCatchupProgress{
		Phase:           DelegatedPhaseHeaders,
		CurrentHeight:   bestBlockHeaderMeta.Height,
		TargetHeight:    targetHeight,
		BlocksRemaining: targetHeightInt32 - bestBlockHeightInt32,
	}

	// Wait for the event-driven sync loop to complete or fail.
	select {
	case err := <-sm.delegated.doneCh:
		return err
	case <-ctx.Done():
		// Context cancelled - clean up sync peer.
		sm.logger.Infof("[delegated_catchup] Context cancelled, aborting catchup")
		sp := sm.loadSyncPeer()
		if sp != nil {
			sp.SetSyncPeer(false)
			sm.storeSyncPeer(nil, nil)
		}
		return ctx.Err()
	case <-sm.quit:
		return fmt.Errorf("sync manager shutting down")
	}
}

// delegatedSendProgress sends a progress update if a delegated catchup is active.
// Called from handleBlockMsg after each successful block.
func (sm *SyncManager) delegatedSendProgress(blockHeight int32) {
	if !sm.delegated.active.Load() || sm.delegated.progressCh == nil {
		return
	}

	targetHeight := sm.delegated.targetHeight
	remaining := targetHeight - blockHeight
	if remaining < 0 {
		remaining = 0
	}

	currentHeightUint32, _ := safeconversion.Int32ToUint32(blockHeight)
	targetHeightUint32, _ := safeconversion.Int32ToUint32(targetHeight)

	sm.delegated.progressCh <- DelegatedCatchupProgress{
		Phase:           DelegatedPhaseBlocks,
		CurrentHeight:   currentHeightUint32,
		TargetHeight:    targetHeightUint32,
		BlocksRemaining: remaining,
	}
}

// delegatedCheckComplete checks if the delegated catchup target has been reached.
// If so, it clears the sync peer and signals completion. Called from handleBlockMsg.
func (sm *SyncManager) delegatedCheckComplete(blockHeight int32) bool {
	if !sm.delegated.active.Load() {
		return false
	}

	if blockHeight < sm.delegated.targetHeight {
		return false
	}

	sm.logger.Infof("[delegated_catchup] Reached target height %d, catchup complete", sm.delegated.targetHeight)

	// Clear sync peer - we're done.
	sp := sm.loadSyncPeer()
	if sp != nil {
		sp.SetSyncPeer(false)
		sm.storeSyncPeer(nil, nil)
	}

	targetHeightUint32, _ := safeconversion.Int32ToUint32(sm.delegated.targetHeight)
	currentHeightUint32, _ := safeconversion.Int32ToUint32(blockHeight)

	sm.delegated.progressCh <- DelegatedCatchupProgress{
		Phase:         DelegatedPhaseComplete,
		CurrentHeight: currentHeightUint32,
		TargetHeight:  targetHeightUint32,
	}

	sm.delegated.doneCh <- nil
	return true
}

// delegatedSignalError signals a failure to the delegated catchup caller.
// Called from handleDonePeerMsg when the sync peer disconnects during delegated catchup.
func (sm *SyncManager) delegatedSignalError(err error, category string) {
	if !sm.delegated.active.Load() {
		return
	}

	targetHeightUint32, _ := safeconversion.Int32ToUint32(sm.delegated.targetHeight)

	sm.delegated.progressCh <- DelegatedCatchupProgress{
		Phase:         DelegatedPhaseFailed,
		TargetHeight:  targetHeightUint32,
		ErrorMessage:  err.Error(),
		ErrorCategory: category,
	}

	sm.delegated.doneCh <- err
}

// delegatedSendHeaderProgress sends a header-download progress update.
func (sm *SyncManager) delegatedSendHeaderProgress() {
	if !sm.delegated.active.Load() || sm.delegated.progressCh == nil {
		return
	}

	targetHeightUint32, _ := safeconversion.Int32ToUint32(sm.delegated.targetHeight)

	sm.delegated.progressCh <- DelegatedCatchupProgress{
		Phase:        DelegatedPhaseHeaders,
		TargetHeight: targetHeightUint32,
	}
}

// isDelegatedCatchupActive returns whether a delegated catchup is currently active.
func (sm *SyncManager) isDelegatedCatchupActive() bool {
	return sm.delegated.active.Load()
}

// SetOnBlockAccepted sets a callback invoked after a block from a peer is accepted.
// The server uses this to record interaction success in the central peer registry.
func (sm *SyncManager) SetOnBlockAccepted(fn func(peerAddr string, height int32)) {
	sm.onBlockAccepted = fn
}
