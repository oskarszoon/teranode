package blockchain

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCatchUpBlocksClientPreservesExhaustedRawUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	var calls atomic.Int32
	var detailed atomic.Bool
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == blockchain_api.BlockchainAPI_CatchUpBlocks_FullMethodName {
			calls.Add(1)
			if detailed.Load() {
				return nil, errors.WrapGRPC(errors.NewStateError("automatic catchup refused from IDLE"))
			}
			return nil, status.Error(codes.Unavailable, "temporary blockchain transport outage")
		}
		return handler(ctx, req)
	}))
	blockchain_api.RegisterBlockchainAPIServer(server, &blockchain_api.UnimplementedBlockchainAPIServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	settings := test.CreateBaseTestSettings(t)
	conn, err := util.GetGRPCClient(t.Context(), listener.Addr().String(), &util.ConnectionOptions{
		MaxRetries: 3, RetryBackoff: time.Millisecond, CallerName: "blockchain-fsm-test",
	}, settings)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	client := &Client{client: blockchain_api.NewBlockchainAPIClient(conn), logger: ulogger.TestLogger{}}
	err = client.CatchUpBlocks(t.Context())
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err), "outer catchup retry must still see raw transport Unavailable")
	require.Equal(t, int32(3), calls.Load(), "production interceptor must exhaust its configured inner attempts")

	detailed.Store(true)
	err = client.CatchUpBlocks(t.Context())
	require.Error(t, err)
	require.True(t, errors.Is(err, errors.ErrStateError), "native state verdict must retain its details")
	require.Equal(t, int32(4), calls.Load(), "nonretryable native verdict needs only one inner attempt")
}
