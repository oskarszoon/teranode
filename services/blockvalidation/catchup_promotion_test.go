package blockvalidation

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/blockvalidation/testhelpers"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Only failed writes are injected; successful writes and the FSM use SQLite.
type promotionFaultStore struct {
	blockchainstore.Store
	fail func() error
}

func (s *promotionFaultStore) SetFSMState(ctx context.Context, state string) error {
	if s.fail != nil {
		if err := s.fail(); err != nil {
			return err
		}
	}
	return s.Store.SetFSMState(ctx, state)
}

// LocalClient has no runtime FSM. Forward these methods to the real authority.
type promotionAuthorityClient struct {
	blockchain.ClientI
	authority     *blockchain.Blockchain
	runCalls      int
	beforeRun     func(context.Context) error
	catchupCalls  atomic.Int32
	beforeCatchup func(context.Context) error
	cachedReads   int
	cachedState   *blockchain.FSMStateType
}

func (c *promotionAuthorityClient) Run(ctx context.Context, _ string) error {
	c.runCalls++
	if c.beforeRun != nil {
		if err := c.beforeRun(ctx); err != nil {
			return err
		}
	}
	_, err := c.authority.Run(ctx, &emptypb.Empty{})
	if err != nil {
		return errors.UnwrapGRPC(err)
	}
	return nil
}

func (c *promotionAuthorityClient) CatchUpBlocks(ctx context.Context) error {
	c.catchupCalls.Add(1)
	if c.beforeCatchup != nil {
		if err := c.beforeCatchup(ctx); err != nil {
			return err
		}
	}
	_, err := c.authority.CatchUpBlocks(ctx, &emptypb.Empty{})
	if err != nil {
		return errors.UnwrapGRPC(err)
	}
	return nil
}

func TestSetFSMCatchingBlocks_RetriesDuringOwningCatchup(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	_, err := client.authority.Run(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	var writes atomic.Int32
	store.fail = func() error {
		if writes.Add(1) == 1 {
			return errors.NewStorageError("temporary CATCHUPBLOCKS write failure")
		}
		return nil
	}
	var size atomic.Int64
	size.Store(1)
	require.NoError(t, server.setFSMCatchingBlocks(context.Background(), catchupCtx, &size))
	require.Equal(t, int32(2), client.catchupCalls.Load())
	requirePromotionState(t, client, store, blockchain.FSMStateCATCHINGBLOCKS)
	server.restoreFSMState(context.Background(), catchupCtx)
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
}

func TestSetFSMCatchingBlocks_ExhaustionLeavesExactIntentWithoutReplay(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	_, err := client.authority.Run(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	var writes atomic.Int32
	store.fail = func() error {
		writes.Add(1)
		return errors.NewStorageError("persistent CATCHUPBLOCKS write failure")
	}
	var size atomic.Int64
	size.Store(1)
	err = server.setFSMCatchingBlocks(context.Background(), catchupCtx, &size)
	require.Error(t, err)
	require.Equal(t, int32(catchupAdmissionAttempts), client.catchupCalls.Load())
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
	require.Never(t, func() bool { return writes.Load() > int32(catchupAdmissionAttempts) }, 750*time.Millisecond, 10*time.Millisecond,
		"no authority worker may write CATCHUPBLOCKS after catchup returns")
}

func TestSetFSMCatchingBlocks_CancellationStopsFurtherAttempts(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	_, err := client.authority.Run(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.fail = func() error {
		cancel()
		return errors.NewStorageError("CATCHUPBLOCKS write failed during cancellation")
	}
	var size atomic.Int64
	size.Store(1)
	err = server.setFSMCatchingBlocks(ctx, catchupCtx, &size)
	require.Error(t, err)
	require.Equal(t, int32(1), client.catchupCalls.Load())
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
}

func TestSetFSMCatchingBlocks_AttemptTimeoutUsesConfiguredOrDefaultStoreBound(t *testing.T) {
	for _, tt := range []struct {
		name           string
		storeTimeoutMS int
		want           time.Duration
	}{
		{"zero_uses_default", 0, 7 * time.Second},
		{"negative_uses_default", -1, 7 * time.Second},
		{"configured", 10, 2010 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, client, _, catchupCtx := newPromotionAuthority(t)
			server.settings.BlockChain.StoreDBTimeoutMillis = tt.storeTimeoutMS
			client.beforeCatchup = func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.InDelta(t, tt.want.Seconds(), time.Until(deadline).Seconds(), 0.25)
				return errors.NewStateError("stop before FSM admission")
			}
			var size atomic.Int64
			size.Store(1)
			err := server.setFSMCatchingBlocks(context.Background(), catchupCtx, &size)
			require.Error(t, err)
			require.Equal(t, int32(1), client.catchupCalls.Load(), "state refusal must not retry")
		})
	}
}

func (c *promotionAuthorityClient) GetFSMCurrentState(ctx context.Context) (*blockchain.FSMStateType, error) {
	c.cachedReads++
	if c.cachedState != nil {
		return c.cachedState, nil
	}
	state, err := c.authority.GetFSMCurrentState(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, errors.UnwrapGRPC(err)
	}
	return &state.State, nil
}

func newPromotionAuthority(t *testing.T) (*Server, *promotionAuthorityClient, *promotionFaultStore, *CatchupContext) {
	t.Helper()
	settings := test.CreateBaseTestSettings(t)
	store, err := blockchainstore.NewStore(ulogger.TestLogger{}, &url.URL{Scheme: "sqlitememory"}, settings)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	faultStore := &promotionFaultStore{Store: store}
	authority, err := blockchain.New(context.Background(), ulogger.TestLogger{}, settings, faultStore, nil, blockchain.FSMStateCATCHINGBLOCKS.String())
	require.NoError(t, err)
	require.NoError(t, authority.Init(context.Background()))
	authority.SetSubscriptionManagerReadyForTesting(true)
	client, err := blockchain.NewLocalClient(ulogger.TestLogger{}, settings, store, nil, nil)
	require.NoError(t, err)
	adapter := &promotionAuthorityClient{ClientI: client, authority: authority}
	server := &Server{settings: settings, logger: ulogger.TestLogger{}, blockchainClient: adapter}
	return server, adapter, faultStore, &CatchupContext{blockUpTo: testhelpers.CreateTestBlockChain(t, 2)[1]}
}

func requirePromotionState(t *testing.T, client *promotionAuthorityClient, store *promotionFaultStore, want blockchain.FSMStateType) {
	t.Helper()
	state, err := client.GetFSMCurrentState(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, *state)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, want.String(), persisted)
}

