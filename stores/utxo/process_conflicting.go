// //go:build aerospike

// Package utxo provides UTXO (Unspent Transaction Output) management for the BSV Blockchain Teranode implementation.
//
// This file implements conflicting transaction processing functionality for handling double-spend scenarios
// and transaction conflicts in the UTXO store. It requires the aerospike build tag.
package utxo

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-subtree"
	txmap "github.com/bsv-blockchain/go-tx-map"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	spendpkg "github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/errgroup"
)

// prometheusUtxoCounterConflictingDanglingRefs counts every dangling reference tolerated
// while resolving conflicts: a parent UTXO still records a spender whose own record has
// been removed (pruned/reorged/deleted). These free functions have no logger, so a counter
// is the observability surface — each tolerated ghost bumps it once. The "site" label
// distinguishes where the ghost was tolerated: "walk" (GetCounterConflictingTxHashes),
// "bfs" (GetConflictingChildren) and "repair" (the ProcessConflicting dangling-slot check).
var prometheusUtxoCounterConflictingDanglingRefs = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "teranode",
		Subsystem: "utxo",
		Name:      "counter_conflicting_dangling_refs",
		Help:      "Number of dangling spender references tolerated while resolving conflicts (site: walk, bfs, repair)",
	},
	[]string{
		"site", // where the ghost was tolerated: walk, bfs, repair
	},
)

// step5RetryDelays controls the bounded back-off when SetLocked(false) fails at the very
// last step of ProcessConflicting. The slice length is the number of attempts; the value
// at index i is the delay BEFORE attempt i (so index 0 is always zero — the first attempt
// is immediate). Rolling back at step 5 would create more inconsistency than retrying a
// simple state update — see ProcessConflicting for rationale.
//
// Declared as a package-level var so tests can shrink the delays.
var step5RetryDelays = []time.Duration{0, 50 * time.Millisecond, 200 * time.Millisecond}

