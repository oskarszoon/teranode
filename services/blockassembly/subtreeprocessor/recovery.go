package subtreeprocessor

import (
	"context"

	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
)

type recoveryRequest struct {
	ctx   context.Context
	visit func(*model.BlockHeader, []*subtree.Subtree) error
	done  chan error
}

// RecoverySnapshot lends the complete, ordered subtree inventory to visit while
// the control loop owns it. visit must not retain or mutate the trees, reenter
// the processor, or block without observing ctx. Pending ingress fails closed.
// Memory overhead is one pointer per subtree; transaction nodes are not copied.
func (stp *SubtreeProcessor) RecoverySnapshot(ctx context.Context, visit func(*model.BlockHeader, []*subtree.Subtree) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(stp.processorContext(), cancel)
	defer stop()
	req := recoveryRequest{ctx: ctx, visit: visit, done: make(chan error, 1)}
	select {
	case stp.recoveryCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-stp.processorContext().Done():
		return stp.processorContext().Err()
	}
	// Once accepted, wait for the callback to return even on cancellation. This
	// guarantees no callback can keep using caller-owned state after we return.
	return <-req.done
}

func (stp *SubtreeProcessor) recoverySnapshot(ctx context.Context, visit func(*model.BlockHeader, []*subtree.Subtree) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if stp.QueueLength() != 0 {
		return errors.NewProcessingError("recovery snapshot requires an empty ingest queue")
	}
	trees := make([]*subtree.Subtree, 0, len(stp.chainedSubtrees)+1)
	trees = append(trees, stp.chainedSubtrees...)
	if current := stp.currentSubtree.Load(); current != nil && len(current.Nodes) > 0 {
		trees = append(trees, current)
	}
	if err := visit(stp.currentBlockHeader.Load(), trees); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if stp.QueueLength() != 0 {
		return errors.NewProcessingError("ingest queue changed during recovery snapshot")
	}
	return nil
}
