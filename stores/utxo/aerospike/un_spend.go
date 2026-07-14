// Package aerospike provides an Aerospike-based implementation of the UTXO store interface.
// It offers high performance, distributed storage capabilities with support for large-scale
// UTXO sets and complex operations like freezing, reassignment, and batch processing.
//
// # Architecture
//
// The implementation uses a combination of Aerospike Key-Value store and Lua scripts
// for atomic operations. Transactions are stored with the following structure:
//   - Main Record: Contains transaction metadata and up to 20,000 UTXOs
//   - Pagination Records: Additional records for transactions with >20,000 outputs
//   - External Storage: Optional blob storage for large transactions
//
// # Features
//
//   - Efficient UTXO lifecycle management (create, spend, unspend)
//   - Support for batched operations with LUA scripting
//   - Automatic cleanup of spent UTXOs through DAH
//   - Alert system integration for freezing/unfreezing UTXOs
//   - Metrics tracking via Prometheus
//   - Support for large transactions through external blob storage
//
// # Usage
//
//	store, err := aerospike.New(ctx, logger, settings, &url.URL{
//	    Scheme: "aerospike",
//	    Host:   "localhost:3000",
//	    Path:   "/test/utxos",
//	    RawQuery: "expiration=3600&set=txmeta",
//	})
//
// # Database Structure
//
// Normal Transaction:
//   - inputs: Transaction input data
//   - outputs: Transaction output data
//   - utxos: List of UTXO hashes
//   - totalUtxos: Total number of UTXOs in the transaction
//   - recordUtxos: Total number of UTXO in this record
//   - spentUtxos: Number of spent UTXOs in this record
//   - blockIDs: Block references
//   - isCoinbase: Coinbase flag
//   - spendingHeight: Coinbase maturity height
//   - frozen: Frozen status
//
// Large Transaction with External Storage:
//   - Same as normal but with external=true
//   - Transaction data stored in blob storage
//   - Multiple records for >20k outputs
//
// # Thread Safety
//
// The implementation is fully thread-safe and supports concurrent access through:
//   - Atomic operations via Lua scripts
//   - Batched operations for better performance
//   - Lock-free reads with optimistic concurrency
package aerospike

import (
	"context"
	"time"

	"github.com/bsv-blockchain/aerospike-client-go/v8"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
)

// unspendBatchChunkSize bounds how many unspend BatchUDFs are sent to
// Aerospike in a single BatchOperate round-trip. A consolidation-tx reversal
// can carry tens of thousands of inputs; without chunking, a single
// oversized batch risks hitting Aerospike's per-batch record limit and
// blows out the request/response payload size. 1024 mirrors the chunk size
// used elsewhere in the store for similar bulk operations.
const unspendBatchChunkSize = 1024

// Unspend operations handle reverting spent UTXOs back to an unspent state.
// This is primarily used during blockchain reorganizations to handle
// transaction rollbacks.
//
// # Operation Flow
//
//	Validation → Lua Script → Update Records → Handle External Storage
//
// The operation:
//   1. Verifies UTXO exists
//   2. Clears spending transaction ID
//   3. Updates record counts for pagination
//   4. Manages external storage DAH
//   5. Updates metrics

// Unspend reverts spent UTXOs to unspent state.
// Parameters:
//   - ctx: Context for cancellation
//   - spends: Array of UTXOs to unspend
//
// Returns error if:
//   - Context is cancelled
//   - Timeout occurs
//   - UTXO doesn't exist
//   - Operation fails
//
// Thread Safety:
//   - Uses Lua scripts for atomic operations
//   - Handles concurrent unspend operations
//   - Coordinates with external storage
func (s *Store) Unspend(ctx context.Context, spends []*utxo.Spend, flagAsLocked ...bool) (err error) {
	return s.unspend(ctx, spends, flagAsLocked...)
}

// joinErr accumulates next into existing, working around a foot-gun in this
// package's errors.Join: when its first argument is a bare nil error
// (interface), Join does NOT preserve the second argument's concrete *Error
// type (and thus its error code) — it degrades the result to a generic
// errors.New(msg), so a later errors.Is(result, errors.ErrNotFound) would
// silently fail. IncrementSpentRecordsMulti and SetDAHForChildRecordsMulti
// (spend.go) guard against this the same way: assign directly on the first
// error, only calling errors.Join once there's already an accumulated error.
func joinErr(existing, next error) error {
	if next == nil {
		return existing
	}

	if existing == nil {
		return next
	}

	return errors.Join(existing, next)
}