// ProcessConflicting is a method to process conflicting transactions
// We got a txp (parent), txa and txb. txa and txb are both spending txp[5].
//
// tx_original is in block 102a
// - tx_original.input[0] = tx_parent1[5]
// - tx_original.input[1] = tx_parent2[6]
//
// tx_original_child1 is in block 102a
// - tx_original_child1.input[0] = tx_parent4[0]
//
// tx_double_spend is in block 102b --> block 103b --> block 104b
// - tx_double_spend.input[0] = tx_parent1[5] - spent by tx_original
// - tx_double_spend.input[1] = tx_parent3[1] - unspent
//
// ReplaceSpend(txb, []chainhash.Hash{txa})
//
// What happens with tx_parent2[6]?
//
/*
5 phase commit
 - 1: mark tx_original and all it's children as conflicting
 - 2: un-spend tx_original (update tx_parent1 & tx_parent2 utxos) and all it's children (tx_parent4 from tx_original_child1),
      marking all unspent txs as not spendable (tx_parent1 & tx_parent2 & tx_parent4)
 - 3: spend tx_double_spend as normal (ignoring the not spendable flag)
 - 4: mark tx_double_spend as not conflicting
 - 5: mark tx_parent1 & tx_parent2 & tx_parent4 as spendable again
*/
// ProcessConflicting returns:
//   - losingTxHashesMap: hashes of txs displaced by the winners (the immediate
//     counter-conflicting set from GetCounterConflicting). Used by callers to
//     mark losers in subtrees / drop them from upstream paths.
//   - allMarkedConflicting: every hash marked Conflicting=true during this run,
//     in BFS order — losers + every descendant the cascade reached. Callers
//     (notably block assembly) need this superset to populate a conflictingMap
//     so the queue→subtree dequeue path can reject children of conflicting
//     parents that arrive after the cascade has run.
func ProcessConflicting(ctx context.Context, s Store, blockHeight uint32, blockHash chainhash.Hash, conflictingTxHashes []chainhash.Hash,
	processedConflictingHashesMap map[chainhash.Hash]struct{}) (losingTxHashesMap txmap.TxMap, allMarkedConflicting []chainhash.Hash, err error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "ProcessConflicting")

	defer deferFn()

	// Crash-safety write-ahead log (#861): record the intent durably BEFORE any
	// state mutation, and remove it once the operation completes successfully. A
	// SIGKILL between steps (which bypasses the in-process rollback below) leaves
	// the intent behind for BlockAssembler startup replay. An in-process failure
	// also leaves it — the deferred rollback unwinds the partial state and a
	// restart re-attempts the (idempotent) operation from the restored precondition.
	// A failed Begin aborts before mutating anything: without a durable intent we
	// cannot guarantee crash recovery, so the operation must not proceed.
	walIntent := ConflictIntent{
		Kind:        ConflictIntentForward,
		BlockHeight: blockHeight,
		BlockHash:   blockHash,
		TxHashes:    conflictingTxHashes,
		StartedAt:   time.Now().UnixNano(),
	}
	if beginErr := s.BeginConflictIntent(ctx, walIntent); beginErr != nil {
		return nil, nil, errors.NewProcessingError("[ProcessConflicting] failed to record WAL intent before processing", beginErr)
	}

	// Registered before the rollback defer below, so it runs AFTER it (LIFO) and
	// observes the final err (including a rollback-failure escalation). Only a
	// genuinely successful operation removes the intent; otherwise it persists for
	// replay. Completion is best-effort: a failed delete just leaves an intent
	// that replays idempotently on the next restart.
	defer func() {
		if err == nil {
			_ = s.CompleteConflictIntent(ctx, walIntent.IntentID())
		}
	}()

	// State for the deferred compensating rollback. Each commit phase flips a flag; the
	// deferred block reads them on the way out and undoes whatever happened — see #4561.
	// allMarkedHashes mirrors the allMarkedConflicting named return but is read by the
	// deferred block; a `return nil, nil, err` in the error paths clobbers the named
	// return before the deferred runs, so we keep a parallel copy that survives.
	var (
		step1Committed        bool
		step2Committed        bool
		step4Committed        bool
		step5Failed           bool // distinct from "not committed" — rollback is intentionally skipped
		affectedParentSpends  []*Spend
		markedAsNotSpendable  []chainhash.Hash
		step3SuccessfulSpends []*Spend
		allMarkedHashes       []chainhash.Hash
	)

	defer func() {
		if err == nil || !step1Committed {
			return
		}

		// Step 5 (SetLocked false) is the last simple state update. Steps 1-4 are correct
		// at this point; rolling back would re-introduce conflicting flags and unspend the
		// winner — strictly worse. Surface the error and let the operator unlock manually.
		if step5Failed {
			return
		}

		rollbackErr := rollbackProcessConflicting(ctx, s, conflictingTxHashes,
			allMarkedHashes, markedAsNotSpendable, step3SuccessfulSpends, blockHeight,
			step2Committed, step4Committed)
		if rollbackErr != nil {
			err = errors.NewProcessingError("[ProcessConflicting] MANUAL INTERVENTION REQUIRED: original=%v rollback=%v", err, rollbackErr)
		}

		losingTxHashesMap = nil
		allMarkedConflicting = nil
	}()

	// 0. Get the transactions, check they are conflicting
	winningTxs := make([]*bt.Tx, len(conflictingTxHashes))

	// losingTxHashesPerConflictingTx is a slice of slices, each slice contains the hashes of the transactions that are conflicting
	// with the winning transaction at the same index in the winningTxs slice
	losingTxHashesPerConflictingTx := make([][]chainhash.Hash, len(conflictingTxHashes))
	losingTxHashesPerConflictingTxCount := atomic.Int64{}

	g, gCtx := errgroup.WithContext(ctx)

	for idx, txHash := range conflictingTxHashes {
		idx := idx
		txHash := txHash

		if txHash.Equal(subtree.CoinbasePlaceholderHashValue) {
			// the counter-conflicting tx is frozen, we should not process anything further
			return nil, nil, errors.NewProcessingError("[ProcessConflicting][%s] tx is frozen", txHash.String())
		}

		g.Go(func() error {
			txMeta, err := s.Get(gCtx, &txHash, fields.Tx, fields.BlockIDs, fields.Conflicting)
			if err != nil {
				return errors.NewProcessingError("[ProcessConflicting][%s] error getting tx", txHash.String(), err)
			}

			// A missing record surfaces as (nil, nil) on some backends (e.g. Aerospike
			// get returns nil for a not-found tx). Guard before dereferencing txMeta —
			// WAL replay can feed a winner hash whose tx was pruned between crash and
			// restart, and a clean error there is logged+counted by the replay path
			// rather than panicking node startup. Mirrors the nil guard in
			// ReverseProcessConflicting. Note: only a nil meta means "not found"; a
			// non-nil meta with a nil Tx is left to the existing flow (callers may
			// supply only the fields they need).
			if txMeta == nil {
				return errors.NewTxNotFoundError("[ProcessConflicting][%s] winning tx not found", txHash.String())
			}

			// the transaction should be marked as conflicting, otherwise it shouldn't be in this process
			// unless it was already processed in this run, then it will be in the processedConflictingHashesMap.
			// This can occur when a transaction is in multiple forks, and we are moving back from one fork to another
			// and the transaction was already processed in the previous fork.
			if _, alreadyProcessed := processedConflictingHashesMap[txHash]; !txMeta.Conflicting && !alreadyProcessed {
				return errors.NewProcessingError("[ProcessConflicting][%s] tx is not conflicting", txHash.String())
			}

			// get the counter conflicting transactions for the current transaction
			// this includes all the children of the conflicting transaction
			if losingTxHashesPerConflictingTx[idx], err = s.GetCounterConflicting(gCtx, txHash); err != nil {
				return errors.NewProcessingError("[ProcessConflicting][%s] error getting counter conflicting txs", txHash.String(), err)
			}

			winningTxs[idx] = txMeta.Tx

			losingTxHashesPerConflictingTxCount.Add(int64(len(losingTxHashesPerConflictingTx[idx])))

			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return nil, nil, err
	}

	// create a unique list of all the losing tx hashes
	losingTxHashesMap = txmap.NewSplitSwissMap(int(losingTxHashesPerConflictingTxCount.Load()))

	for _, hashes := range losingTxHashesPerConflictingTx {
		for _, hash := range hashes {
			// an error will be returned if the hash already exists in the map
			// we don't really care, we just need the unique hashes
			_ = losingTxHashesMap.Put(hash, 1)
		}
	}

	losingTxHashes := losingTxHashesMap.Keys()

	// - 1: mark all losingTxHashesPerConflictingTx as conflicting + all its spending transactions recursively.
	//   allMarkedConflicting is the BFS expansion: every hash now flagged Conflicting=true. Forwarded to callers so
	//   the block-assembly conflictingMap can include the cascaded descendants (not just the immediate losers).
	affectedParentSpends, allMarkedHashes, err = MarkConflictingRecursively(ctx, s, losingTxHashes)
	if err != nil {
		return nil, nil, err
	}

	allMarkedConflicting = allMarkedHashes
	step1Committed = true

	// A ghost loser — a spender whose record was removed while the parent still records
	// its spend — is excluded from the losing set by GetCounterConflicting, so step 1
	// produced no unspend entry for its slot. Left alone, that stale slot would make the
	// winner's spend in step 3 fail as a double-spend. Detect each winner input whose
	// recorded spender no longer exists and queue an explicit unspend so step 2 clears it.
	markedSet := make(map[chainhash.Hash]struct{}, len(allMarkedHashes))
	for _, h := range allMarkedHashes {
		markedSet[h] = struct{}{}
	}

	danglingSpends, dErr := collectDanglingWinnerInputSpends(ctx, s, winningTxs, conflictingTxHashes, markedSet)
	if dErr != nil {
		err = dErr
		return nil, nil, err
	}

	affectedParentSpends = append(affectedParentSpends, danglingSpends...)

	// - 2: un-spend txa, marking the input txs as not spendable (txp & txq)
	if err = s.Unspend(ctx, affectedParentSpends, true); err != nil {
		return nil, nil, errors.NewProcessingError("error unspending affected parent spends", err)
	}

	step2Committed = true

	// get the unique hashes of the transactions that were marked as not spendable
	markedAsNotSpendableHashesUnique := make(map[chainhash.Hash]struct{})
	for _, spend := range affectedParentSpends {
		markedAsNotSpendableHashesUnique[*spend.TxID] = struct{}{}
	}

	markedAsNotSpendable = make([]chainhash.Hash, 0, len(markedAsNotSpendableHashesUnique))
	for hash := range markedAsNotSpendableHashesUnique {
		markedAsNotSpendable = append(markedAsNotSpendable, hash)
	}

	// - 3: spend tx_double_spend as normal (ignoring the not spendable flag)
	var tErr *errors.Error

	for _, tx := range winningTxs {
		spends, spendErr := s.Spend(ctx, tx, blockHeight, IgnoreFlags{
			IgnoreConflicting: true,
			IgnoreLocked:      true,
		})
		// Capture per-input partial successes regardless of overall outcome so the rollback
		// can undo them via Unspend(false) (parents at step 3 entry were unlocked-by-us, so
		// the unspend MUST NOT relock).
		for _, sp := range spends {
			if sp != nil && sp.Err == nil {
				step3SuccessfulSpends = append(step3SuccessfulSpends, sp)
			}
		}

		if spendErr != nil {
			if errors.As(spendErr, &tErr) {
				for _, spend := range spends {
					if spend.Err != nil {
						tErr.SetWrappedErr(spend.Err)
					}
				}
			}

			err = spendErr
			return nil, nil, err
		}
	}

	// - 4: mark txb as not conflicting
	if _, _, err = s.SetConflicting(ctx, conflictingTxHashes, false); err != nil {
		return nil, nil, err
	}

	step4Committed = true

	// - 5: mark txp & txq as spendable again. Step 5 is a near-final state update; rolling
	// the entire commit back now would re-introduce the very inconsistencies we just fixed.
	// Retry with bounded back-off and surface any persistent failure for operator action.
	if err = setLockedWithRetry(ctx, s, markedAsNotSpendable, false); err != nil {
		step5Failed = true
		return nil, nil, err
	}

	return losingTxHashesMap, allMarkedConflicting, nil
}

// ReverseProcessConflicting undoes the side effects of a previous
// ProcessConflicting call so the UTXO store is restored to the state it would
// have been in had that call never happened. It is the inverse of
// ProcessConflicting and is meant to run inside moveBackBlock when the block
// whose subtree was processed is being removed from the chain.
//
// Inputs: demotedTxHashes is the list of txs originally passed to
// ProcessConflicting as winners (i.e. subtree.ConflictingNodes from the block
// being moved back).
//
// Operation for each demoted tx D, per input (parentHash, vout):
//
//  1. Pick a counter tx C from parent.ConflictingChildren that
//     (a) is not in demotedTxHashes, (b) is currently Conflicting=true, and
//     (c) spends the same (parentHash, vout) as D. C is the original mempool
//     spender that ProcessConflicting demoted.
//  2. Mark D and its spending descendants Conflicting=true (cascade). This
//     undoes Phase 4 of the original call for D and rebuilds the cascade for
//     D's descendants in case any were added after ProcessConflicting ran.
//  3. Unspend(D's inputs) so parent.SpendingDatas no longer points at D.
//  4. Spend(C's tx) so parent.SpendingDatas[vout] points at C again.
//  5. UnmarkConflictingRecursively(C) so C and its descendants flip back to
//     Conflicting=false.
//
// Demoted txs whose Conflicting flag is already true are skipped — the
// previous reverse already ran. Demoted txs with no counter currently
// conflicting are skipped at the per-input level: nothing to restore.
//
// Returns:
//   - cascadedToConflicting: every hash whose Conflicting flag this call flipped
//     to true (the demoted txs + their spending descendants). Callers feed
//     this into the moveForward dequeue path so the queue evicts the
//     unmined-side cascade.
//   - allTouched: union of cascadedToConflicting and the un-cascade hashes
//     whose flag flipped back to false (counter + descendants). Callers
//     feed this into processedConflictingHashesMap so the subsequent
//     moveForwardBlock pass skips ProcessConflicting on these hashes —
//     re-running it would double-apply the UTXO swap and fail.
func ReverseProcessConflicting(ctx context.Context, s Store, blockHeight uint32, blockHash chainhash.Hash, demotedTxHashes []chainhash.Hash) (cascadedToConflicting []chainhash.Hash, allTouched []chainhash.Hash, err error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "ReverseProcessConflicting")
	defer deferFn()

	if len(demotedTxHashes) == 0 {
		return nil, nil, nil
	}

	// Crash-safety write-ahead log (#861): record the reverse intent before any
	// state mutation and remove it on successful completion. See ProcessConflicting
	// for the full rationale. ReverseProcessConflicting self-heals partial state via
	// the isReverseFullyApplied guard, so replay from any step boundary is safe.
	walIntent := ConflictIntent{
		Kind:        ConflictIntentReverse,
		BlockHeight: blockHeight,
		BlockHash:   blockHash,
		TxHashes:    demotedTxHashes,
		StartedAt:   time.Now().UnixNano(),
	}
	if beginErr := s.BeginConflictIntent(ctx, walIntent); beginErr != nil {
		return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting] failed to record WAL intent before processing", beginErr)
	}

	defer func() {
		if err == nil {
			_ = s.CompleteConflictIntent(ctx, walIntent.IntentID())
		}
	}()

	demotedSet := make(map[chainhash.Hash]struct{}, len(demotedTxHashes))
	for _, h := range demotedTxHashes {
		demotedSet[h] = struct{}{}
	}

	cascadedConflictingSet := make(map[chainhash.Hash]struct{}, 2*len(demotedTxHashes))
	touchedSet := make(map[chainhash.Hash]struct{}, 2*len(demotedTxHashes))

	for i := range demotedTxHashes {
		demotedHash := demotedTxHashes[i]

		if demotedHash.Equal(subtree.CoinbasePlaceholderHashValue) {
			continue
		}

		demotedMeta, getErr := s.Get(ctx, &demotedHash, fields.Tx, fields.Conflicting)
		if getErr != nil {
			return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error getting demoted tx meta", demotedHash.String(), getErr)
		}

		if demotedMeta == nil || demotedMeta.Tx == nil {
			continue
		}

		if demotedMeta.Conflicting {
			// D.Conflicting=true alone is NOT sufficient evidence the
			// reverse is fully applied — a previous call may have failed
			// after step 1 (Mark) succeeded but before step 3 (Spend(C))
			// completed, leaving parent.SpendingDatas[vout] empty (cleared
			// in step 2). On retry we must complete the missing
			// Spend(C)+Unmark(C) work, not short-circuit. Confirm full
			// completion via observable parent state: every input of D must
			// have parent.SpendingDatas[vout] pointing to a non-nil,
			// non-D spender. If any input shows nil or still points at D,
			// fall through and re-run the steps below; the Mark and
			// Unspend are idempotent on the already-applied state.
			fullyReversed, checkErr := isReverseFullyApplied(ctx, s, demotedMeta.Tx, demotedHash)
			if checkErr != nil {
				return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error confirming reverse completion via parent state", demotedHash.String(), checkErr)
			}

			if fullyReversed {
				continue
			}
		}

		// Step 1: identify counters per input.
		countersToPromote, selErr := selectCountersForDemotedTx(ctx, s, demotedMeta.Tx, demotedSet)
		if selErr != nil {
			return nil, nil, selErr
		}

		// Step 2: re-mark D + descendants Conflicting=true.
		_, markedOrder, markErr := MarkConflictingRecursively(ctx, s, []chainhash.Hash{demotedHash})
		if markErr != nil {
			return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error marking demoted tx + descendants conflicting", demotedHash.String(), markErr)
		}

		for _, h := range markedOrder {
			cascadedConflictingSet[h] = struct{}{}
			touchedSet[h] = struct{}{}
		}

		// Step 3: unspend D's input spends so parent.SpendingDatas[vout]
		// no longer points at D.
		demotedSpends, buildErr := spendsForTx(demotedMeta.Tx)
		if buildErr != nil {
			return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error building unspend records", demotedHash.String(), buildErr)
		}

		if unspendErr := s.Unspend(ctx, demotedSpends, false); unspendErr != nil {
			return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error unspending demoted tx inputs", demotedHash.String(), unspendErr)
		}

		// Step 4 & 5: per counter, re-spend its inputs and un-cascade.
		for _, counterHash := range countersToPromote {
			counterMeta, getCounterErr := s.Get(ctx, &counterHash, fields.Tx)
			if getCounterErr != nil {
				return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error getting counter tx %s", demotedHash.String(), counterHash.String(), getCounterErr)
			}

			if counterMeta == nil || counterMeta.Tx == nil {
				continue
			}

			if _, spendErr := s.Spend(ctx, counterMeta.Tx, blockHeight, IgnoreFlags{
				IgnoreConflicting: true,
				IgnoreLocked:      true,
			}); spendErr != nil {
				return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error spending counter %s", demotedHash.String(), counterHash.String(), spendErr)
			}

			unmarked, unmarkErr := UnmarkConflictingRecursively(ctx, s, []chainhash.Hash{counterHash})
			if unmarkErr != nil {
				return nil, nil, errors.NewProcessingError("[ReverseProcessConflicting][%s] error un-marking counter %s + descendants", demotedHash.String(), counterHash.String(), unmarkErr)
			}

			for _, h := range unmarked {
				touchedSet[h] = struct{}{}
			}
		}
	}

	if len(touchedSet) == 0 {
		return nil, nil, nil
	}

	cascadedToConflicting = make([]chainhash.Hash, 0, len(cascadedConflictingSet))
	for h := range cascadedConflictingSet {
		cascadedToConflicting = append(cascadedToConflicting, h)
	}

	allTouched = make([]chainhash.Hash, 0, len(touchedSet))
	for h := range touchedSet {
		allTouched = append(allTouched, h)
	}

	return cascadedToConflicting, allTouched, nil
}

