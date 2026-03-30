package blockvalidation

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/catchup"
	"github.com/bsv-blockchain/teranode/util"
)

const (
	// maxHeadersPerBackwardRequest is the max headers the /headers endpoint returns per request.
	// This is hardcoded in the Asset server's GetBlockHeaders handler.
	maxHeadersPerBackwardRequest = 1000
)

// fetchHeadersBackwardFromCheckpoint fetches headers by walking backwards from a known
// checkpoint hash using the /headers/{hash}?n=N endpoint. This is faster and more
// predictable than headers_from_common_ancestor because it doesn't need to compute
// a common ancestor — it just walks the parent chain.
//
// The headers are returned in forward order (genesis → checkpoint) ready for validation.
//
// Parameters:
//   - ctx: Context for cancellation
//   - baseURL: Peer's DataHub base URL
//   - checkpointHash: Known checkpoint block hash to walk backwards from
//   - checkpointHeight: Height of the checkpoint
//   - localHeight: Our current UTXO height (stop walking when we reach this)
//
// Returns:
//   - []*model.BlockHeader: Headers in ascending height order
//   - error: If any request fails
func (u *Server) fetchHeadersBackwardFromCheckpoint(
	ctx context.Context,
	baseURL string,
	checkpointHash *chainhash.Hash,
	checkpointHeight uint32,
	localHeight uint32,
) ([]*model.BlockHeader, error) {
	needed := int(checkpointHeight - localHeight)
	if needed <= 0 {
		return nil, nil
	}

	startTime := time.Now()
	u.logger.Infof("[catchup:checkpointHeaders] Fetching %d headers backward from checkpoint %s (height %d) to height %d using /headers endpoint",
		needed, checkpointHash.String(), checkpointHeight, localHeight)

	// Collect headers in reverse order (newest first, walking backwards)
	allHeaders := make([]*model.BlockHeader, 0, needed)
	currentHash := checkpointHash
	iteration := 0

	for len(allHeaders) < needed {
		iteration++
		remaining := needed - len(allHeaders)
		requestCount := remaining
		if requestCount > maxHeadersPerBackwardRequest {
			requestCount = maxHeadersPerBackwardRequest
		}

		// Build URL for backward header walk
		requestURL := fmt.Sprintf("%s/headers/%s?n=%d", baseURL, currentHash.String(), requestCount)

		headerBytes, err := util.DoHTTPRequest(ctx, requestURL)
		if err != nil {
			return nil, errors.NewProcessingError("[catchup:checkpointHeaders] iteration %d: failed to fetch headers from %s", iteration, baseURL, err)
		}

		if err = catchup.ValidateBlockHeaderBytes(headerBytes); err != nil {
			return nil, errors.NewProcessingError("[catchup:checkpointHeaders] iteration %d: invalid header bytes", iteration, err)
		}

		headers, err := catchup.ParseBlockHeaders(headerBytes)
		if err != nil {
			return nil, errors.NewProcessingError("[catchup:checkpointHeaders] iteration %d: failed to parse headers", iteration, err)
		}

		if len(headers) == 0 {
			u.logger.Warnf("[catchup:checkpointHeaders] iteration %d: received 0 headers, stopping", iteration)
			break
		}

		u.logger.Debugf("[catchup:checkpointHeaders] iteration %d: received %d headers (total so far: %d/%d)",
			iteration, len(headers), len(allHeaders)+len(headers), needed)

		// Headers come in descending order (newest first). Append them.
		allHeaders = append(allHeaders, headers...)

		// The last header in the response is the oldest — use its HashPrevBlock for the next request.
		oldest := headers[len(headers)-1]
		if oldest.HashPrevBlock == nil {
			// Reached genesis
			break
		}
		currentHash = oldest.HashPrevBlock

		// Safety: if we got fewer headers than requested, we've reached the end of the chain
		if len(headers) < requestCount {
			break
		}
	}

	if len(allHeaders) == 0 {
		return nil, errors.NewProcessingError("[catchup:checkpointHeaders] no headers received")
	}

	// Reverse to get forward order (ascending height)
	for i, j := 0, len(allHeaders)-1; i < j; i, j = i+1, j-1 {
		allHeaders[i], allHeaders[j] = allHeaders[j], allHeaders[i]
	}

	elapsed := time.Since(startTime)
	u.logger.Infof("[catchup:checkpointHeaders] Fetched %d headers in %d iterations (%.1fs, %.0f headers/s)",
		len(allHeaders), iteration, elapsed.Seconds(), float64(len(allHeaders))/elapsed.Seconds())

	return allHeaders, nil
}

// findNextCheckpointForHeight returns the next checkpoint above the given height,
// or nil if there are no more checkpoints.
func findNextCheckpointForHeight(checkpoints []chaincfg.Checkpoint, height uint32) *chaincfg.Checkpoint {
	for i := range checkpoints {
		if uint32(checkpoints[i].Height) > height {
			return &checkpoints[i]
		}
	}
	return nil
}