// chunkSpends splits spends into consecutive slices of at most size elements.
// Pure helper (no I/O) so it can be unit-tested without an Aerospike
// container: it drives the sizing behind the batched unspend below.
func chunkSpends(spends []*utxo.Spend, size int) [][]*utxo.Spend {
	if size <= 0 {
		size = len(spends)
	}

	var chunks [][]*utxo.Spend

	for i := 0; i < len(spends); i += size {
		end := i + size
		if end > len(spends) {
			end = len(spends)
		}

		chunks = append(chunks, spends[i:end])
	}

	return chunks
}

// unspend implements the core unspend logic.
//
// Unlike the previous implementation (one synchronous client.Execute Lua
// call per UTXO), this batches UTXOs into chunks of up to
// unspendBatchChunkSize and reverses each chunk with a single
// BatchOperate of "unspend" BatchUDFs — the same Lua UDF, same mandatory
// ownership check (a mismatched SpendingData is a no-op inside the UDF),
// and the same per-record response handling as before (see
// postProcessUnspendRecord). This turns a consolidation-tx reversal of
// tens of thousands of inputs from N round-trips into ceil(N/1024).
//
// Semantics change (intentional): the previous serial loop returned on the
// first per-UTXO error. This batched path attempts every record in every
// chunk and aggregates all per-record errors via errors.Join, so a single
// bad record does not abort the reversal of the rest of the batch — this is
// necessary for the batch to finish reversing whatever it still can.
func (s *Store) unspend(ctx context.Context, spends []*utxo.Spend, flagAsLocked ...bool) (err error) {
	start := time.Now()
	count := 0

	batchPolicy := util.GetAerospikeBatchPolicy(s.settings)
	batchUDFPolicy := aerospike.NewBatchUDFPolicy()

	var aggErr error

	for _, chunk := range chunkSpends(spends, unspendBatchChunkSize) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return errors.NewStorageError("timeout un-spending after %d of %d utxos", count, len(spends), ctxErr)
			}

			return errors.NewStorageError("context cancelled un-spending after %d of %d utxos", count, len(spends), ctxErr)
		}

		batchRecords := make([]aerospike.BatchRecordIfc, 0, len(chunk))
		chunkSpendsByIdx := make([]*utxo.Spend, 0, len(chunk))

		for _, spend := range chunk {
			if spend == nil {
				continue
			}

			if spend.SpendingData == nil {
				return errors.NewProcessingError("[Unspend] SpendingData is required for %s:%d", spend.TxID, spend.Vout)
			}

			s.logger.Debugf("un-spending utxo %s of tx %s:%d, spending data: %v", spend.UTXOHash.String(), spend.TxID.String(), spend.Vout, spend.SpendingData)

			keySource := uaerospike.CalculateKeySource(spend.TxID, spend.Vout, s.utxoBatchSize)

			key, aErr := aerospike.NewKey(s.namespace, s.setName, keySource)
			if aErr != nil {
				if e, ok := aErr.(*aerospike.AerospikeError); ok {
					prometheusUtxoMapErrors.WithLabelValues("Reset", e.ResultCode.String()).Inc()
				} else {
					prometheusUtxoMapErrors.WithLabelValues("Reset", "unknown").Inc()
				}

				return errors.NewProcessingError("error in aerospike NewKey", aErr)
			}

			offset := s.calculateOffsetForOutput(spend.Vout)

			batchRecords = append(batchRecords, aerospike.NewBatchUDF(batchUDFPolicy, key, LuaPackage, "unspend",
				aerospike.NewIntegerValue(int(offset)),         // vout adjusted for utxoBatchSize
				aerospike.NewValue(spend.UTXOHash[:]),          // utxo hash
				aerospike.NewValue(spend.SpendingData.Bytes()), // expected stored spending data (mandatory ownership check)
				aerospike.NewIntegerValue(int(s.blockHeight.Load())),
				aerospike.NewValue(s.settings.GetUtxoStoreBlockHeightRetention()),
			))
			chunkSpendsByIdx = append(chunkSpendsByIdx, spend)
		}

		if len(batchRecords) == 0 {
			continue
		}

		if opErr := s.batchOperate(batchPolicy, batchRecords); opErr != nil {
			prometheusUtxoMapErrors.WithLabelValues("Reset", "batch error").Inc()
			aggErr = joinErr(aggErr, errors.NewStorageError("error in aerospike unspend batch", opErr))
			// Do NOT skip the per-record loop below on a partial batch failure:
			// records that succeeded still have populated results and each needs
			// postProcessUnspendRecord (the NOTALLSPENT spentExtraRecs decrement).
			// Skipping the whole chunk would leave a paginated parent's master
			// spent-count drifted high. The loop already aggregates per-record
			// errors and skips records whose BatchRec().Err is set.
		}

		for i := range batchRecords {
			count++

			batchRec := batchRecords[i].BatchRec()
			if batchRec.Err != nil {
				if e, ok := batchRec.Err.(*aerospike.AerospikeError); ok {
					prometheusUtxoMapErrors.WithLabelValues("Reset", e.ResultCode.String()).Inc()
				} else {
					prometheusUtxoMapErrors.WithLabelValues("Reset", "unknown").Inc()
				}

				aggErr = joinErr(aggErr, errors.NewStorageError("error in aerospike unspend record", batchRec.Err))

				continue
			}

			if recErr := s.postProcessUnspendRecord(ctx, chunkSpendsByIdx[i], batchRec.Record); recErr != nil {
				aggErr = joinErr(aggErr, recErr)
			}
		}
	}

	s.logger.Debugf("[Unspend] reversed %d/%d utxos in %s", count, len(spends), time.Since(start))

	return aggErr
}

