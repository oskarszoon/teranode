package blockchain

import (
	"context"
	"testing"

	teranodeerrors "github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// reconciliationRPC routes the real Client through the real blockchain service;
// SQLite and the existing fault wrapper provide the actual persisted state.
type reconciliationRPC struct {
	blockchain_api.BlockchainAPIClient
	server *Blockchain
	calls  int
	runErr error
}

func (r *reconciliationRPC) Run(ctx context.Context, req *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	r.calls++
	if r.runErr != nil {
		return nil, r.runErr
	}
	return r.server.Run(ctx, req)
}
func (r *reconciliationRPC) Idle(ctx context.Context, req *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	r.calls++
	return r.server.Idle(ctx, req)
}
func (r *reconciliationRPC) CatchUpBlocks(ctx context.Context, req *emptypb.Empty, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	r.calls++
	return r.server.CatchUpBlocks(ctx, req)
}

func TestClient_ConvenienceCallsDoNotRewriteUncertainIntent(t *testing.T) {
	for _, tt := range []struct {
		name       string
		state      blockchain_api.FSMStateType
		event      blockchain_api.FSMEventType
		call       func(*Client, context.Context) error
		resultCall func(*Client, context.Context) error
	}{
		{"idle after ambiguous run", FSMStateIDLE, blockchain_api.FSMEventType_RUN, (*Client).Idle, func(c *Client, ctx context.Context) error { return c.Run(ctx, "test") }},
		{"run after ambiguous stop", FSMStateRUNNING, blockchain_api.FSMEventType_STOP, func(c *Client, ctx context.Context) error { return c.Run(ctx, "test") }, (*Client).Idle},
		{"catchup after ambiguous run", FSMStateCATCHINGBLOCKS, blockchain_api.FSMEventType_RUN, (*Client).CatchUpBlocks, func(c *Client, ctx context.Context) error { return c.Run(ctx, "test") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b, store := newFSMPersistenceTestBlockchain(t, tt.state)
			store.writeErr = teranodeerrors.NewStorageError("lost write acknowledgement")
			store.commitBeforeError = true
			_, err := b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: tt.event})
			require.Error(t, err)
			require.Equal(t, tt.state.String(), b.finiteStateMachine.Current())
			persisted, err := store.GetFSMState(ctx)
			require.NoError(t, err)
			require.NotEqual(t, tt.state.String(), persisted)

			rpc := &reconciliationRPC{server: b}
			client := &Client{client: rpc, logger: ulogger.TestLogger{}}
			client.fmsState.Store(&tt.state)
			store.commitBeforeError = false
			require.Error(t, tt.call(client, ctx), "a failed reconciliation cannot report success")
			require.Equal(t, 1, rpc.calls, "cached state must not bypass the authority")
			persisted, err = store.GetFSMState(ctx)
			require.NoError(t, err)
			require.NotEqual(t, tt.state.String(), persisted)
			require.Equal(t, tt.state.String(), b.finiteStateMachine.Current(), "a different event must not clear the pending intent")
			require.Empty(t, b.notifications, "a rejected event must not publish a notification")

			_, err = b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: tt.event})
			require.Error(t, err, "the exact event cannot succeed without an acknowledged write")
			require.Equal(t, tt.state.String(), b.finiteStateMachine.Current())
			store.writeErr = nil
			_, err = b.SendFSMEvent(ctx, &blockchain_api.SendFSMEventRequest{Event: tt.event})
			require.NoError(t, err, "only the exact original intent can reconcile ambiguous persistence")
			persisted, err = store.GetFSMState(ctx)
			require.NoError(t, err)
			require.NotEqual(t, tt.state.String(), persisted)
			require.Equal(t, persisted, b.finiteStateMachine.Current())
			require.Len(t, b.notifications, 1, "the acknowledged transition publishes exactly once")

			writes := 0
			store.setBeforeWrite(func(context.Context) { writes++ })
			require.NoError(t, tt.resultCall(client, ctx), "an acknowledged same-target call is a clean no-op")
			require.Zero(t, writes, "a clean no-op needs no additional write")
			require.Len(t, b.notifications, 1, "a clean no-op must not publish again")
		})
	}
}

func TestRun_AutomaticPromotionPreservesOperatorIdle(t *testing.T) {
	b, store := newFSMPersistenceTestBlockchain(t, FSMStateIDLE)
	_, err := b.Run(context.Background(), &emptypb.Empty{})
	require.ErrorContains(t, err, "automatic RUN")
	require.Equal(t, "IDLE", b.finiteStateMachine.Current())
	persisted, err := store.GetFSMState(context.Background())
	require.NoError(t, err)
	require.Equal(t, "IDLE", persisted)
	require.Empty(t, b.notifications)

	_, err = b.SendFSMEvent(context.Background(), &blockchain_api.SendFSMEventRequest{Event: blockchain_api.FSMEventType_RUN})
	require.NoError(t, err, "an explicit operator event may leave IDLE when checkpoint-safe")
	require.Equal(t, "RUNNING", b.finiteStateMachine.Current())
}

func TestClientRun_PreservesRetryableTransportStatus(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			rpc := &reconciliationRPC{runErr: status.Error(code, "transport failed")}
			client := &Client{client: rpc, logger: ulogger.TestLogger{}}
			require.Equal(t, code, status.Code(client.Run(context.Background(), "test")))
			require.Equal(t, 1, rpc.calls)
		})
	}
}