// isReverseFullyApplied returns true iff every input of the demoted tx D has
// parent.SpendingDatas[vout] populated with a non-nil spender C that is not D
// itself AND C is no longer flagged Conflicting. Used as the post-D.Conflicting=true
// guard to distinguish a fully applied reverse from a partial one.
//
// Returns false (no error) on:
//   - any input whose parent.SpendingDatas[vout] is nil (post-Unspend, pre-Spend
//     state — Spend(C) failed last time around)
//   - any input whose parent.SpendingDatas[vout].TxID equals demotedHash
//     (Unspend never ran successfully for that input)
//   - any input whose recorded spender C is still Conflicting=true — this is the
//     crash-between-Spend(C)-and-Unmark(C) state (#861): parent[vout] already
//     points at C, but the final UnmarkConflictingRecursively(C) never ran, so
//     C is wrongly still conflicting. Returning false re-runs the steps, whose
//     Mark(D)/Unspend(D)/Spend(C) are idempotent and whose Unmark(C) finishes the job.
//   - any input whose parent has no SpendingDatas slice or is shorter than vout,
//     or whose recorded spender record is missing (defensive: cannot confirm full
//     application, so retry rather than claim done)
//
// Returns true only when ALL inputs unambiguously have a non-D, non-conflicting
// spender. An error is surfaced for any Get failure — that's a store-level
// problem, not a state question, and the caller must abort the reverse rather
// than make assumptions.
func isReverseFullyApplied(ctx context.Context, s Store, demotedTx *bt.Tx, demotedHash chainhash.Hash) (bool, error) {
	for _, input := range demotedTx.Inputs {
		parentHash := input.PreviousTxIDChainHash()
		vout := input.PreviousTxOutIndex

		parentMeta, err := s.Get(ctx, parentHash, fields.Utxos)
		if err != nil {
			return false, errors.NewProcessingError("[isReverseFullyApplied][%s] error getting parent %s meta", demotedHash.String(), parentHash.String(), err)
		}

		if parentMeta == nil {
			return false, nil
		}

		if int(vout) >= len(parentMeta.SpendingDatas) {
			return false, nil
		}

		sd := parentMeta.SpendingDatas[vout]
		if sd == nil || sd.TxID == nil {
			return false, nil
		}

		if sd.TxID.IsEqual(&demotedHash) {
			return false, nil
		}

		// The recorded spender C must also be non-conflicting. A crash between
		// Spend(C) and UnmarkConflictingRecursively(C) leaves parent[vout]->C
		// with C still Conflicting=true; that is NOT a fully-applied reverse.
		spenderMeta, err := s.Get(ctx, sd.TxID, fields.Conflicting)
		if err != nil {
			return false, errors.NewProcessingError("[isReverseFullyApplied][%s] error getting recorded spender %s meta", demotedHash.String(), sd.TxID.String(), err)
		}

		if spenderMeta == nil || spenderMeta.Conflicting {
			return false, nil
		}
	}

	return true, nil
}