func TestRestoreFSMState_RetriesPersistenceFailure(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	writes := 0
	store.fail = func() error {
		writes++
		if writes == 1 {
			return errors.NewStorageError("temporary database failure")
		}
		return nil
	}
	server.restoreFSMState(context.Background(), catchupCtx)
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
	require.Equal(t, 2, client.runCalls)
}

func TestRestoreFSMState_RecoversAfterLegacyThreeAttemptWindow(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var writes atomic.Int32
	fourthAttempt := make(chan struct{})
	store.fail = func() error {
		attempt := writes.Add(1)
		if attempt == 4 {
			close(fourthAttempt)
		}
		if attempt <= 5 {
			return errors.NewStorageError("temporary database outage")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		server.restoreFSMState(ctx, catchupCtx)
		close(done)
	}()
	select {
	case <-fourthAttempt:
		// The catchup owner remains inside restore after the former retry cap.
	case <-done:
		t.Fatal("catchup owner returned before the database recovered")
	case <-ctx.Done():
		t.Fatal("RUN promotion did not reach a fourth attempt")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("RUN promotion did not recover before the owning context ended")
	}
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
	require.Equal(t, 6, client.runCalls, "the owner must retry exact RUN until it is acknowledged")
}

func TestRestoreFSMState_AttemptTimeoutUsesConfiguredOrDefaultStoreBound(t *testing.T) {
	for _, tt := range []struct {
		name           string
		storeTimeoutMS int
		want           time.Duration
	}{
		{"zero_uses_default", 0, 7 * time.Second},
		{"negative_uses_default", -1, 7 * time.Second},
		{"configured", 10, 2010 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, client, _, catchupCtx := newPromotionAuthority(t)
			server.settings.BlockChain.StoreDBTimeoutMillis = tt.storeTimeoutMS
			client.beforeRun = func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "every RUN RPC must be bounded independently")
				require.InDelta(t, tt.want.Seconds(), time.Until(deadline).Seconds(), 0.25)
				return errors.NewStateError("permanent operator refusal")
			}
			server.restoreFSMState(context.Background(), catchupCtx)
			require.Equal(t, 1, client.runCalls, "permanent state refusal must stop immediately")
		})
	}
}

