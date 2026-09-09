package blockassembly

import (
	"context"
	"io"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/replayrecovery"
	"github.com/bsv-blockchain/teranode/services/blockassembly/blockassembly_api"
	"google.golang.org/grpc"
)

const recoveryPageSize = 1024

func recoveryStateMessage(s replayrecovery.AssemblyState) *blockassembly_api.RecoveryStateMessage {
	return &blockassembly_api.RecoveryStateMessage{ProcessId: s.ProcessID, TipHash: s.Tip.Hash, TipHeight: s.Tip.Height, ResetId: s.ResetID, CandidateId: s.CandidateID}
}
func recoveryNativeState(s *blockassembly_api.RecoveryStateMessage) (replayrecovery.AssemblyState, error) {
	if s == nil || s.ProcessId == "" {
		return replayrecovery.AssemblyState{}, errors.NewProcessingError("missing recovery state")
	}
	if h, err := chainhash.NewHashFromStr(s.TipHash); err != nil || h.String() != s.TipHash {
		return replayrecovery.AssemblyState{}, errors.NewProcessingError("invalid recovery tip")
	}
	if s.CandidateId != "" {
		if h, err := chainhash.NewHashFromStr(s.CandidateId); err != nil || h.String() != s.CandidateId {
			return replayrecovery.AssemblyState{}, errors.NewProcessingError("invalid mining candidate ID")
		}
	}
	return replayrecovery.AssemblyState{ProcessID: s.ProcessId, Tip: replayrecovery.Tip{Hash: s.TipHash, Height: s.TipHeight}, ResetID: s.ResetId, CandidateID: s.CandidateId}, nil
}

func (ba *BlockAssembly) RecoveryState(ctx context.Context, _ *blockassembly_api.EmptyMessage) (*blockassembly_api.RecoveryStateMessage, error) {
	state, err := ba.blockAssembler.RecoveryState(ctx)
	if err != nil {
		return nil, err
	}
	return recoveryStateMessage(state), nil
}
func (ba *BlockAssembly) RecoveryReset(ctx context.Context, _ *blockassembly_api.EmptyMessage) (*blockassembly_api.RecoveryStateMessage, error) {
	if ba.blockAssembler.unminedTransactionsLoading.Load() {
		ba.logger.Warnf("[RecoveryReset] service not ready - unmined transactions are still being loaded")
		return nil, errors.NewServiceError(errServiceNotReadyUnminedLoading)
	}
	state, err := ba.blockAssembler.RecoveryReset(ctx)
	if err != nil {
		return nil, err
	}
	return recoveryStateMessage(state), nil
}
func (ba *BlockAssembly) RecoveryTransactions(req *blockassembly_api.RecoveryTransactionsRequest, stream grpc.ServerStreamingServer[blockassembly_api.RecoveryPage]) error {
	var expected uint64
	batch := make([]string, 0, recoveryPageSize)
	var createCandidate func(context.Context) (*model.MiningCandidate, []*subtree.Subtree, error)
	if req.Candidate {
		createCandidate = ba.createMiningCandidateJob
	}
	state, err := ba.blockAssembler.streamRecovery(stream.Context(), createCandidate, func(s replayrecovery.AssemblyState, count uint64) error {
		expected = count
		return stream.Send(&blockassembly_api.RecoveryPage{State: recoveryStateMessage(s), Count: count})
	}, func(txid string) error {
		batch = append(batch, txid)
		if len(batch) == recoveryPageSize {
			if err := stream.Send(&blockassembly_api.RecoveryPage{Txids: batch}); err != nil {
				return err
			}
			batch = make([]string, 0, recoveryPageSize)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(batch) > 0 {
		if err = stream.Send(&blockassembly_api.RecoveryPage{Txids: batch}); err != nil {
			return err
		}
	}
	return stream.Send(&blockassembly_api.RecoveryPage{State: recoveryStateMessage(state), Count: expected, Complete: true})
}

func (c *Client) RecoveryState(ctx context.Context) (replayrecovery.AssemblyState, error) {
	s, err := c.client.RecoveryState(ctx, &blockassembly_api.EmptyMessage{})
	if err != nil {
		return replayrecovery.AssemblyState{}, err
	}
	return recoveryNativeState(s)
}
func (c *Client) RecoveryReset(ctx context.Context) (replayrecovery.AssemblyState, error) {
	s, err := c.client.RecoveryReset(ctx, &blockassembly_api.EmptyMessage{})
	if err != nil {
		return replayrecovery.AssemblyState{}, err
	}
	return recoveryNativeState(s)
}
func (c *Client) RecoveryTransactions(ctx context.Context, candidate bool, visit func(string) error) (replayrecovery.AssemblyState, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.client.RecoveryTransactions(ctx, &blockassembly_api.RecoveryTransactionsRequest{Candidate: candidate})
	if err != nil {
		return replayrecovery.AssemblyState{}, err
	}
	return consumeRecoveryStream(stream.Recv, candidate, visit)
}

func consumeRecoveryStream(recv func() (*blockassembly_api.RecoveryPage, error), candidate bool, visit func(string) error) (replayrecovery.AssemblyState, error) {
	first, err := recv()
	if err != nil {
		return replayrecovery.AssemblyState{}, err
	}
	state, err := recoveryNativeState(first.State)
	if err != nil {
		return state, err
	}
	if first.Complete || len(first.Txids) != 0 || candidate != (state.CandidateID != "") {
		return state, errors.NewProcessingError("invalid recovery snapshot opening page")
	}
	var count uint64
	for {
		page, err := recv()
		if err != nil {
			return state, errors.NewProcessingError("incomplete recovery snapshot", err)
		}
		if page.Complete {
			end, err := recoveryNativeState(page.State)
			if err != nil {
				return state, err
			}
			if end != state || page.Count != first.Count || count != first.Count || len(page.Txids) != 0 {
				return state, errors.NewProcessingError("recovery snapshot identity or count mismatch")
			}
			if _, err = recv(); err != io.EOF {
				return state, errors.NewProcessingError("unexpected data after recovery snapshot", err)
			}
			return state, nil
		}
		if page.State != nil || page.Count != 0 || len(page.Txids) == 0 || len(page.Txids) > recoveryPageSize {
			return state, errors.NewProcessingError("invalid recovery transaction page")
		}
		for _, txid := range page.Txids {
			if h, err := chainhash.NewHashFromStr(txid); err != nil || h.String() != txid {
				return state, errors.NewProcessingError("invalid recovery transaction ID")
			}
			count++
			if count > first.Count {
				return state, errors.NewProcessingError("recovery snapshot exceeds declared count")
			}
			if err = visit(txid); err != nil {
				return state, err
			}
		}
	}
}