// selectCountersForDemotedTx walks the inputs of a demoted tx and returns the
// set of counter txs to restore as canonical spenders.
//
// For each (parent, vout) the demoted tx spends, candidates are entries in
// parent.ConflictingChildren that:
//
//  1. are not themselves being demoted in this call,
//  2. are currently Conflicting=true (the previous ProcessConflicting demoted
//     them), and
//  3. actually spend the same (parent, vout) — guards against sibling-output
//     spenders that wouldn't conflict with this demoted tx.
//
// When more than one candidate matches per input, the function picks the one
// with the lowest CreatedAt (first-seen mempool spender, set once at insert
// in both backends — Aerospike at stores/utxo/aerospike/create.go:706, SQL
// via the inserted_at column populated by getUnbatched / batchDecorateChunk).
// Tiebreak on equal CreatedAt is lexicographic hash compare so the choice is
// deterministic across nodes and across runs.
//
// The same counter may legitimately spend several of the demoted tx's
// inputs; the function deduplicates so we only Spend()/Unmark() it once.
//
// Returns nil with no error when no candidate passes the filters for any
// input — caller demotes D + descendants but leaves SpendingDatas[vout]
// untouched for that input. ReverseProcessConflicting's caller can rely on
// the returned list being the exact set to feed Spend/UnmarkConflicting.
func selectCountersForDemotedTx(ctx context.Context, s Store, demotedTx *bt.Tx, demotedSet map[chainhash.Hash]struct{}) ([]chainhash.Hash, error) {
	seen := make(map[chainhash.Hash]struct{})

	result := make([]chainhash.Hash, 0)

	for _, input := range demotedTx.Inputs {
		parentHash := input.PreviousTxIDChainHash()
		vout := input.PreviousTxOutIndex

		parentMeta, err := s.Get(ctx, parentHash, fields.ConflictingChildren)
		if err != nil {
			return nil, errors.NewProcessingError("[selectCountersForDemotedTx][%s] error getting parent meta", parentHash.String(), err)
		}

		if parentMeta == nil {
			continue
		}

		var (
			best          *chainhash.Hash
			bestCreatedAt int64
		)

		for j := range parentMeta.ConflictingChildren {
			candidate := parentMeta.ConflictingChildren[j]

			if _, demoted := demotedSet[candidate]; demoted {
				continue
			}

			if _, dup := seen[candidate]; dup {
				continue
			}

			candidateMeta, err := s.Get(ctx, &candidate, fields.Tx, fields.Conflicting, fields.CreatedAt)
			if err != nil {
				return nil, errors.NewProcessingError("[selectCountersForDemotedTx][%s] error getting candidate counter", candidate.String(), err)
			}

			if candidateMeta == nil || candidateMeta.Tx == nil {
				continue
			}

			if !candidateMeta.Conflicting {
				continue
			}

			if !candidateSpendsOutput(candidateMeta.Tx, parentHash, vout) {
				continue
			}

			// First-seen wins. Pin the candidate by value because parentMeta is
			// rewritten under us via deeper Get calls.
			candidateCopy := candidate

			if best == nil || isOlderCounter(candidateMeta.CreatedAt, candidateCopy, bestCreatedAt, *best) {
				best = &candidateCopy
				bestCreatedAt = candidateMeta.CreatedAt
			}
		}

		if best != nil {
			seen[*best] = struct{}{}
			result = append(result, *best)
		}
	}

	return result, nil
}

// isOlderCounter returns true when (aCreatedAt, aHash) sorts strictly before
// (bCreatedAt, bHash). CreatedAt comes first, hash bytes are the tiebreak.
// A candidate whose CreatedAt is zero (missing on legacy records) is treated
// as newer than any candidate with a real timestamp — we never prefer the
// unknown-vintage record over one with a known first-seen time.
func isOlderCounter(aCreatedAt int64, aHash chainhash.Hash, bCreatedAt int64, bHash chainhash.Hash) bool {
	switch {
	case aCreatedAt == 0 && bCreatedAt == 0:
		// fall through to hash compare
	case aCreatedAt == 0:
		return false
	case bCreatedAt == 0:
		return true
	case aCreatedAt < bCreatedAt:
		return true
	case aCreatedAt > bCreatedAt:
		return false
	}

	// equal CreatedAt — lex compare the hash bytes for determinism.
	for i := range aHash {
		if aHash[i] != bHash[i] {
			return aHash[i] < bHash[i]
		}
	}

	return false
}

func candidateSpendsOutput(tx *bt.Tx, parentHash *chainhash.Hash, vout uint32) bool {
	for _, in := range tx.Inputs {
		if in.PreviousTxOutIndex == vout && in.PreviousTxIDChainHash().IsEqual(parentHash) {
			return true
		}
	}

	return false
}

// spendsForTx builds the []*Spend records for tx.Inputs in the same shape
// Unspend / Spend expect.
func spendsForTx(tx *bt.Tx) ([]*Spend, error) {
	spends := make([]*Spend, len(tx.Inputs))

	for i, input := range tx.Inputs {
		utxoHash, err := util.UTXOHashFromInput(input)
		if err != nil {
			return nil, err
		}

		spends[i] = &Spend{
			TxID:         input.PreviousTxIDChainHash(),
			Vout:         input.PreviousTxOutIndex,
			UTXOHash:     utxoHash,
			SpendingData: spendpkg.NewSpendingData(tx.TxIDChainHash(), i),
		}
	}

	return spends, nil
}

// UnmarkConflictingRecursively flips Conflicting=false on the given txs and
// every spending descendant reached via BFS over SpendingDatas. Inverse of
// MarkConflictingRecursively.
//
// Returns the BFS-ordered list of every hash whose flag this call cleared
// (the input set plus every descendant the cascade reached).
func UnmarkConflictingRecursively(ctx context.Context, s Store, hashes []chainhash.Hash) ([]chainhash.Hash, error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "UnmarkConflictingRecursively")
	defer deferFn()

	toProcess := hashes

	visited := make(map[chainhash.Hash]struct{}, len(hashes))
	clearedOrder := make([]chainhash.Hash, 0, len(hashes))

	for _, h := range hashes {
		if _, ok := visited[h]; !ok {
			visited[h] = struct{}{}
			clearedOrder = append(clearedOrder, h)
		}
	}

	for len(toProcess) > 0 {
		_, spendingChildTxs, err := s.SetConflicting(ctx, toProcess, false)
		if err != nil {
			return nil, err
		}

		// filter out already-visited hashes to prevent infinite loops
		nextBatch := spendingChildTxs[:0]
		for _, child := range spendingChildTxs {
			if _, ok := visited[child]; !ok {
				visited[child] = struct{}{}
				clearedOrder = append(clearedOrder, child)
				nextBatch = append(nextBatch, child)
			}
		}

		toProcess = nextBatch
	}

	return clearedOrder, nil
}

