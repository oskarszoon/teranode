package blockassembly

import (
	"context"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtree "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/stretchr/testify/require"
)

func TestRecoveryResetWaitsAndPropagatesOutcome(t *testing.T) {
	for _, failure := range []error{nil, errors.NewError("reset storage failure")} {
		b := &BlockAssembler{resetCh: make(chan resetRequest, 2)}
		result := make(chan error, 1)
		go func() { _, err := b.RecoveryReset(t.Context()); result <- err }()
		req := <-b.resetCh
		select {
		case <-result:
			t.Fatal("reset acknowledged before completion")
		case <-time.After(10 * time.Millisecond):
		}
		b.completeRecoveryReset(req, failure)
		require.ErrorIs(t, <-result, failure)
	}
}

func TestRecoveryResetCoalescedFailure(t *testing.T) {
	b := &BlockAssembler{resetCh: make(chan resetRequest, 2)}
	first := resetRequest{ErrCh: make(chan error, 1), RecoveryCh: make(chan recoveryResetResult, 1)}
	second := resetRequest{ErrCh: make(chan error, 1), RecoveryCh: make(chan recoveryResetResult, 1)}
	b.resetCh <- second
	failure := errors.NewError("reset failed")
	b.completeRecoveryReset(first, failure)
	require.ErrorIs(t, <-first.ErrCh, failure)
	require.ErrorIs(t, <-second.ErrCh, failure)
	require.ErrorIs(t, (<-first.RecoveryCh).err, failure)
	require.ErrorIs(t, (<-second.RecoveryCh).err, failure)
	require.Zero(t, b.recoveryState().ResetID)
}

func TestRecoveryResetCancellation(t *testing.T) {
	b := &BlockAssembler{resetCh: make(chan resetRequest, 2)}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := b.RecoveryReset(ctx); result <- err }()
	req := <-b.resetCh
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	// A departed caller cannot wedge the owner when the operation finishes.
	b.completeRecoveryReset(req, nil)
}

func TestRecoveryProcessIdentity(t *testing.T) {
	a, b := &BlockAssembler{}, &BlockAssembler{}
	require.NotEmpty(t, a.recoveryState().ProcessID)
	require.Equal(t, a.recoveryState().ProcessID, a.recoveryState().ProcessID)
	require.NotEqual(t, a.recoveryState().ProcessID, b.recoveryState().ProcessID)
}

func TestRecoveryCandidateAndAssemblyRemainderAtCurrentTip(t *testing.T) {
	initPrometheusMetrics()
	items := setupBlockAssemblyTest(t)
	b := items.blockAssembler
	_, _, genesis := setupBlockchainClient(t, items)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Real subtree publication acknowledgements, without Kafka/network services.
	go func() {
		for {
			select {
			case req := <-items.newSubtreeChan:
				if req.ErrChan != nil {
					select {
					case req.ErrChan <- nil:
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	require.NoError(t, b.startChannelListeners(ctx))
	t.Cleanup(func() { cancel(); b.wg.Wait() })
	var want []string
	for i := byte(1); i <= 6; i++ {
		hash := chainhash.Hash{i}
		want = append(want, hash.String())
		require.True(t, b.AddTxBatchIfRoom([]subtree.Node{{Hash: hash, Fee: 1, SizeInBytes: 1}}, []*subtree.TxInpoints{{ParentTxHashes: []chainhash.Hash{}}}))
		if i == 2 {
			require.Eventually(t, func() bool { return b.QueueLength() == 0 && b.TxCount() >= 2 }, 5*time.Second, 10*time.Millisecond)
			var incomplete []string
			snapshot, err := b.RecoveryTransactions(ctx, true, func(id string) error { incomplete = append(incomplete, id); return nil })
			require.NoError(t, err)
			require.NotEmpty(t, snapshot.CandidateID)
			require.Equal(t, want, incomplete, "an incomplete-only mining candidate includes every remainder transaction")
		}
	}
	require.Eventually(t, func() bool { return b.QueueLength() == 0 && b.TxCount() >= 6 }, 5*time.Second, 10*time.Millisecond)
	var assembly []string
	state, err := b.RecoveryTransactions(ctx, false, func(id string) error { assembly = append(assembly, id); return nil })
	require.NoError(t, err)
	require.Equal(t, want, assembly)
	require.Equal(t, genesis.Hash().String(), state.Tip.Hash)
	require.Empty(t, state.CandidateID)
	var candidate []string
	candidateState, err := b.RecoveryTransactions(ctx, true, func(id string) error { candidate = append(candidate, id); return nil })
	require.NoError(t, err)
	require.Equal(t, want[:3], candidate, "actual miner policy includes complete subtrees; full assembly snapshot covers the remainder")
	require.NotEmpty(t, candidateState.CandidateID)
	require.Equal(t, state.Tip, candidateState.Tip)
	require.Equal(t, state.ProcessID, candidateState.ProcessID)
	// Recovery follows the actual ordinary miner policy, which returns complete
	// subtrees once any exist. The remainder is audited by the full snapshot.
	ordinary, _, err := b.GetMiningCandidate(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, ordinary.NumTxs)
	require.Len(t, candidateState.CandidateID, 64)
	for _, corrupt := range []func(*model.MiningCandidate){
		func(mc *model.MiningCandidate) { mc.PreviousHash = make([]byte, 32) },
		func(mc *model.MiningCandidate) { mc.NumTxs++ },
	} {
		_, err := b.streamRecovery(ctx, func(ctx context.Context) (*model.MiningCandidate, []*subtree.Subtree, error) {
			mc, trees, err := b.GetMiningCandidate(ctx)
			if err == nil {
				corrupt(mc)
			}
			return mc, trees, err
		}, nil, func(string) error { t.Error("corrupt candidate must fail before emitting transactions"); return nil })
		require.Error(t, err)
	}
}

func TestRecoveryResetCompletesOrdinaryReset(t *testing.T) {
	initPrometheusMetrics()
	items := setupBlockAssemblyTest(t)
	b := items.blockAssembler
	setupBlockchainClient(t, items)
	b.SetSkipWaitForPendingBlocks(true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, b.startChannelListeners(ctx))
	t.Cleanup(func() { cancel(); b.wg.Wait() })
	before, err := b.RecoveryState(ctx)
	require.NoError(t, err)
	after, err := b.RecoveryReset(ctx)
	require.NoError(t, err)
	require.Equal(t, before.ProcessID, after.ProcessID)
	require.Equal(t, before.ResetID+1, after.ResetID)
	require.Equal(t, before.Tip, after.Tip)
}

func TestRecoveryResetActualStoreFailure(t *testing.T) {
	initPrometheusMetrics()
	items := setupBlockAssemblyTest(t)
	b := items.blockAssembler
	setupBlockchainClient(t, items)
	b.SetSkipWaitForPendingBlocks(true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, b.startChannelListeners(ctx))
	t.Cleanup(func() { cancel(); b.wg.Wait() })
	before, err := b.RecoveryState(ctx)
	require.NoError(t, err)
	require.NoError(t, items.utxoStore.Close(ctx))
	after, err := b.RecoveryReset(ctx)
	require.Error(t, err)
	require.Equal(t, before.ResetID, after.ResetID, "a failed real reset cannot produce a completed reset ID")
}
