package sql

import (
	"context"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

// MainChainBlockHashesByHeights returns the hash of the main-chain block at each
// requested height in a single indexed query, but only when startHash is itself
// on the main chain. When startHash is a fork tip, the store is mid-rebuild, or
// any requested height is absent, it returns ok=false (nil error) so the caller
// falls back to the per-height recursive-CTE walk.
//
// Correctness: the main chain is linear, so the on_main_chain=true block at a
// given height (<= startHeight) is the unique ancestor of startHash at that
// height — identical to what the recursive walk would return. This mirrors the
// preflight + fallback used by GetLatestBlockHeaderFromBlockLocator.
func (s *SQL) MainChainBlockHashesByHeights(ctx context.Context, startHash *chainhash.Hash, startHeight uint32, heights []uint32) (map[uint32]*chainhash.Hash, bool, error) {
	ctx, _, deferFn := tracing.Tracer("blockchain").Start(ctx, "sql:MainChainBlockHashesByHeights")
	defer deferFn()

	if startHash == nil || len(heights) == 0 {
		return nil, false, nil
	}

	// Reorg guard: during a main-chain rebuild the on_main_chain flags are in
	// flux, so the fast path cannot be trusted. Fall back.
	if s.mainChainRebuilding.Load() > 0 {
		return nil, false, nil
	}

	// Preflight: only safe when startHash is on the main chain. Treat DB errors
	// or unknown hashes as "not on main chain" so the CTE walk stays
	// authoritative and surfaces any real error.
	var startOnMain bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT on_main_chain FROM blocks WHERE hash = $1 LIMIT 1), false)`,
		startHash[:],
	).Scan(&startOnMain); err != nil {
		return nil, false, nil
	}

	if !startOnMain {
		return nil, false, nil
	}

	var (
		q    string
		args []interface{}
	)

	if s.engine == util.Postgres {
		hs := make(pgtype.FlatArray[int64], len(heights))
		for i, h := range heights {
			hs[i] = int64(h)
		}

		q = `
			SELECT height, hash
			FROM blocks
			WHERE on_main_chain = true
			  AND height <= $1
			  AND height = ANY($2)`
		args = []interface{}{int64(startHeight), hs}
	} else {
		placeholders := make([]string, len(heights))
		args = make([]interface{}, len(heights)+1)
		args[0] = int64(startHeight)

		for i, h := range heights {
			placeholders[i] = fmt.Sprintf("$%d", i+2)
			args[i+1] = int64(h)
		}

		q = fmt.Sprintf(`
			SELECT height, hash
			FROM blocks
			WHERE on_main_chain = true
			  AND height <= $1
			  AND height IN (%s)`, strings.Join(placeholders, ","))
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, errors.NewStorageError("[MainChainBlockHashesByHeights] query failed", err)
	}
	defer rows.Close()

	result := make(map[uint32]*chainhash.Hash, len(heights))

	for rows.Next() {
		var (
			height    uint32
			hashBytes []byte
		)

		if err := rows.Scan(&height, &hashBytes); err != nil {
			return nil, false, errors.NewStorageError("[MainChainBlockHashesByHeights] scan failed", err)
		}

		hash, err := chainhash.NewHash(hashBytes)
		if err != nil {
			return nil, false, errors.NewProcessingError("[MainChainBlockHashesByHeights] failed to convert hash", err)
		}

		result[height] = hash
	}

	if err := rows.Err(); err != nil {
		return nil, false, errors.NewStorageError("[MainChainBlockHashesByHeights] rows error", err)
	}

	// Defensive: any missing height (duplicates are impossible — the locator
	// schedule is strictly decreasing) means we cannot guarantee an identical
	// locator, so fall back rather than emit a short/wrong one.
	if len(result) != len(heights) {
		return nil, false, nil
	}

	return result, true, nil
}