// rollbackProcessConflicting reverses the committed phases of ProcessConflicting in
// reverse-of-forward order. It is best-effort: each sub-step's error is collected via
// errors.Join so subsequent sub-steps still run. If the caller sees a non-nil return
// the UTXO store may be in an inconsistent state — see ProcessConflicting deferred
// block which tags this as MANUAL INTERVENTION REQUIRED.
//
// Cleared-ghost-slot note: collectDanglingWinnerInputSpends now fails closed BEFORE
// step 2 for every winner-input slot whose step-3 spend is deterministically doomed
// (frozen sentinel, live third-party spender, pruner-marked ghost). So the only way a
// dangling ghost slot is cleared at step 2 is when step 3 can succeed — a slot the
// winner is entitled to. Rollback cannot re-Spend a cleared ghost slot (the ghost has
// no tx body to restore), but that only leaves a residual strand when step 2 or step 3
// fails transiently AFTER a ghost slot was cleared. That window is transient: the empty
// slot yields no dangling entry on the next attempt, so WAL replay converges (empty slot
// → nothing to clear → the winner spends it). The previously-deterministic
// unspent+unlocked exposure — where a doomed-anyway resolution stranded an OTHER input's
// cleared ghost — is gone.
func rollbackProcessConflicting(ctx context.Context, s Store, conflictingTxHashes,
	allMarkedHashes, markedAsNotSpendable []chainhash.Hash,
	step3SuccessfulSpends []*Spend, blockHeight uint32, step2Committed, step4Committed bool) error {
	var rollbackErr error

	// 1. Undo step 4 first (re-mark winners as conflicting) so the system briefly observes
	// "everything is conflicting" rather than "winner accepted but parents still missing
	// their spend record".
	if step4Committed {
		if _, _, e := s.SetConflicting(ctx, conflictingTxHashes, true); e != nil {
			rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 4 (re-mark winners conflicting) failed", e))
		}
	}

	// 2. Undo step 3 partial spends. Pass flagAsLocked=false: step 3 used IgnoreLocked, so
	// re-locking here is meaningless — the parents are still locked from step 2 and that
	// lock will be cleared together at step 5 of the rollback (SetLocked false below).
	if len(step3SuccessfulSpends) > 0 {
		if e := s.Unspend(ctx, step3SuccessfulSpends, false); e != nil {
			rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 3 (unspend partial winning spends) failed", e))
		}
	}

	// 3. Undo step 2: re-spend every tx the cascade marked conflicting so the original
	// spending_data is restored on affectedParentSpends. We iterate allMarkedHashes — not
	// just losingTxHashes — because MarkConflictingRecursively does a BFS that reaches
	// spending descendants of the counter-conflicting set, and step 2's Unspend covered
	// the parents of that whole cascade. Skipping descendants would leave their parent
	// UTXOs unspent and the store in a torn state. Parents are still locked here, so we
	// MUST set IgnoreLocked; the cascade is still flagged conflicting, so IgnoreConflicting.
	// A descendant's body may be unfetchable (pruned, frozen placeholder, or missing) —
	// log via rollbackErr and continue rather than abort the rest of the unwind.
	if step2Committed {
		for _, h := range allMarkedHashes {
			h := h

			if h.Equal(subtree.CoinbasePlaceholderHashValue) {
				continue
			}

			txMeta, e := s.Get(ctx, &h, fields.Tx)
			if e != nil {
				rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 2 (fetch tx %s) failed", h.String(), e))
				continue
			}

			if txMeta == nil || txMeta.Tx == nil {
				rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 2 (tx %s has no body)", h.String()))
				continue
			}

			if _, e := s.Spend(ctx, txMeta.Tx, blockHeight, IgnoreFlags{IgnoreConflicting: true, IgnoreLocked: true}); e != nil {
				rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 2 (re-spend tx %s) failed", h.String(), e))
			}
		}
	}

	// 4. Undo step 1: clear the conflicting flag on every hash MarkConflictingRecursively
	// added (cascaded children included).
	if len(allMarkedHashes) > 0 {
		if _, _, e := s.SetConflicting(ctx, allMarkedHashes, false); e != nil {
			rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 1 (clear conflicting flag) failed", e))
		}
	}

	// 5. Undo the lock applied at step 2 (only attempted if step 2 actually committed).
	if step2Committed && len(markedAsNotSpendable) > 0 {
		if e := s.SetLocked(ctx, markedAsNotSpendable, false); e != nil {
			rollbackErr = errors.Join(rollbackErr, errors.NewProcessingError("rollback step 2 lock (SetLocked false) failed", e))
		}
	}

	return rollbackErr
}

// setLockedWithRetry retries SetLocked with a bounded back-off — a deliberate exception
// to "always roll back". By the time we reach step 5 every other phase is committed and
// correct; the only inconsistency a SetLocked failure introduces is parents that are
// still locked. Rolling back here would re-introduce conflicting markers and unspend the
// winner — strictly worse than retrying a simple state update.
func setLockedWithRetry(ctx context.Context, s Store, hashes []chainhash.Hash, value bool) error {
	var err error

	for _, delay := range step5RetryDelays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return errors.NewProcessingError("setLockedWithRetry aborted by context", ctx.Err())
			case <-time.After(delay):
			}
		}

		if err = s.SetLocked(ctx, hashes, value); err == nil {
			return nil
		}
	}

	return err
}

