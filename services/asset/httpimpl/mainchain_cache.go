package httpimpl

import (
	"context"
	"sync"

	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// mainChainCache caches BlockchainClient.CheckBlockIsInCurrentChain lookups
// in-process to avoid a gRPC round-trip on every asset HTTP request.
// Hot endpoints (/merkle_proof, /txmeta) are hit far more often than blocks
// are produced, so caching pays for itself within the first repeat hit.
//
// Design:
//   - Lazy fill: cache misses fall through to the blockchain client and write
//     the result.
//   - Conservative invalidation: the whole cache clears on each block-level
//     notification. Reorgs may flip on-chain status of cached IDs, so a full
//     rebuild is safer than tracking deltas — at the cost of a brief warm-up
//     after each block (handled by lazy fill).
//
// Thread-safety: a TOCTOU race exists between a fresh RPC result and a
// concurrent invalidation. Worst case: a stale entry survives until the next
// notification. Acceptable for this best-effort hint; both consumers
// (/merkle_proof, /txmeta) already document best-effort semantics around
// reorgs.
type mainChainCache struct {
	client blockchain.ClientI
	logger ulogger.Logger

	mu    sync.RWMutex
	cache map[uint32]bool
}

// newMainChainCache constructs an unstarted cache. Call Start(ctx) to begin
// consuming blockchain notifications.
func newMainChainCache(client blockchain.ClientI, logger ulogger.Logger) *mainChainCache {
	return &mainChainCache{
		client: client,
		logger: logger,
		cache:  make(map[uint32]bool),
	}
}

// Start subscribes to blockchain notifications and runs a goroutine that
// clears the cache on each block event until ctx is cancelled. Returns an
// error if the initial subscription fails.
func (c *mainChainCache) Start(ctx context.Context) error {
	sub, err := c.client.Subscribe(ctx, "asset-mainchain-cache")
	if err != nil {
		return err
	}
	go c.consume(ctx, sub)
	return nil
}

func (c *mainChainCache) consume(ctx context.Context, sub <-chan *blockchain_api.Notification) {
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-sub:
			if !ok {
				return
			}
			if n == nil {
				continue
			}
			switch n.Type {
			case model.NotificationType_Block,
				model.NotificationType_BlockMinedUnset:
				c.invalidate()
			}
		}
	}
}

// invalidate clears all cached entries. Called on block notifications.
func (c *mainChainCache) invalidate() {
	c.mu.Lock()
	c.cache = make(map[uint32]bool)
	c.mu.Unlock()
}

// IsOnMainChain reports whether the given block ID is part of the current
// best chain. Reads cached state on hit; falls back to the blockchain client
// on miss and stores the result.
func (c *mainChainCache) IsOnMainChain(ctx context.Context, blockID uint32) (bool, error) {
	c.mu.RLock()
	if v, ok := c.cache[blockID]; ok {
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	onChain, err := c.client.CheckBlockIsInCurrentChain(ctx, []uint32{blockID})
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	c.cache[blockID] = onChain
	c.mu.Unlock()
	return onChain, nil
}
