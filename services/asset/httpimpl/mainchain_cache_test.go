package httpimpl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestMainChainCache_MissThenHit(t *testing.T) {
	bcMock := &blockchain.Mock{}
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{42}).Return(true, nil).Once()

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	// First call: miss → RPC.
	onChain, err := c.IsOnMainChain(context.Background(), 42)
	require.NoError(t, err)
	assert.True(t, onChain)

	// Second call: hit → no RPC. Once() on the mock above means the assert below
	// fails if RPC is called a second time.
	onChain, err = c.IsOnMainChain(context.Background(), 42)
	require.NoError(t, err)
	assert.True(t, onChain)

	bcMock.AssertExpectations(t)
}

func TestMainChainCache_NegativeResultCached(t *testing.T) {
	bcMock := &blockchain.Mock{}
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{99}).Return(false, nil).Once()

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	// Cache misses propagate negative results too.
	for i := 0; i < 5; i++ {
		onChain, err := c.IsOnMainChain(context.Background(), 99)
		require.NoError(t, err)
		assert.False(t, onChain)
	}

	bcMock.AssertExpectations(t)
}

func TestMainChainCache_RPCErrorNotCached(t *testing.T) {
	bcMock := &blockchain.Mock{}
	// Two RPC calls expected: error responses are NOT cached.
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{42}).
		Return(false, assert.AnError).Twice()

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	_, err := c.IsOnMainChain(context.Background(), 42)
	require.Error(t, err)

	_, err = c.IsOnMainChain(context.Background(), 42)
	require.Error(t, err)

	bcMock.AssertExpectations(t)
}

func TestMainChainCache_InvalidationClears(t *testing.T) {
	bcMock := &blockchain.Mock{}
	// Expect TWO RPC calls: one before invalidation, one after.
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{42}).Return(true, nil).Twice()

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	_, err := c.IsOnMainChain(context.Background(), 42)
	require.NoError(t, err)

	c.invalidate()

	_, err = c.IsOnMainChain(context.Background(), 42)
	require.NoError(t, err)

	bcMock.AssertExpectations(t)
}

func TestMainChainCache_StartConsumesNotifications(t *testing.T) {
	notifyCh := make(chan *blockchain_api.Notification, 1)
	bcMock := &blockchain.Mock{}
	bcMock.On("Subscribe", mock.Anything, "asset-mainchain-cache").Return(notifyCh, nil)
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, []uint32{42}).Return(true, nil).Twice()

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))

	// Prime the cache.
	_, err := c.IsOnMainChain(ctx, 42)
	require.NoError(t, err)

	// Send a Block notification → cache must clear.
	notifyCh <- &blockchain_api.Notification{Type: model.NotificationType_Block}

	// Spin until cache is cleared (consume runs on a goroutine).
	require.Eventually(t, func() bool {
		c.mu.RLock()
		empty := len(c.cache) == 0
		c.mu.RUnlock()
		return empty
	}, time.Second, time.Millisecond)

	// Second lookup goes back to RPC.
	_, err = c.IsOnMainChain(ctx, 42)
	require.NoError(t, err)

	bcMock.AssertExpectations(t)
}

func TestMainChainCache_ConcurrentReadsSafe(t *testing.T) {
	bcMock := &blockchain.Mock{}
	bcMock.On("CheckBlockIsInCurrentChain", mock.Anything, mock.Anything).Return(true, nil)

	c := newMainChainCache(bcMock, ulogger.TestLogger{})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id uint32) {
			defer wg.Done()
			_, _ = c.IsOnMainChain(context.Background(), id%5)
		}(uint32(i))
	}
	wg.Wait()
}
