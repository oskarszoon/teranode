package sql

import (
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func TestGenerationalCache_PreventStaleWrites(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	// Start a query (captures generation 0)
	query := gc.BeginQuery(key)

	// Simulate cache invalidation while query is in progress
	gc.DeleteAll()

	// Attempt to cache the now-stale result
	cached := query.Set("stale data", 1*time.Hour)

	// Should NOT cache because generation changed
	require.False(t, cached, "stale result should not be cached after invalidation")

	// Verify nothing was cached
	newQuery := gc.BeginQuery(key)
	item := newQuery.Get()
	require.Nil(t, item, "cache should be empty after rejecting stale write")
}

func TestGenerationalCache_AllowFreshWrites(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	// Start a query and immediately cache result (no invalidation)
	query := gc.BeginQuery(key)
	cached := query.Set("fresh data", 1*time.Hour)

	// Should cache successfully
	require.True(t, cached, "fresh result should be cached")

	// Verify data was cached
	newQuery := gc.BeginQuery(key)
	item := newQuery.Get()
	require.NotNil(t, item, "cache should contain the value")
	require.Equal(t, "fresh data", item.Value())
}

func TestGenerationalCache_MultipleInvalidations(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	// Start multiple queries
	query1 := gc.BeginQuery(key)
	gc.DeleteAll() // Invalidate after query1
	query2 := gc.BeginQuery(key)
	gc.DeleteAll() // Invalidate after query2
	query3 := gc.BeginQuery(key)

	// Only query3 should be able to cache
	require.False(t, query1.Set("data1", 1*time.Hour), "query1 should be stale")
	require.False(t, query2.Set("data2", 1*time.Hour), "query2 should be stale")
	require.True(t, query3.Set("data3", 1*time.Hour), "query3 should be fresh")

	// Verify only the latest data was cached
	newQuery := gc.BeginQuery(key)
	item := newQuery.Get()
	require.NotNil(t, item)
	require.Equal(t, "data3", item.Value())
}

func TestGenerationalCache_ConcurrentOperations(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}
	const numGoroutines = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Simulate concurrent queries and invalidations
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			query := gc.BeginQuery(key)
			time.Sleep(1 * time.Millisecond) // Simulate work

			// Randomly invalidate cache
			if id%10 == 0 {
				gc.DeleteAll()
			}

			// Try to cache result
			query.Set(id, 1*time.Hour)
		}(i)
	}

	wg.Wait()

	// Cache should have at most one value (the last successful write)
	// This test mainly ensures no panics occur with concurrent access
	newQuery := gc.BeginQuery(key)
	item := newQuery.Get()
	if item != nil {
		t.Logf("Final cached value: %v", item.Value())
	}
}

func TestGenerationalCache_StopMultipleTimes(t *testing.T) {
	gc := NewGenerationalCache()

	// Should not panic when called multiple times
	require.NotPanics(t, func() {
		gc.Stop()
		gc.Stop()
		gc.Stop()
	}, "Stop should be safe to call multiple times")
}

func TestGenerationalCache_GetBeforeSet(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	// Get on empty cache should return nil
	query := gc.BeginQuery(key)
	item := query.Get()
	require.Nil(t, item, "cache miss should return nil")
}

func TestGenerationalCache_TTLExpiration(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	// Cache with very short TTL
	query := gc.BeginQuery(key)
	cached := query.Set("expiring data", 50*time.Millisecond)
	require.True(t, cached)

	// Verify it's there immediately
	newQuery := gc.BeginQuery(key)
	item := newQuery.Get()
	require.NotNil(t, item)

	// Wait for expiration
	time.Sleep(100 * time.Millisecond)

	// Should be gone
	expiredQuery := gc.BeginQuery(key)
	item = expiredQuery.Get()
	require.Nil(t, item, "item should have expired")
}

func TestGenerationalCache_DifferentKeys(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key1 := chainhash.Hash{1}
	key2 := chainhash.Hash{2}

	// Cache two different keys
	query1 := gc.BeginQuery(key1)
	query1.Set("data1", 1*time.Hour)

	query2 := gc.BeginQuery(key2)
	query2.Set("data2", 1*time.Hour)

	// Invalidate cache
	gc.DeleteAll()

	// Both should be cleared
	newQuery1 := gc.BeginQuery(key1)
	require.Nil(t, newQuery1.Get(), "key1 should be cleared")

	newQuery2 := gc.BeginQuery(key2)
	require.Nil(t, newQuery2.Get(), "key2 should be cleared")
}

func TestGenerationalCache_SetReturnValue(t *testing.T) {
	gc := NewGenerationalCache()
	defer gc.Stop()

	key := chainhash.Hash{1, 2, 3}

	t.Run("returns true when cached", func(t *testing.T) {
		query := gc.BeginQuery(key)
		result := query.Set("test", 1*time.Hour)
		require.True(t, result, "Set should return true when value is cached")
	})

	t.Run("returns false when generation changed", func(t *testing.T) {
		query := gc.BeginQuery(key)
		gc.DeleteAll() // Invalidate
		result := query.Set("test", 1*time.Hour)
		require.False(t, result, "Set should return false when generation changed")
	})
}
