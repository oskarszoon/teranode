package blockchain

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-chaincfg"
	teranodeerrors "github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/settings"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fsmPersistenceStore uses SQLite for all storage, injecting failures only at
// the FSM write boundary to exercise both definite and ambiguous write errors.
type fsmPersistenceStore struct {
	blockchainstore.Store
	mu                sync.Mutex
	writes            atomic.Int32
	writeErr          error
	commitBeforeError bool
	beforeWrite       func(context.Context)
}

func (s *fsmPersistenceStore) SetFSMState(ctx context.Context, state string) error {
	s.writes.Add(1)
	s.mu.Lock()
	beforeWrite, writeErr, commitBeforeError := s.beforeWrite, s.writeErr, s.commitBeforeError
	s.mu.Unlock()
	if beforeWrite != nil {
		beforeWrite(ctx)
	}
	if writeErr != nil && !commitBeforeError {
		return writeErr
	}
	if err := s.Store.SetFSMState(ctx, state); err != nil {
		return err
	}
	return writeErr
}

func TestFSMFailedCatchupDoesNotReplayAfterCallerReturns(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "before_commit"
		if committed {
			name = "lost_acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
			appCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			b.AppCtx = appCtx
			b.subscriptionManagerReady.Store(true)
			store.setFault(teranodeerrors.NewStorageError("catchup write failed"), committed)
			_, err := b.CatchUpBlocks(context.Background(), &emptypb.Empty{})
			require.Error(t, err)
			require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), b.finiteStateMachine.Current())
			require.Empty(t, b.notifications)
			require.True(t, b.fsmPersistenceUncertain)
			store.setFault(nil, false)
			require.Never(t, func() bool { return store.writes.Load() > 1 }, 750*time.Millisecond, 10*time.Millisecond,
				"no worker may finish CATCHUPBLOCKS after the caller received an error")
			persisted, err := store.GetFSMState(context.Background())
			require.NoError(t, err)
			if committed {
				require.Equal(t, blockchain_api.FSMStateType_CATCHINGBLOCKS.String(), persisted)
			} else {
				require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
			}
			_, err = b.ReadFSMState(context.Background(), &emptypb.Empty{})
			require.Error(t, err)
			_, err = b.Idle(context.Background(), &emptypb.Empty{})
			require.Error(t, err, "a different event cannot overwrite uncertain persistence")
			_, err = b.CatchUpBlocks(context.Background(), &emptypb.Empty{})
			require.NoError(t, err, "an explicit exact retry must reconcile the intent")
			require.Equal(t, blockchain_api.FSMStateType_CATCHINGBLOCKS.String(), b.finiteStateMachine.Current())
			require.Len(t, b.notifications, 1)
		})
	}
}

func TestFSMCanceledWhileQueuedDoesNotAdmitCatchup(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "convenience"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, done := make(chan struct{}), make(chan error, 1)
			b.fsmMu.Lock()
			go func() {
				close(started)
				if direct {
					_, err := b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_CATCHUPBLOCKS})
					done <- err
				} else {
					_, err := b.CatchUpBlocks(ctx, &emptypb.Empty{})
					done <- err
				}
			}()
			<-started
			cancel()
			b.fsmMu.Unlock()
			require.Error(t, <-done)
			require.Zero(t, store.writes.Load())
			require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), b.finiteStateMachine.Current())
			require.Empty(t, b.notifications)
			require.Nil(t, b.fsmPendingIntent)
		})
	}
}

func (s *fsmPersistenceStore) setFault(err error, commitBeforeError bool) {
	s.mu.Lock()
	s.writeErr = err
	s.commitBeforeError = commitBeforeError
	s.mu.Unlock()
}

func (s *fsmPersistenceStore) setBeforeWrite(fn func(context.Context)) {
	s.mu.Lock()
	s.beforeWrite = fn
	s.mu.Unlock()
}

type fsmRetryCheckpointStore struct {
	*fsmPersistenceStore
	height atomic.Uint32
	onRead func()
}

func (s *fsmRetryCheckpointStore) GetBestBlockHeader(context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	if s.onRead != nil {
		s.onRead()
	}
	return &model.BlockHeader{}, &model.BlockHeaderMeta{Height: s.height.Load()}, nil
}