func TestRestoreFSMState_CancellationStopsRetry(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.fail = func() error {
		cancel()
		return errors.NewStorageError("database unavailable")
	}
	server.restoreFSMState(ctx, catchupCtx)
	requirePromotionState(t, client, store, blockchain.FSMStateCATCHINGBLOCKS)
	require.Equal(t, 1, client.runCalls)
	require.Never(t, func() bool { return client.runCalls > 1 }, 250*time.Millisecond, 10*time.Millisecond,
		"no RUN retry may outlive its owning context")
}

func TestRestoreFSMState_CancellationInterruptsBackoff(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.fail = func() error {
		timer := time.AfterFunc(50*time.Millisecond, cancel)
		t.Cleanup(func() { timer.Stop() })
		return errors.NewStorageError("database unavailable")
	}
	start := time.Now()
	server.restoreFSMState(ctx, catchupCtx)
	require.Less(t, time.Since(start), 800*time.Millisecond, "cancellation must interrupt the one-second backoff")
	requirePromotionState(t, client, store, blockchain.FSMStateCATCHINGBLOCKS)
	require.Equal(t, 1, client.runCalls)
}

func TestRestoreFSMState_TransientTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unavailable", status.Error(codes.Unavailable, "temporary transport failure")},
		{"deadline", status.Error(codes.DeadlineExceeded, "temporary transport failure")},
		{"checkpoint read deadline", errors.UnwrapGRPC(errors.WrapGRPC(errors.NewStateError("tip read failed", context.DeadlineExceeded)))},
		{"checkpoint read storage failure", errors.UnwrapGRPC(errors.WrapGRPC(errors.NewStateError("tip read failed", errors.NewStorageError("temporary read failure"))))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client, store, catchupCtx := newPromotionAuthority(t)
			client.beforeRun = func(context.Context) error {
				if client.runCalls == 1 {
					return tc.err
				}
				return nil
			}
			server.restoreFSMState(context.Background(), catchupCtx)
			requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
			require.Equal(t, 2, client.runCalls)
		})
	}
}

func TestRestoreFSMState_DoesNotRetryStateRejection(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	client.beforeRun = func(context.Context) error { return errors.NewStateError("tip below highest checkpoint") }
	server.restoreFSMState(context.Background(), catchupCtx)
	requirePromotionState(t, client, store, blockchain.FSMStateCATCHINGBLOCKS)
	require.Equal(t, 1, client.runCalls)
}

func TestRetryableFSMPromotionError_PermanentVerdictWinsConcurrentChildDeadline(t *testing.T) {
	require.False(t, retryableFSMPromotionError(errors.NewStateError("operator IDLE"), context.DeadlineExceeded),
		"an expired child deadline cannot turn an authoritative state refusal into an endless retry")
	require.True(t, retryableFSMPromotionError(errors.NewStateError("tip read failed", context.DeadlineExceeded), context.DeadlineExceeded),
		"a checkpoint read timeout remains transient")
}

func TestRestoreFSMState_IgnoresCachedIdle(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	idle := blockchain.FSMStateIDLE
	client.cachedState = &idle
	server.restoreFSMState(context.Background(), catchupCtx)
	require.Zero(t, client.cachedReads, "cached/synthetic IDLE must not suppress authoritative promotion")
	client.cachedState = nil
	requirePromotionState(t, client, store, blockchain.FSMStateRUNNING)
}

func TestRestoreFSMState_RetryPreservesOperatorIdle(t *testing.T) {
	server, client, store, catchupCtx := newPromotionAuthority(t)
	client.beforeRun = func(context.Context) error {
		if client.runCalls == 1 {
			// Another promotion completes, then an operator parks the node before
			// our retry. The retry must consult the authority after this STOP.
			_, err := client.authority.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
			require.NoError(t, err)
			_, err = client.authority.Idle(context.Background(), &emptypb.Empty{})
			require.NoError(t, err)
			return status.Error(codes.Unavailable, "lost transport response")
		}
		return nil
	}
	server.restoreFSMState(context.Background(), catchupCtx)
	requirePromotionState(t, client, store, blockchain.FSMStateIDLE)
	require.Equal(t, 2, client.runCalls, "IDLE refusal must stop further retries")
}
