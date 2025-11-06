package sql

import (
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/jellydator/ttlcache/v3"
)

// GenerationalCache wraps ttlcache with generation-based invalidation tracking.
// This prevents stale query results from being cached after invalidation occurs.
//
// Race condition without generational tracking:
// 1. Thread A: cache miss, starts DB query
// 2. Cache is invalidated (DeleteAll called, for example block added to chain)
// 3. Thread A: completes query with now-stale result
// 4. Thread A: writes stale result to cache ❌
// 5. Future reads return stale data instead of fresh data
//
// With generation tracking:
// - BeginQuery() captures the current generation in a CacheQuery object
// - DeleteAll() increments the generation
// - CacheQuery.Set() only writes if generation matches (query wasn't invalidated)
// - This ensures stale results from pre-invalidation queries aren't cached
type GenerationalCache struct {
	cache      *ttlcache.Cache[chainhash.Hash, any]
	generation atomic.Uint64
	stopped    atomic.Bool
}

// CacheQuery represents a scoped cache operation that captures generation at query start.
// This provides a cleaner API than token passing - the generation is encapsulated in the object.
type CacheQuery struct {
	cache      *GenerationalCache
	key        chainhash.Hash
	generation uint64 // captured at BeginQuery time
}

// NewGenerationalCache creates a new generational cache instance.
// The cache is automatically started and begins cleanup of expired items.
func NewGenerationalCache() *GenerationalCache {
	gc := &GenerationalCache{
		cache: ttlcache.New[chainhash.Hash, any](
			ttlcache.WithDisableTouchOnHit[chainhash.Hash, any](),
		),
	}
	// Auto-start the cache cleanup goroutine
	go gc.cache.Start()
	return gc
}

// BeginQuery starts a cache-safe query operation by capturing the current generation.
// Use this for Get→work→Set patterns to prevent stale writes after cache invalidation.
func (gc *GenerationalCache) BeginQuery(key chainhash.Hash) *CacheQuery {
	return &CacheQuery{
		cache:      gc,
		key:        key,
		generation: gc.generation.Load(),
	}
}

// Get retrieves the cached Item if present, or nil on miss.
// Returns *ttlcache.Item to maintain API compatibility - call .Value() on result.
func (cq *CacheQuery) Get() *ttlcache.Item[chainhash.Hash, any] {
	return cq.cache.cache.Get(cq.key)
}

// Set writes a value to the cache only if generation hasn't changed since BeginQuery.
// Returns true if cached, false if generation changed (cache was invalidated during query).
func (cq *CacheQuery) Set(value any, ttl time.Duration) bool {
	// Only cache if generation matches (cache wasn't invalidated during query)
	if cq.generation == cq.cache.generation.Load() {
		cq.cache.cache.Set(cq.key, value, ttl)
		return true
	}
	// Generation changed - skip caching stale result
	return false
}

// DeleteAll clears all cached entries and increments the generation.
// This invalidates any in-flight queries, preventing them from caching stale results.
func (gc *GenerationalCache) DeleteAll() {
	gc.cache.DeleteAll()
	gc.generation.Add(1)
}

// Stop halts automatic cleanup.
// It is safe to call Stop multiple times.
func (gc *GenerationalCache) Stop() {
	if gc.stopped.CompareAndSwap(false, true) {
		gc.cache.Stop()
	}
}