func TestSendFSMEvent_CanceledDuringSuccessfulCheckpointReadDoesNotAdmit(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_IDLE)
	params := *b.settings.ChainCfgParams
	params.Checkpoints = []chaincfg.Checkpoint{{Height: 1}}
	b.settings.ChainCfgParams = &params
	checkpointStore := &fsmRetryCheckpointStore{fsmPersistenceStore: store}
	checkpointStore.height.Store(1)
	b.store = checkpointStore
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	checkpointStore.onRead = func() {
		reads++
		cancel()
	}

	resp, err := b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.Error(t, err)
	require.Nil(t, resp)
	require.Equal(t, 1, reads, "the RUN gate must finish a successful checkpoint read")
	require.Zero(t, store.writes.Load(), "cancellation before admission must not persist RUN")
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), b.finiteStateMachine.Current())
	require.Nil(t, b.fsmPendingIntent)
	require.Empty(t, b.notifications)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted)
}

func TestFSMUncertainRunRetryRechecksCheckpoint(t *testing.T) {
	b, faultStore := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_IDLE)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.AppCtx = ctx
	b.subscriptionManagerReady.Store(true)
	params := *b.settings.ChainCfgParams
	params.Checkpoints = []chaincfg.Checkpoint{{Height: 1}}
	b.settings.ChainCfgParams = &params
	checkpointStore := &fsmRetryCheckpointStore{fsmPersistenceStore: faultStore}
	checkpointStore.height.Store(1)
	b.store = checkpointStore
	faultStore.setFault(teranodeerrors.NewStorageError("RUN acknowledgement lost"), false)
	_, err := b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.Error(t, err)
	checkpointStore.height.Store(0)
	faultStore.setFault(nil, false)
	_, err = b.ReadFSMState(context.Background(), &emptypb.Empty{})
	require.Error(t, err, "below-checkpoint retry must leave authority unavailable")
	persisted, err := faultStore.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.Error(t, err, "an explicit retry must still obey the checkpoint gate")
	checkpointStore.height.Store(1)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	state, err := b.ReadFSMState(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING, state.State)
}

func TestFSMUncertainCallerRetryRetiresIntent(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.AppCtx = ctx
	store.setFault(teranodeerrors.NewStorageError("lost STOP acknowledgement"), true)
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	store.setFault(nil, false)
	_, err = b.Idle(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
	require.Len(t, b.notifications, 2, "retired STOP must publish once before RUN")
}

func TestFSMAdmittedWriteCompletesAfterCallerCancellation(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	store.setBeforeWrite(func(storeCtx context.Context) {
		close(entered)
		_, hasDeadline := storeCtx.Deadline()
		if !hasDeadline {
			panic("admitted write must remain bounded")
		}
		<-release
	})
	done := make(chan error, 1)
	go func() {
		_, err := b.Idle(ctx, &emptypb.Empty{})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("admitted write did not start")
	}
	cancel()
	close(release)
	require.NoError(t, <-done)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted,
		"an already admitted detached bounded write may finish after caller cancellation")
}

func TestFSMUncertainExplicitRetryPreservesCommittedStop(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.AppCtx = ctx
	b.subscriptionManagerReady.Store(true)
	store.setFault(teranodeerrors.NewStorageError("lost STOP acknowledgement"), true)
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	_, err = b.Run(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_CATCHUPBLOCKS})
	require.Error(t, err, "explicit different event cannot replace ambiguous STOP")
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted)
	store.setFault(nil, false)
	_, err = b.Idle(context.Background(), &emptypb.Empty{})
	require.NoError(t, err, "only the exact STOP retry may reconcile a lost acknowledgement")
	state, err := b.ReadFSMState(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE, state.State)
	require.Len(t, b.notifications, 1)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	persisted, err = store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted, "retired STOP must not overwrite a later RUN")
	require.Len(t, b.notifications, 2)
}

func TestFSMUncertainStopDoesNotReplayInBackground(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	b.AppCtx = ctx
	t.Cleanup(cancel)
	store.setFault(teranodeerrors.NewStorageError("persistent outage"), false)
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	store.setFault(nil, false)
	require.Never(t, func() bool { return store.writes.Load() > 1 }, 750*time.Millisecond, 10*time.Millisecond)
	require.True(t, b.fsmPersistenceUncertain)
}

func newFSMPersistenceTestBlockchain(t *testing.T, state blockchain_api.FSMStateType) (*Blockchain, *fsmPersistenceStore) {
	t.Helper()
	initPrometheusMetrics()
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)
	tSettings := &settings.Settings{ChainCfgParams: &chaincfg.RegressionNetParams}
	store, err := blockchainstore.NewStore(ulogger.TestLogger{}, storeURL, tSettings)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	require.NoError(t, store.SetFSMState(context.Background(), state.String()))
	faultStore := &fsmPersistenceStore{Store: store}
	b := &Blockchain{
		logger: ulogger.TestLogger{}, store: faultStore, settings: tSettings,
		notifications: make(chan *blockchain_api.Notification, 10),
		stats:         gocore.NewStat("blockchain-fsm-persistence-test"),
	}
	b.finiteStateMachine = b.NewFiniteStateMachine()
	b.finiteStateMachine.SetState(state.String())
	return b, faultStore
}

