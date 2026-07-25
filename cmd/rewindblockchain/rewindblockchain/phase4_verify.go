package rewindblockchain

import (
	"context"

	"github.com/bsv-blockchain/teranode/errors"
)

// phase4Verify runs cheap post-rewind consistency checks. It also asserts
// state["BlockPersisterHeight"] never increased across the run.
//
// Deeper checks (BlockID membership sweep, orphan subtree scan) are left out
// for now — they'd require iterator primitives we don't have on the UTXO
// store, or a directory walk on the blob store. Best addressed as a
// follow-up.
func (e *env) phase4Verify(ctx context.Context, pf *preflightResult) error {
	header, meta, err := e.blockchainStore.GetBestBlockHeader(ctx)
	if err != nil {
		return errors.NewStorageError("verify: GetBestBlockHeader", err)
	}

	if meta.Height != pf.target {
		return errors.NewProcessingError("verify: best block height %d != target %d", meta.Height, pf.target)
	}
	if header.Hash().String() != pf.targetHash.String() {
		return errors.NewProcessingError("verify: best block hash %s != target hash %s", header.Hash(), pf.targetHash)
	}

	if pf.persisterHeightKnown {
		current, known, readErr := e.readBlockPersisterHeight(ctx)
		if readErr != nil {
			return errors.NewStorageError("verify: read state[%s]", blockPersisterHeightKey, readErr)
		}

		if known && current > pf.persisterHeight {
			return errors.NewProcessingError("verify: state[%s] increased from %d to %d", blockPersisterHeightKey, pf.persisterHeight, current)
		}

		e.logger.Infof("verify: state[%s]=%d did not increase (was %d)", blockPersisterHeightKey, current, pf.persisterHeight)
	}

	e.logger.Infof("verify: best block at height %d matches target", pf.target)
	return nil
}