// postProcessUnspendRecord mirrors the response handling previously done
// inline in unspendLua for a single synchronous client.Execute call, applied
// per-record to a BatchUDF response instead:
//  1. Parses the Lua map response (record.Bins[LuaSuccess]).
//  2. On OK + NOTALLSPENT signal, decrements spentExtraRecs on the master
//     record via handleExtraRecords — this is required to keep pagination
//     spent-counts correct; dropping it would corrupt master-record
//     spent-counts for paginated transactions.
//  3. On ERROR + TX_NOT_FOUND, surfaces a NotFoundError; any other error
//     status surfaces as a StorageError.
//
// Lua Return Values:
//   - Map response with status="OK" and optional signal
//   - Map response with status="ERROR" and message field
//
// Metrics:
//   - prometheusUtxoMapReset: Successful unspends
//   - prometheusUtxoMapErrors: Failed operations
func (s *Store) postProcessUnspendRecord(ctx context.Context, spend *utxo.Spend, record *aerospike.Record) error {
	if record == nil || record.Bins == nil || record.Bins[LuaSuccess.String()] == nil {
		prometheusUtxoMapErrors.WithLabelValues("Reset", "no response").Inc()
		return errors.NewProcessingError("[Unspend] no response from Lua for %s:%d", spend.TxID, spend.Vout)
	}

	res, err := s.ParseLuaMapResponse(record.Bins[LuaSuccess.String()])
	if err != nil {
		prometheusUtxoMapErrors.WithLabelValues("Reset", "error parsing response").Inc()
		return errors.NewProcessingError("error parsing response", err)
	}

	if res.Status == LuaStatusOK {
		// Handle signal if present
		if res.Signal == LuaSignalNotAllSpent {
			// Decrement spentExtraRecs on the master record since this child
			// record transitioned from ALLSPENT back to NOTALLSPENT.
			// This mirrors the +1 increment done in handleSpendSignal for ALLSPENT.
			// Pass 0 so the DAH height falls back to the current block height; a
			// decrement clears the DAH (tx no longer all-spent) rather than setting
			// it, so the height is not actually used here.
			if err := s.handleExtraRecords(ctx, spend.TxID, -1, 0); err != nil {
				return err
			}
		}
	} else if res.Status == LuaStatusError {
		prometheusUtxoMapErrors.WithLabelValues("Reset", "error response").Inc()

		if res.ErrorCode == LuaErrorCodeTxNotFound {
			return errors.NewNotFoundError("output %s:%d not found", spend.TxID, spend.Vout)
		}

		return errors.NewStorageError("error in aerospike unspend record: %s", res.Message)
	}

	prometheusUtxoMapReset.Inc()

	return nil
}

// txa & txb both spending txp

// processConflicting(txb)
//    mark txa conflicting
//    change spend of txp
//    mark txb not conflicting
