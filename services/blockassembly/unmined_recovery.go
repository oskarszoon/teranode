package blockassembly

import (
	"context"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

const unminedRecoveryRetryDelay = time.Minute

func (b *BlockAssembler) unminedRecoveryInterval() time.Duration {
	if interval := b.settings.BlockAssembly.UnminedRecoveryInterval; interval > 0 {
		return interval
	}
	return settings.DefaultUnminedRecoveryInterval
}

// Retry deferred or failed recovery without spinning or waiting another full
// recovery interval after an operator resume or temporary loss of authority.
func (b *BlockAssembler) nextUnminedRecoveryDelay(recovered bool) time.Duration {
	interval := b.unminedRecoveryInterval()
	if (!recovered || b.recoveryMiningBlocked.Load()) && interval > unminedRecoveryRetryDelay {
		return unminedRecoveryRetryDelay
	}
	return interval
}

// recoverUnminedTransactions repairs missing template entries at the current tip.
// The channel listener owns serialization with block notifications and resets.
func (b *BlockAssembler) recoverUnminedTransactions(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// State and tip reads must not hold the assembly listener indefinitely.
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := b.subtreeProcessor.WaitForPendingBlocks(readCtx); err != nil {
		cancel()
		return false, err
	}
	state, err := b.blockchainClient.ReadFSMState(readCtx)
	if err != nil {
		cancel()
		return false, err
	}
	if state != blockchain.FSMStateRUNNING {
		cancel()
		return false, nil
	}
	bestHeader, bestMeta, err := b.blockchainClient.GetBestBlockHeader(readCtx)
	cancel()
	if err != nil {
		return false, err
	}
	header, height := b.CurrentBlock()
	if header == nil || bestHeader == nil || bestMeta == nil {
		return false, errors.NewProcessingError("unmined recovery requires a known chain tip")
	}
	repairPending := b.subtreeProcessor.RecoveryPending()
	if !repairPending && (height != bestMeta.Height || !header.Hash().IsEqual(bestHeader.Hash())) {
		// Let normal block/reorg processing catch up first. Recovery must not
		// introduce additional tip-moving resets or bypass their conflict rules.
		b.triggerReconcile()
		return false, errors.NewProcessingError("unmined recovery deferred until assembly reaches the chain tip")
	}

	started := time.Now()
	b.logger.Infof("[BlockAssembler] Recovering unmined transactions at height %d", height)
	hashes, err := b.scanUnminedRecoveryHashes(ctx)
	if err != nil {
		return false, err
	}
	wasBlocked := b.recoveryMiningBlocked.Swap(true)
	err = b.subtreeProcessor.RecoverUnmined(ctx, header, hashes,
		func(ctx context.Context, candidates []chainhash.Hash, accepted func(chainhash.Hash) bool) ([]*utxo.UnminedTransaction, error) {
			// The index scan may be long. Recheck authority and tip before the
			// dispatcher prepares any memory replacement. This remains admission
			// of one recovery pass, not a lease or a global STOP/drain barrier.
			checkCtx, stopCheck := context.WithTimeout(ctx, 5*time.Second)
			defer stopCheck()
			state, err := b.blockchainClient.ReadFSMState(checkCtx)
			if err != nil {
				return nil, err
			}
			if state != blockchain.FSMStateRUNNING {
				return nil, errors.NewProcessingError("unmined recovery deferred because the FSM is no longer RUNNING")
			}
			latest, _, err := b.blockchainClient.GetBestBlockHeader(checkCtx)
			if err != nil {
				return nil, err
			}
			if latest == nil || (!repairPending && !latest.Hash().IsEqual(header.Hash())) {
				b.triggerReconcile()
				return nil, errors.NewProcessingError("unmined recovery deferred because the chain tip changed")
			}
			return b.prepareUnminedRecovery(ctx, candidates, accepted)
		})
	if err != nil {
		if !wasBlocked && !b.subtreeProcessor.RecoveryPending() {
			b.recoveryMiningBlocked.Store(false)
		}
		return false, err
	}
	// Repair at the original anchor is necessary even when a block arrived
	// after a failed rebuild. Only a complete template may enter normal chain
	// movement; no mining work may escape from the repaired old tip meanwhile.
	checkCtx, stopCheck := context.WithTimeout(ctx, 5*time.Second)
	latest, _, checkErr := b.blockchainClient.GetBestBlockHeader(checkCtx)
	stopCheck()
	if checkErr == nil && latest != nil && latest.Hash().IsEqual(header.Hash()) {
		b.recoveryMiningBlocked.Store(false)
	} else {
		b.triggerReconcile()
	}
	b.logger.Infof("[BlockAssembler] Unmined transaction recovery completed in %s", time.Since(started))
	return true, nil
}

// The index only discovers candidates. Eligibility is checked again from fresh
// metadata on the dispatcher, together with queued and already assembled work.
// In particular, startup's unlock/cleanup behavior is never used live.
func (b *BlockAssembler) scanUnminedRecoveryHashes(ctx context.Context) (hashes []chainhash.Hash, resultErr error) {
	iterator, err := b.utxoStore.GetUnminedTxIterator()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := iterator.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := iterator.Next(ctx)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return hashes, iterator.Err()
		}
		for _, tx := range batch {
			if tx == nil || tx.Node == nil {
				return nil, errors.NewProcessingError("unmined recovery encountered missing transaction metadata")
			}
			if !tx.Skip {
				hashes = append(hashes, tx.Hash)
			}
		}
	}
}