// MarkConflictingRecursively marks the given transactions as conflicting, and iteratively marks all their spending
// children as conflicting too using breadth-first traversal.
//
// Parameters:
//   - ctx: The context for managing request-scoped values, cancellation signals, and deadlines.
//   - s: The UTXO store interface used to interact with the underlying data store.
//   - hashes: A slice of transaction hashes to be marked as conflicting.
//
// Returns:
//   - A slice of pointers to Spend structs representing the affected parent spends.
//   - A slice of all transaction hashes that were marked conflicting by this call,
//     including the input hashes and every descendant reached via BFS. Insertion
//     order is BFS order (input level first, then each descendant level) — callers
//     can rely on this for deterministic logs, traces, and eviction ordering.
//   - An error if any issues occur during the process.
//
// collectDanglingWinnerInputSpends runs BEFORE step 2 (Unspend) and is a pre-flight
// fail-closed gate. It inspects every winner-input slot and either queues an explicit
// Unspend for a genuinely dangling ghost slot, or returns a hard error when step 3's
// spend of that slot is deterministically doomed. It mutates nothing; only step 1 (fully
// reversible via SetConflicting(false)) has committed when it runs, so a return here is a
// clean abort.
//
// For each winner input it reads the recorded spender of the exact (parent, vout) slot
// via GetSpend — a single targeted page-record read on aerospike, avoiding the
// getAllExtraUTXOs fan-out that Get(fields.Utxos) triggers on high-fanout parents. Both
// backends normalise a missing parent to Status NOT_FOUND (no slot to repair — the
// missing-parent guard already ran inside GetCounterConflicting before step 1). The slot
// is then classified:
//
//   - empty, spent by the winner itself, or spender already in the marked-conflicting set
//     (step 1 queues its clear): nothing to do.
//   - frozen sentinel (subtree.FrozenBytesTxHash == subtree.CoinbasePlaceholderHashValue,
//     the all-0xFF hash): step 3 is doomed — the aerospike lua refuses frozen slots and
//     SQL keeps frozen state in a column Unspend never touches. Fail closed.
//   - live spender outside the marked set: step 3 is a real double-spend against a live
//     tx. Fail closed rather than silently steal its spend.
//   - ghost spender the pruner marked in the parent's deletedChildren set: deletion was
//     deliberate (e.g. a mined tx reaped after retention). Fail closed.
//   - unmarked ghost spender (record gone, no marker): a benign dangling ref — queue a
//     Spend carrying the stored spending data so step 2 clears the stale slot.
//
// Why the doomed cases fail closed BEFORE step 2 rather than skipping the slot and
// letting step 3 fail: step 3 IS doomed either way, but failing only at step 3 means
// step 2 has already run — and if the winner has ANOTHER input holding an unmarked ghost,
// step 2 clears that ghost's slot. Rollback cannot restore a cleared ghost slot (the
// ghost has no tx body to re-Spend), so a doomed-anyway resolution would strand it
// unspent+unlocked. Failing here, before any Unspend, closes that gap: danglingSpends is
// only ever queued when step 3 can succeed, so a mid-run failure can only strand a cleared
// ghost transiently and WAL replay converges (see rollbackProcessConflicting).
// winningTxHashes pairs with winningTxs by index.
func collectDanglingWinnerInputSpends(ctx context.Context, s Store, winningTxs []*bt.Tx, winningTxHashes []chainhash.Hash, markedSet map[chainhash.Hash]struct{}) ([]*Spend, error) {
	var danglingSpends []*Spend

	for txIdx, tx := range winningTxs {
		if tx == nil || txIdx >= len(winningTxHashes) {
			continue
		}

		txID := winningTxHashes[txIdx]

		for _, input := range tx.Inputs {
			prevTxIDHash := input.PreviousTxIDChainHash()
			if prevTxIDHash == nil {
				continue
			}

			parentHash := *prevTxIDHash

			// Targeted per-slot read (P2 read-storm fix): GetSpend returns the recorded
			// spender for exactly (parent, vout) and normalises a missing parent to Status
			// NOT_FOUND on both backends (aerospike ErrKeyNotFound, SQL sql.ErrNoRows). A
			// missing parent means there is no slot to repair — the fail-closed guard for
			// missing parents already ran inside GetCounterConflicting before step 1.
			resp, err := s.GetSpend(ctx, &Spend{TxID: &parentHash, Vout: input.PreviousTxOutIndex})
			if err != nil {
				if errors.IsNotFound(err) {
					continue
				}

				return nil, errors.NewProcessingError("[ProcessConflicting][%s] error getting parent %s:%d spend for dangling-slot check", txID.String(), parentHash.String(), input.PreviousTxOutIndex, err)
			}

			if resp == nil || resp.Status == int(Status_NOT_FOUND) {
				continue
			}

			spendingData := resp.SpendingData
			if spendingData == nil || spendingData.TxID == nil {
				continue
			}

			spender := *spendingData.TxID
			if spender.Equal(txID) {
				continue
			}

			// Frozen sentinel on a winner input: GetSpend reports the slot's spender as
			// subtree.FrozenBytesTxHash (== subtree.CoinbasePlaceholderHashValue — both are
			// the all-0xFF hash). Step 3 is deterministically doomed (aerospike lua refuses
			// frozen slots; SQL frozen state lives in a column Unspend never touches), so
			// fail closed here rather than clear the slot (which would unfreeze the output)
			// or let step 2 commit and strand another input's cleared ghost.
			if spender.Equal(subtree.FrozenBytesTxHash) {
				return nil, errors.NewProcessingError("[ProcessConflicting][%s] winner input %s:%d is frozen; refusing to resolve conflict", txID.String(), parentHash.String(), input.PreviousTxOutIndex)
			}

			if _, marked := markedSet[spender]; marked {
				continue
			}

			spenderMeta, err := s.Get(ctx, &spender, fields.Conflicting)
			if err != nil && !errors.IsNotFound(err) {
				return nil, errors.NewProcessingError("[ProcessConflicting][%s] error getting spender %s for dangling-slot check", txID.String(), spender.String(), err)
			}

			// Live spender outside the marked set: step 3 is a real double-spend against a
			// live tx. Fail closed BEFORE step 2 — silently clearing the slot would steal
			// the live tx's spend, and completing step 2 on a doomed resolution would
			// strand another input's cleared ghost (see the frozen case above).
			if err == nil && spenderMeta != nil {
				return nil, errors.NewProcessingError("[ProcessConflicting][%s] winner input %s:%d is spent by live non-conflicting tx %s; refusing to resolve conflict", txID.String(), parentHash.String(), input.PreviousTxOutIndex, spender.String())
			}

			// Ghost spender: its record is gone. GetSpend does not carry the parent's
			// deletedChildren marker, so fetch it here (rare repair path). On aerospike
			// this Get is page-aggregating: it unions the marker map from the master
			// record and every page record, so a marker on any vout's page is seen
			// (the pruner writes the marker page-keyed). A marked ghost was reaped
			// deliberately by the pruner (e.g. a mined tx after retention) — fail closed
			// rather than clear the slot. Safety bounds of tolerating an UNMARKED ghost
			// are documented at the walk's spender-existence check in
			// GetCounterConflictingTxHashes.
			markerMeta, err := s.Get(ctx, &parentHash, fields.DeletedChildren)
			if err != nil {
				return nil, errors.NewProcessingError("[ProcessConflicting][%s] error getting deletedChildren marker for parent %s for dangling-slot check", txID.String(), parentHash.String(), err)
			}

			if markerMeta != nil {
				if _, deleted := markerMeta.DeletedChildren[spender]; deleted {
					return nil, errors.NewProcessingError("[ProcessConflicting][%s] recorded spender %s was deleted by the pruner (deletedChildren); cannot resolve conflict", txID.String(), spender.String())
				}
			}

			prometheusUtxoCounterConflictingDanglingRefs.WithLabelValues("repair").Inc()

			utxoHash, err := util.UTXOHashFromInput(input)
			if err != nil {
				return nil, errors.NewProcessingError("[ProcessConflicting][%s] error hashing input %d for dangling-slot check", txID.String(), input.PreviousTxOutIndex, err)
			}

			parentHashCopy := parentHash

			danglingSpends = append(danglingSpends, &Spend{
				TxID:         &parentHashCopy,
				Vout:         input.PreviousTxOutIndex,
				UTXOHash:     utxoHash,
				SpendingData: spendingData,
			})
		}
	}

	return danglingSpends, nil
}

func MarkConflictingRecursively(ctx context.Context, s Store, hashes []chainhash.Hash) ([]*Spend, []chainhash.Hash, error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "MarkConflictingRecursively")

	defer deferFn()

	var allAffectedSpends []*Spend

	// The frozen sentinel (subtree.FrozenBytesTxHash == subtree.CoinbasePlaceholderHashValue,
	// the all-0xFF hash) can reach here two ways: the counter-conflicting walk keeps it in the
	// counter set on purpose, and a frozen parent slot can name it as a spending child in the
	// batches SetConflicting returns below. It is a marker, not a transaction: it has no store
	// record, marking it conflicting is meaningless, and feeding it to SetConflicting would
	// nil-deref on aerospike (the fetch loop skips the placeholder, leaving a nil tx the
	// processing loop derefs) and NOT_FOUND-error on SQL. Filter it out of BOTH the initial
	// batch and every descendant batch so it never reaches SetConflicting.
	toProcess := make([]chainhash.Hash, 0, len(hashes))
	for _, h := range hashes {
		if !h.Equal(subtree.FrozenBytesTxHash) {
			toProcess = append(toProcess, h)
		}
	}

	visited := make(map[chainhash.Hash]struct{}, len(hashes))
	markedOrder := make([]chainhash.Hash, 0, len(hashes))
	for _, h := range toProcess {
		if _, ok := visited[h]; !ok {
			visited[h] = struct{}{}
			markedOrder = append(markedOrder, h)
		}
	}

	for len(toProcess) > 0 {
		affectedParentSpends, spendingChildTxs, err := s.SetConflicting(ctx, toProcess, true)
		if err != nil {
			return nil, nil, err
		}

		allAffectedSpends = append(allAffectedSpends, affectedParentSpends...)

		// filter out already-visited hashes to prevent infinite loops, and the frozen
		// sentinel a frozen slot can surface as a spending child (see the note above).
		nextBatch := spendingChildTxs[:0]
		for _, child := range spendingChildTxs {
			if child.Equal(subtree.FrozenBytesTxHash) {
				continue
			}

			if _, ok := visited[child]; !ok {
				visited[child] = struct{}{}
				markedOrder = append(markedOrder, child)
				nextBatch = append(nextBatch, child)
			}
		}
		toProcess = nextBatch
	}

	return allAffectedSpends, markedOrder, nil
}

