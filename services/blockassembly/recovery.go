package blockassembly

import (
	"bytes"
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/google/uuid"
)

// RecoveryClient is optional: ordinary block assembly clients retain their
// existing interface and asynchronous reset semantics.
type RecoveryClient interface {
	RecoveryState(context.Context) (replayrecovery.AssemblyState, error)
	RecoveryReset(context.Context) (replayrecovery.AssemblyState, error)
	RecoveryTransactions(context.Context, bool, func(string) error) (replayrecovery.AssemblyState, error)
}

type recoveryRequest struct {
	run  func() error
	done chan error
}
type recoveryResetResult struct {
	state replayrecovery.AssemblyState
	err   error
}

func (b *BlockAssembler) recoveryState() replayrecovery.AssemblyState {
	b.recoveryOnce.Do(func() { b.recoveryProcessID = uuid.NewString() })
	state := replayrecovery.AssemblyState{ProcessID: b.recoveryProcessID, ResetID: b.recoveryResetID.Load()}
	if header, height := b.CurrentBlock(); header != nil {
		state.Tip = replayrecovery.Tip{Hash: header.Hash().String(), Height: height}
	}
	return state
}

// withRecoveryOwner serializes recovery against the normal reset/reorg loop.
func (b *BlockAssembler) withRecoveryOwner(ctx context.Context, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := recoveryRequest{run: func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return run()
	}, done: make(chan error, 1)}
	select {
	case b.recoveryCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-req.done
}

func (b *BlockAssembler) RecoveryState(ctx context.Context) (state replayrecovery.AssemblyState, err error) {
	err = b.withRecoveryOwner(ctx, func() error {
		state = b.recoveryState()
		if state.Tip.Hash == "" {
			return errors.NewProcessingError("assembly tip unavailable")
		}
		return nil
	})
	return
}

// RecoveryReset waits for the actual ordinary reset outcome. Cancellation stops
// waiting; an accepted reset still completes on the service lifecycle context.
func (b *BlockAssembler) RecoveryReset(ctx context.Context) (replayrecovery.AssemblyState, error) {
	if err := ctx.Err(); err != nil {
		return replayrecovery.AssemblyState{}, err
	}
	result := make(chan recoveryResetResult, 1)
	select {
	case b.resetCh <- resetRequest{RecoveryCh: result}:
	case <-ctx.Done():
		return replayrecovery.AssemblyState{}, ctx.Err()
	}
	select {
	case r := <-result:
		return r.state, r.err
	case <-ctx.Done():
		return replayrecovery.AssemblyState{}, ctx.Err()
	}
}

// completeRecoveryReset reports the same operation outcome to every request
// coalesced by the existing reset loop. Failed resets never advance ResetID.
func (b *BlockAssembler) completeRecoveryReset(req resetRequest, err error) {
	if err == nil {
		b.recoveryResetID.Add(1)
	}
	result := recoveryResetResult{state: b.recoveryState(), err: err}
	complete := func(r resetRequest) {
		if r.ErrCh != nil {
			r.ErrCh <- err
		}
		if r.RecoveryCh != nil {
			r.RecoveryCh <- result
		}
	}
	for len(b.resetCh) > 0 {
		complete(<-b.resetCh)
	}
	complete(req)
}

func (b *BlockAssembler) RecoveryTransactions(ctx context.Context, candidate bool, visit func(string) error) (replayrecovery.AssemblyState, error) {
	var createCandidate func(context.Context) (*model.MiningCandidate, []*subtree.Subtree, error)
	if candidate {
		createCandidate = b.GetMiningCandidate
	}
	return b.streamRecovery(ctx, createCandidate, nil, visit)
}

func (b *BlockAssembler) streamRecovery(ctx context.Context, createCandidate func(context.Context) (*model.MiningCandidate, []*subtree.Subtree, error), begin func(replayrecovery.AssemblyState, uint64) error, visit func(string) error) (state replayrecovery.AssemblyState, err error) {
	err = b.withRecoveryOwner(ctx, func() error {
		var candidate *model.MiningCandidate
		var candidateTrees []*subtree.Subtree
		if createCandidate != nil {
			var err error
			candidate, candidateTrees, err = createCandidate(ctx)
			if err != nil {
				return err
			}
		}

		owner, ok := b.subtreeProcessor.(interface {
			RecoverySnapshot(context.Context, func(*model.BlockHeader, []*subtree.Subtree) error) error
		})
		if !ok {
			return errors.NewProcessingError("subtree processor does not support recovery snapshots")
		}
		return owner.RecoverySnapshot(ctx, func(header *model.BlockHeader, trees []*subtree.Subtree) error {
			state = b.recoveryState()
			if header == nil || header.Hash().String() != state.Tip.Hash {
				return errors.NewProcessingError("assembly and subtree tips disagree")
			}
			if candidate != nil {
				if !bytes.Equal(candidate.PreviousHash, header.Hash()[:]) || candidate.Height != state.Tip.Height+1 {
					return errors.NewProcessingError("fresh mining candidate tip disagrees with assembly")
				}
				candidateHash, err := chainhash.NewHash(candidate.Id)
				if err != nil {
					return errors.NewProcessingError("invalid mining candidate ID", err)
				}
				state.CandidateID = candidateHash.String()
				trees = candidateTrees
			}
			var count uint64
			for i, tree := range trees {
				for j, node := range tree.Nodes {
					if err := ctx.Err(); err != nil {
						return err
					}
					if node.Hash == subtree.CoinbasePlaceholderHashValue {
						if i != 0 || j != 0 {
							return errors.NewProcessingError("misplaced coinbase placeholder")
						}
						continue
					}
					count++
				}
			}
			if len(trees) > 0 && (len(trees[0].Nodes) == 0 || trees[0].Nodes[0].Hash != subtree.CoinbasePlaceholderHashValue) {
				return errors.NewProcessingError("candidate coinbase placeholder missing")
			}
			if candidate != nil && uint64(candidate.NumTxs) != count {
				return errors.NewProcessingError("fresh mining candidate transaction count mismatch")
			}
			if begin != nil {
				if err := begin(state, count); err != nil {
					return err
				}
			}
			for _, tree := range trees {
				for _, node := range tree.Nodes {
					if err := ctx.Err(); err != nil {
						return err
					}
					if node.Hash == subtree.CoinbasePlaceholderHashValue {
						continue
					}
					if err := visit(node.Hash.String()); err != nil {
						return err
					}
				}
			}
			after := b.recoveryState()
			after.CandidateID = state.CandidateID
			if after != state {
				return errors.NewProcessingError("assembly identity changed during snapshot")
			}
			return nil
		})
	})
	return
}