func TestSendFSMEvent_PersistenceFailureDoesNotTransition(t *testing.T) {
	for _, tt := range []struct {
		name  string
		from  blockchain_api.FSMStateType
		event blockchain_api.FSMEventType
		to    blockchain_api.FSMStateType
	}{
		{"run", blockchain_api.FSMStateType_IDLE, blockchain_api.FSMEventType_RUN, blockchain_api.FSMStateType_RUNNING},
		{"catchup", blockchain_api.FSMStateType_RUNNING, blockchain_api.FSMEventType_CATCHUPBLOCKS, blockchain_api.FSMStateType_CATCHINGBLOCKS},
		{"stop", blockchain_api.FSMStateType_RUNNING, blockchain_api.FSMEventType_STOP, blockchain_api.FSMStateType_IDLE},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, store := newFSMPersistenceTestBlockchain(t, tt.from)
			store.writeErr = teranodeerrors.NewStorageError("injected FSM write failure")
			priorTimestamp := time.Now().Add(-time.Minute)
			b.stateChangeTimestamp = priorTimestamp
			req := &blockchain_api.SendFSMEventRequest{Event: tt.event}
			resp, err := b.SendFSMEvent(context.Background(), req)
			require.Error(t, err)
			require.ErrorContains(t, teranodeerrors.UnwrapGRPC(err), "injected FSM write failure")
			require.Equal(t, teranodeerrors.ERR_STORAGE_ERROR, teranodeerrors.UnwrapGRPC(err).Code())
			require.Nil(t, resp)
			require.Equal(t, tt.from.String(), b.finiteStateMachine.Current())
			persisted, err := store.GetFSMState(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.from.String(), persisted)
			require.Empty(t, b.notifications)
			require.Equal(t, priorTimestamp, b.stateChangeTimestamp)

			store.writeErr = nil
			resp, err = b.SendFSMEvent(context.Background(), req)
			require.NoError(t, err, "failed write must leave the FSM available for retry")
			require.Equal(t, tt.to, resp.State)
			persisted, err = store.GetFSMState(context.Background())
			require.NoError(t, err)
			require.Equal(t, tt.to.String(), persisted)
			require.Len(t, b.notifications, 1)
		})
	}
}

func TestSendFSMEvent_PersistsBeforeStateAndNotification(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_IDLE)
	store.beforeWrite = func(ctx context.Context) {
		require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), b.finiteStateMachine.Current())
		require.Empty(t, b.notifications, "subscribers must not observe success before the write completes")
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "store write must have a bounded deadline")
		require.Positive(t, time.Until(deadline))
	}
	resp, err := b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING, resp.State)
	require.Len(t, b.notifications, 1)
	notification := <-b.notifications
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), notification.Metadata.Metadata["destination"])
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
}

func TestSendFSMEvent_AmbiguousWriteCanRetry(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	store.writeErr = teranodeerrors.NewStorageError("injected lost write acknowledgement")
	store.commitBeforeError = true
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	require.ErrorContains(t, teranodeerrors.UnwrapGRPC(err), "injected lost write acknowledgement")
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), b.finiteStateMachine.Current())
	require.Empty(t, b.notifications)
	// An error cannot establish whether the database committed. Do not roll back
	// the persisted state: an explicit retry rewrites the same target safely.
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted)
	store.writeErr = nil
	_, err = b.Idle(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), b.finiteStateMachine.Current())
	require.Len(t, b.notifications, 1)
}

func TestSendFSMEvent_CanceledCallerDuringPersistence(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_IDLE)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.beforeWrite = func(storeCtx context.Context) {
		cancel()
		require.NoError(t, storeCtx.Err(), "accepted write must survive caller cancellation")
	}
	resp, err := b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING, resp.State)
	state, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, state, b.finiteStateMachine.Current())
	require.Len(t, b.notifications, 1)
}

func TestSendFSMEvent_PersistenceTimeoutDoesNotWedge(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_IDLE)
	b.settings.BlockChain.StoreDBTimeoutMillis = 5
	store.beforeWrite = func(ctx context.Context) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("FSM write did not time out")
		}
	}
	req := &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN}
	resp, err := b.SendFSMEvent(context.Background(), req)
	require.Error(t, err)
	require.Nil(t, resp)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), b.finiteStateMachine.Current())
	require.Empty(t, b.notifications)
	store.beforeWrite = nil
	b.settings.BlockChain.StoreDBTimeoutMillis = 5000
	resp, err = b.SendFSMEvent(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING, resp.State)
}
