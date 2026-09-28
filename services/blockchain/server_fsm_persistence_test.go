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
	writeErr          error
	commitBeforeError bool
	beforeWrite       func(context.Context)
}

func (s *fsmPersistenceStore) SetFSMState(ctx context.Context, state string) error {
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
}

func (s *fsmRetryCheckpointStore) GetBestBlockHeader(context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	return &model.BlockHeader{}, &model.BlockHeaderMeta{Height: s.height.Load()}, nil
}

func TestFSMUncertainWorkerRechecksCheckpointBeforeRun(t *testing.T) {
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
	time.Sleep(3 * fsmPersistenceRetryInterval)
	_, err = b.ReadFSMState(context.Background(), &emptypb.Empty{})
	require.Error(t, err, "below-checkpoint retry must leave authority unavailable")
	persisted, err := faultStore.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted)
	checkpointStore.height.Store(1)
	require.Eventually(t, func() bool {
		state, readErr := b.ReadFSMState(context.Background(), &emptypb.Empty{})
		return readErr == nil && state.State == blockchain_api.FSMStateType_RUNNING
	}, 3*time.Second, 10*time.Millisecond)
}

func TestFSMUncertainCallerRetryRetiresWorkerIntent(t *testing.T) {
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
	time.Sleep(2 * fsmPersistenceRetryInterval)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted)
	require.Len(t, b.notifications, 2, "retired STOP must publish once and never replay after RUN")
}

func TestFSMUncertainWorkerShutdownDuringBoundedWrite(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	b.AppCtx = ctx
	store.setFault(teranodeerrors.NewStorageError("initial STOP failure"), false)
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	store.setBeforeWrite(func(storeCtx context.Context) {
		close(entered)
		_, hasDeadline := storeCtx.Deadline()
		if !hasDeadline {
			panic("replay write must remain bounded")
		}
		<-release
	})
	store.setFault(nil, false)
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
	cancel()
	close(release)
	require.Eventually(t, func() bool {
		b.fsmMu.RLock()
		defer b.fsmMu.RUnlock()
		return !b.fsmRetryRunning
	}, time.Second, 10*time.Millisecond)
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_IDLE.String(), persisted,
		"already admitted detached bounded replay may finish after shutdown")
}

func TestFSMUncertainWorkerPreservesCommittedStop(t *testing.T) {
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
	require.Eventually(t, func() bool {
		state, readErr := b.ReadFSMState(context.Background(), &emptypb.Empty{})
		return readErr == nil && state.State == blockchain_api.FSMStateType_IDLE
	}, 3*time.Second, 10*time.Millisecond)
	require.Len(t, b.notifications, 1)
	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err)
	time.Sleep(2 * fsmPersistenceRetryInterval)
	persisted, err = store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, blockchain_api.FSMStateType_RUNNING.String(), persisted, "retired STOP retry must never replay after a later RUN")
	require.Len(t, b.notifications, 2)
}

func TestFSMUncertainWorkerStopsOnShutdown(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, blockchain_api.FSMStateType_RUNNING)
	ctx, cancel := context.WithCancel(context.Background())
	b.AppCtx = ctx
	store.setFault(teranodeerrors.NewStorageError("persistent outage"), false)
	_, err := b.Idle(context.Background(), &emptypb.Empty{})
	require.Error(t, err)
	cancel()
	require.Eventually(t, func() bool {
		b.fsmMu.RLock()
		defer b.fsmMu.RUnlock()
		return !b.fsmRetryRunning
	}, time.Second, 10*time.Millisecond)
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
