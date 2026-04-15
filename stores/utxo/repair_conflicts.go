package utxo

import (
	"context"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
)

// BlockHeaderInfo holds the minimal block header data needed by RepairConflictingChains.
type BlockHeaderInfo struct {
	Hash   *chainhash.Hash
	Height uint32
}

// BlockchainQuerier is the subset of blockchain.ClientI needed by RepairConflictingChains.
// Using a local interface with primitive types avoids an import cycle between stores/utxo and services/blockchain.
type BlockchainQuerier interface {
	GetBestBlockHeaderInfo(ctx context.Context) (BlockHeaderInfo, error)
	GetBlockHeaderIDs(ctx context.Context, blockHash *chainhash.Hash, numberOfHeaders uint64) ([]uint32, error)
}

// RepairReport contains the results of a conflict repair run.
type RepairReport struct {
	UnminedSinceFixed int     // Txs fixed in step 0 (had block_ids on main chain but unmined_since still set)
	CaseAFixed        int     // Losers fixed (missing Conflicting=true mark, cascaded to subtree)
	CaseCFixed        int     // Inverted winner/loser pairs fixed via ProcessConflicting
	Errors            []error // Non-fatal errors encountered during repair
}

// RepairConflictingChains detects and fixes inconsistent conflicting transaction state in the UTXO store.
// It must be called when the node is stopped (offline repair).
//
// Steps:
//
//  0. Fix unmined_since inconsistencies: txs with block_ids on main chain but unmined_since > 0 (prerequisite — mined txs must not appear in iterator)
//  1. Detect Case A (loser not marked) and Case C (inverted winner/loser confirmed on best chain)
//  2. Fix Case C first via ProcessConflicting (corrects SpendingData before Case A sweep)
//  3. Fix remaining Case A via MarkConflictingRecursively
//
// dryRun=true reports without writing any changes.
func RepairConflictingChains(ctx context.Context, s Store, blockchainClient BlockchainQuerier, dryRun bool) (RepairReport, error) {
	var report RepairReport

	// Step 0: fix unmined_since inconsistencies — prerequisite so mined txs don't appear in the iterator.
	bestHeader, err := blockchainClient.GetBestBlockHeaderInfo(ctx)
	if err != nil {
		return report, err
	}

	scanHeaders := uint64(bestHeader.Height + 1)

	bestBlockHeaderIDs, err := blockchainClient.GetBlockHeaderIDs(ctx, bestHeader.Hash, scanHeaders)
	if err != nil {
		return report, err
	}

	bestBlockIDsMap := make(map[uint32]bool, len(bestBlockHeaderIDs))
	for _, id := range bestBlockHeaderIDs {
		bestBlockIDsMap[id] = true
	}

	scanIt, err := s.ScanInconsistentUnminedTxs()
	if err != nil {
		return report, err
	}

	if scanIt != nil {
		defer scanIt.Close()

		for {
			batch, bErr := scanIt.Next(ctx)
			if bErr != nil {
				return report, bErr
			}
			if batch == nil {
				break
			}

			var toMark []chainhash.Hash
			for _, rec := range batch {
				if rec.UnminedSince == 0 {
					continue
				}
				for _, blockID := range rec.BlockIDs {
					if bestBlockIDsMap[blockID] {
						toMark = append(toMark, rec.Hash)
						break
					}
				}
			}

			if len(toMark) > 0 {
				if !dryRun {
					if mErr := s.MarkTransactionsOnLongestChain(ctx, toMark, true); mErr != nil {
						return report, mErr
					}
				}
				report.UnminedSinceFixed += len(toMark)
			}
		}
	}

	// Steps 1-3: conflict detection and repair.
	type caseCPair struct {
		loser  chainhash.Hash
		winner chainhash.Hash
	}

	var caseALosers []chainhash.Hash
	var caseCPairs []caseCPair
	processedMap := map[chainhash.Hash]bool{}

	unminedIt, err := s.GetUnminedTxIterator()
	if err != nil {
		return report, err
	}
	defer unminedIt.Close()

	for {
		batch, bErr := unminedIt.Next(ctx)
		if bErr != nil {
			return report, bErr
		}
		if batch == nil {
			break
		}

		for _, tx := range batch {
			if tx.Node == nil {
				continue
			}

			txHash := tx.Node.Hash
			if tx.Skip {
				continue
			}

			txMeta, gErr := s.Get(ctx, &txHash, fields.Conflicting, fields.Tx)
			if gErr != nil {
				report.Errors = append(report.Errors, gErr)
				continue
			}
			if txMeta == nil || txMeta.Conflicting || txMeta.Tx == nil {
				continue
			}

		inputLoop:
			for _, input := range txMeta.Tx.Inputs {
				parentHash := input.PreviousTxIDChainHash()
				vout := input.PreviousTxOutIndex

				parentMeta, pErr := s.Get(ctx, parentHash, fields.Utxos)
				if pErr != nil {
					report.Errors = append(report.Errors, pErr)
					continue
				}
				if parentMeta == nil {
					continue
				}
				if int(vout) >= len(parentMeta.SpendingDatas) {
					continue
				}

				spendingData := parentMeta.SpendingDatas[vout]
				if spendingData == nil || spendingData.TxID == nil {
					continue
				}

				if !spendingData.TxID.IsEqual(&txHash) {
					// Case A: this tx is a loser not yet marked conflicting.
					caseALosers = append(caseALosers, txHash)
					break inputLoop
				}

				// tx appears to be the winner per SpendingData — check for inversion.
				counterTxs, cErr := GetCounterConflictingTxHashes(ctx, s, txHash)
				if cErr != nil {
					report.Errors = append(report.Errors, cErr)
					break inputLoop
				}

			counterLoop:
				for _, c := range counterTxs {
					if c.IsEqual(&txHash) {
						continue
					}
					cCopy := c
					cMeta, cErr := s.Get(ctx, &cCopy, fields.BlockIDs)
					if cErr != nil {
						report.Errors = append(report.Errors, cErr)
						continue
					}
					if cMeta == nil {
						continue
					}
					for _, blockID := range cMeta.BlockIDs {
						if bestBlockIDsMap[blockID] {
							// Case C: c is confirmed on best chain — it's the real winner.
							caseCPairs = append(caseCPairs, caseCPair{loser: txHash, winner: c})
							break counterLoop
						}
					}
				}

				break inputLoop
			}
		}
	}

	// Fix Case C first so SpendingData is corrected before the Case A sweep.
	currentBlockHeight := bestHeader.Height
	for _, pair := range caseCPairs {
		if processedMap[pair.winner] {
			continue
		}
		if !dryRun {
			if _, pErr := ProcessConflicting(ctx, s, currentBlockHeight, []chainhash.Hash{pair.winner}, processedMap); pErr != nil {
				report.Errors = append(report.Errors, pErr)
				continue
			}
		}
		report.CaseCFixed++
		processedMap[pair.winner] = true
	}

	// Fix Case A, skipping any already resolved by Case C.
	for _, loser := range caseALosers {
		freshMeta, gErr := s.Get(ctx, &loser, fields.Conflicting)
		if gErr != nil {
			report.Errors = append(report.Errors, gErr)
			continue
		}
		if freshMeta != nil && freshMeta.Conflicting {
			continue
		}
		if !dryRun {
			if _, mErr := MarkConflictingRecursively(ctx, s, []chainhash.Hash{loser}); mErr != nil {
				report.Errors = append(report.Errors, mErr)
				continue
			}
		}
		report.CaseAFixed++
	}

	return report, nil
}