func GetAndLockChildren(ctx context.Context, s Store, hash chainhash.Hash) ([]chainhash.Hash, error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "GetAndLockChildren")

	defer deferFn()

	if hash.Equal(subtree.CoinbasePlaceholderHashValue) {
		// skip the coinbase placeholder hash
		return nil, errors.NewProcessingError("[GetAndLockChildren][%s] tx is frozen", hash.String())
	}

	visited := make(map[chainhash.Hash]struct{})
	visited[hash] = struct{}{}
	currentLevel := []chainhash.Hash{hash}

	for len(currentLevel) > 0 {
		results := make([]*meta.Data, len(currentLevel))
		g, gCtx := errgroup.WithContext(ctx)

		for i, current := range currentLevel {
			i := i
			current := current
			g.Go(func() error {
				if err := s.SetLocked(gCtx, []chainhash.Hash{current}, true); err != nil {
					return err
				}
				txMeta, err := s.Get(gCtx, &current, fields.Utxos)
				if err != nil {
					return err
				}
				results[i] = txMeta
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			return nil, err
		}

		var nextLevel []chainhash.Hash
		for _, txMeta := range results {
			if txMeta == nil {
				continue
			}

			if txMeta.SpendingDatas != nil {
				for _, spendingData := range txMeta.SpendingDatas {
					if spendingData != nil {
						child := *spendingData.TxID
						if _, ok := visited[child]; ok {
							continue
						}

						if child.Equal(subtree.CoinbasePlaceholderHashValue) {
							return nil, errors.NewProcessingError("[GetAndLockChildren][%s] tx is frozen", child.String())
						}

						visited[child] = struct{}{}
						nextLevel = append(nextLevel, child)
					}
				}
			}
		}
		currentLevel = nextLevel
	}

	// exclude the root hash from the result
	delete(visited, hash)

	children := make([]chainhash.Hash, 0, len(visited))
	for child := range visited {
		children = append(children, child)
	}

	return children, nil
}

func GetConflictingChildren(ctx context.Context, s Store, hash chainhash.Hash) ([]chainhash.Hash, error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "GetConflictingChildren")

	defer deferFn()

	if hash.Equal(subtree.CoinbasePlaceholderHashValue) {
		// skip the coinbase placeholder hash
		return nil, nil
	}

	visited := make(map[chainhash.Hash]struct{})
	visited[hash] = struct{}{}
	currentLevel := []chainhash.Hash{hash}

	for len(currentLevel) > 0 {
		results := make([]*meta.Data, len(currentLevel))
		g, gCtx := errgroup.WithContext(ctx)

		for i, current := range currentLevel {
			i := i
			current := current
			g.Go(func() error {
				txMeta, err := s.Get(gCtx, &current, fields.Utxos, fields.ConflictingChildren)
				if err != nil {
					// A parent may still record a spender whose own record has been removed
					// (pruned/reorged/deleted). Tolerate the dangling ref: leave results[i] nil
					// so the accumulation loop evicts and counts it (covering both the
					// isNotFound-error and the (nil, nil) missing-record variants uniformly),
					// then continue the BFS.
					if errors.IsNotFound(err) {
						return nil
					}

					return err
				}
				results[i] = txMeta
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			return nil, err
		}

		var nextLevel []chainhash.Hash
		for i, txMeta := range results {
			if txMeta == nil {
				// Tolerated dangling ref (or a backend that surfaces a missing record as
				// (nil, nil)): the hash has no record, so it must not appear in the result
				// set either — callers feed the result into SetConflicting/GetMeta, which
				// fail on missing records. Count both variants here (the goroutine no longer
				// increments), exactly once per evicted node.
				prometheusUtxoCounterConflictingDanglingRefs.WithLabelValues("bfs").Inc()

				delete(visited, currentLevel[i])

				continue
			}

			if txMeta.ConflictingChildren != nil {
				for _, child := range txMeta.ConflictingChildren {
					if _, ok := visited[child]; !ok {
						visited[child] = struct{}{}

						// The frozen sentinel (subtree.FrozenBytesTxHash, == the coinbase
						// placeholder — the all-0xFF hash) has no store record; descending
						// into it would only evict it via the ghost tolerance above. Keep
						// it IN the result — callers rely on its presence to reject frozen
						// children — but do not BFS past it.
						if child.Equal(subtree.FrozenBytesTxHash) {
							continue
						}

						nextLevel = append(nextLevel, child)
					}
				}
			}

			if txMeta.SpendingDatas != nil {
				for _, spendingData := range txMeta.SpendingDatas {
					if spendingData != nil {
						child := *spendingData.TxID
						if _, ok := visited[child]; !ok {
							visited[child] = struct{}{}

							// A frozen output records the frozen sentinel in its spend slot;
							// same handling as above: visible in the result, never descended.
							if child.Equal(subtree.FrozenBytesTxHash) {
								continue
							}

							nextLevel = append(nextLevel, child)
						}
					}
				}
			}
		}
		currentLevel = nextLevel
	}

	// exclude the root hash from the result
	delete(visited, hash)

	conflictingChildren := make([]chainhash.Hash, 0, len(visited))
	for child := range visited {
		conflictingChildren = append(conflictingChildren, child)
	}

	return conflictingChildren, nil
}

// parentSpendInfo carries, per parent tx, the recorded spender of each output slot plus
// the parent's deletedChildren set (aerospike bin / SQL deleted_children table; written
// reliably as of this PR — see meta.Data.DeletedChildren). The deletedChildren set lets
// GetCounterConflictingTxHashes discriminate a ghost spender the pruner deleted
// deliberately (marker present → fail closed) from one to tolerate (marker absent).
type parentSpendInfo struct {
	spendingTxIDs   []*chainhash.Hash
	deletedChildren map[chainhash.Hash]struct{}
}

// discriminateGhostSpender applies the deletedChildren discriminator to a recorded
// spender whose own record is gone — surfaced either as a NOT_FOUND error or as a
// (nil, nil) missing record. A ghost the pruner recorded in the parent's deletedChildren
// set was deleted deliberately (e.g. a mined tx reaped after retention): it returns a
// non-nil error so the walk fails closed. An unmarked ghost is a benign dangling ref: it
// bumps the tolerance counter once and returns nil so the caller tolerates it (excludes
// it from the counter set). Shared by the NOT_FOUND-error and (nil, nil) paths so both
// apply the identical rule.
func discriminateGhostSpender(txHash chainhash.Hash, spender *chainhash.Hash, deletedChildren map[chainhash.Hash]struct{}) error {
	if _, deleted := deletedChildren[*spender]; deleted {
		return errors.NewProcessingError("[GetCounterConflictingTxHashes][%s] recorded spender %s was deleted by the pruner (deletedChildren); cannot resolve conflict", txHash.String(), spender.String())
	}

	prometheusUtxoCounterConflictingDanglingRefs.WithLabelValues("walk").Inc()

	return nil
}

func GetCounterConflictingTxHashes(ctx context.Context, s Store, txHash chainhash.Hash) ([]chainhash.Hash, error) {
	ctx, _, deferFn := tracing.Tracer("utxo").Start(ctx, "GetCounterConflictingTxHashes")

	defer deferFn()

	txMeta, err := s.Get(ctx, &txHash, fields.Tx)
	if err != nil {
		// The queried tx itself may have been removed since it was flagged. There is no
		// counter-conflicting set to compute for a tx that no longer exists — tolerate it
		// and return an empty set rather than failing the caller with TX_NOT_FOUND.
		if errors.IsNotFound(err) {
			prometheusUtxoCounterConflictingDanglingRefs.WithLabelValues("walk").Inc()
			return make([]chainhash.Hash, 0), nil
		}

		return nil, err
	}

	counterConflictingMap := make(map[chainhash.Hash]struct{})
	counterConflictingMap[txHash] = struct{}{}

	// get the unique parent txs. Each parent carries its recorded spenders AND its
	// deletedChildren set, so a ghost spender can be discriminated: a marker means the
	// pruner deleted that spender deliberately (fail closed) vs. an unmarked ghost we
	// tolerate.
	parentTxs := make(map[chainhash.Hash]parentSpendInfo)

	for _, input := range txMeta.Tx.Inputs {
		// get the parent tx
		parentTxs[*input.PreviousTxIDChainHash()] = parentSpendInfo{}
	}

	for parentTx := range parentTxs {
		parentTxHash := &parentTx

		parentTxMeta, err := s.Get(ctx, parentTxHash, fields.Utxos, fields.DeletedChildren)
		if err != nil {
			// Do NOT tolerate a missing PARENT record the way a missing spender/child record
			// is tolerated. When only a spender's record is gone, the surviving parent's
			// SpendingDatas still tells us who currently spends the output. When the parent
			// record itself is gone, the entire spend graph for this input is lost: we can no
			// longer tell whether a counter — including one that was mined on our chain and
			// later pruned by retention — spends it. Silently dropping the input would turn
			// this guard fail-open, so propagate the error and fail closed instead.
			return nil, err
		}

		if parentTxMeta == nil {
			// Some backends surface a missing record as (nil, nil) — aerospike returns nil
			// data with no error for the coinbase-placeholder key (get.go). Same policy as
			// the error branch above: a missing parent record erases the spend graph for
			// this input, so fail closed rather than dereference nil SpendingDatas.
			return nil, errors.NewTxNotFoundError("[GetCounterConflictingTxHashes][%s] parent %s not found", txHash.String(), parentTxHash.String())
		}

		spendingTxIDs := make([]*chainhash.Hash, len(parentTxMeta.SpendingDatas))

		for idx, spendingData := range parentTxMeta.SpendingDatas {
			if spendingData == nil {
				continue
			}

			spendingTxIDs[idx] = spendingData.TxID
		}

		parentTxs[*parentTxHash] = parentSpendInfo{
			spendingTxIDs:   spendingTxIDs,
			deletedChildren: parentTxMeta.DeletedChildren,
		}
	}

	for _, input := range txMeta.Tx.Inputs {
		parentInfo, ok := parentTxs[*input.PreviousTxIDChainHash()]
		if ok {
			parenTxIDS := parentInfo.spendingTxIDs

			// check the length of the spending txs, if it's less than the index, then the input is not spent
			if len(parenTxIDS) <= int(input.PreviousTxOutIndex) {
				// throw an error
				return nil, errors.NewProcessingError("[GetCounterConflictingTxHashes][%s] cannot process counter conflicting, input %d of %s is out of range (len: %d, %v)", txHash.String(), input.PreviousTxOutIndex, input.PreviousTxIDChainHash().String(), len(parenTxIDS), parenTxIDS)
			}

			spendingTxID := parenTxIDS[input.PreviousTxOutIndex]
			if spendingTxID != nil {
				// Frozen sentinel: a frozen output records subtree.FrozenBytesTxHash
				// (== subtree.CoinbasePlaceholderHashValue — both are the all-0xFF hash)
				// in its spend slot. It has no store record, so the existence Get below
				// would misclassify it as a ghost; skip that Get and the child BFS, but
				// keep the sentinel IN the counter set. Consumers stay fail-safe on it:
				// checkCounterConflictingOnCurrentChain rejects a placeholder counter as
				// frozen (SubtreeValidation.go). If the sentinel reaches the losing set,
				// MarkConflictingRecursively filters it out BEFORE SetConflicting is ever
				// called (it is a marker, not a tx), so ProcessConflicting still resolves
				// the conflict; both SetConflicting backends also skip the placeholder
				// defensively (aerospike nil-skips it in the processing loop, SQL skips it
				// before its Get) so neither panics nor errors on it.
				if spendingTxID.Equal(subtree.FrozenBytesTxHash) {
					counterConflictingMap[*spendingTxID] = struct{}{}

					continue
				}

				// The recorded spender may itself have been removed while the parent still
				// records its spend (dangling ref). A ghost must be excluded from the counter
				// set entirely: callers feed this set into SetConflicting (ProcessConflicting
				// step 1) and GetMeta, both of which fail on a missing record. The default
				// GetConflictingChildren tolerates a missing root and returns an empty set, so
				// existence must be checked explicitly here.
				//
				// Consensus safety of tolerating an UNMARKED ghost: the mined-on-chain gate
				// (checkCounterConflictingOnCurrentChain) compares counters against blockIds
				// built from retention*2 (576 blocks, check_block_subtrees.go), NOT the
				// retention horizon (288) at which a mined-spent counter is pruned. The
				// (288, 576] band — pruned but still inside the comparison window — is
				// covered by the pruner's deletedChildren marker on BOTH backends (aerospike
				// bin + SQL deleted_children table): a marked ghost fails closed below.
				// The marker is written reliably as of this PR — aerospike writes it
				// unconditionally (the cuckoo consult that could skip it on a ~3% false
				// positive was removed) and the SQL pruner freezes the deletable set in a
				// temp table so no row is deleted unmarked under READ COMMITTED. The aerospike
				// marker is page-keyed (bounded per page record) and the parent Get above is
				// page-aggregating (get.go mergePageDeletedChildren unions every page's map),
				// so a marker for any vout — including vout ≥ utxoBatchSize — is seen here
				// regardless of which page holds it. Residual exposure is therefore historical
				// only: aerospike deletes done before this PR whose marker the cuckoo
				// pre-filter suppressed, and SQL stores upgraded mid-life whose
				// deleted_children table starts empty. Both additionally require an attacker
				// fork inside the 576-block window and are backstopped by the primary Spend
				// double-spend defense.
				spenderMeta, err := s.Get(ctx, spendingTxID, fields.Conflicting)
				if err != nil {
					// A ghost (NOT_FOUND) is discriminated by the deletedChildren marker:
					// marked → fail closed, unmarked → tolerated (counter bumped).
					if errors.IsNotFound(err) {
						if ghostErr := discriminateGhostSpender(txHash, spendingTxID, parentInfo.deletedChildren); ghostErr != nil {
							return nil, ghostErr
						}

						continue
					}

					return nil, err
				}

				if spenderMeta == nil {
					// Some backends surface a missing record as (nil, nil): same
					// marker discriminator as the NOT_FOUND-error path above.
					if ghostErr := discriminateGhostSpender(txHash, spendingTxID, parentInfo.deletedChildren); ghostErr != nil {
						return nil, ghostErr
					}

					continue
				}

				counterConflictingMap[*spendingTxID] = struct{}{}

				childHashes, err := s.GetConflictingChildren(ctx, *spendingTxID)
				if err != nil {
					// Fallback for Store implementations whose GetConflictingChildren still
					// propagates NOT_FOUND for a spender deleted between the existence check
					// above and this call.
					if errors.IsNotFound(err) {
						prometheusUtxoCounterConflictingDanglingRefs.WithLabelValues("walk").Inc()
						delete(counterConflictingMap, *spendingTxID)

						continue
					}

					return nil, err
				}

				for _, childHash := range childHashes {
					if childHash.Equal(subtree.FrozenBytesTxHash) {
						return nil, errors.NewProcessingError("[GetCounterConflictingTxHashes][%s] tx has frozen child", spendingTxID.String())
					}

					counterConflictingMap[childHash] = struct{}{}
				}
			}
		}
	}

	counterConflicting := make([]chainhash.Hash, 0, len(counterConflictingMap))

	for child := range counterConflictingMap {
		counterConflicting = append(counterConflicting, child)
	}

	// fmt.Printf("counterConflicting: %v\n", counterConflicting)

	return counterConflicting, nil
}
