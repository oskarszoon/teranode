package subtreevalidation

import (
	"context"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation/subtreevalidation_api"
	"github.com/bsv-blockchain/teranode/services/validator"
	blobmemory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestBulkClientPreservesUnavailableAuthorityProvenanceAfterRetries(t *testing.T) {
	InitPrometheusMetrics()
	ctx := t.Context()
	logger := ulogger.TestLogger{}
	cfg := test.CreateBaseTestSettings(t)
	cfg.SubtreeValidation.QuorumPath = t.TempDir()
	cfg.BlockValidation.CheckSubtreeFromBlockRetries = 3
	cfg.BlockValidation.CheckSubtreeFromBlockRetryBackoffDuration = time.Millisecond
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)
	chainStore, err := blockchainstore.NewStore(logger, storeURL, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, chainStore.Close(context.Background())) })
	utxoStore, err := utxosql.New(ctx, logger, cfg, storeURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, utxoStore.Close(context.Background())) })
	subtreeStore, txStore := blobmemory.New(), blobmemory.New()
	chainServer, err := blockchain.New(ctx, logger, cfg, chainStore, nil, blockchain.FSMStateRUNNING.String())
	require.NoError(t, err)
	require.NoError(t, chainServer.Init(ctx))
	authorityListener := bufconn.Listen(1024 * 1024)
	authorityGRPC := grpc.NewServer()
	blockchain_api.RegisterBlockchainAPIServer(authorityGRPC, chainServer)
	go func() { _ = authorityGRPC.Serve(authorityListener) }()
	t.Cleanup(authorityGRPC.Stop)
	authorityConn, err := grpc.NewClient("passthrough:///bulk-sqlite-authority", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return authorityListener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authorityConn.Close()) })
	localClient, err := blockchain.NewLocalClient(logger, cfg, chainStore, subtreeStore, utxoStore)
	require.NoError(t, err)
	authorityClient := &sqliteGRPCAuthorityClient{ClientI: localClient, rpc: blockchain_api.NewBlockchainAPIClient(authorityConn)}
	validatorClient := newRecordingValidatorClient(&validator.MockValidator{UtxoStore: utxoStore})
	consumer := &kafka.KafkaConsumerGroup{}
	server, err := New(ctx, logger, cfg, subtreeStore, txStore, utxoStore, validatorClient, authorityClient, consumer, consumer, nil, nil)
	require.NoError(t, err)

	var calls atomic.Int32
	var returnNative atomic.Bool
	var returnDetailedUnavailable atomic.Bool
	detail, err := anypb.New(&errors.TError{Code: errors.ERR_SERVICE_UNAVAILABLE, Message: "native service unavailable"})
	require.NoError(t, err)
	detailedUnavailable, err := status.New(codes.Unavailable, "native service unavailable").WithDetails(detail)
	require.NoError(t, err)
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/subtreevalidation_api.SubtreeValidationAPI/CheckBlockSubtrees" {
			calls.Add(1)
			if returnDetailedUnavailable.Load() {
				return nil, detailedUnavailable.Err()
			}
			if returnNative.Load() {
				return nil, errors.WrapGRPC(errors.NewBlockInvalidError("peer block invalid"))
			}
		}
		return handler(ctx, req)
	}))
	subtreevalidation_api.RegisterSubtreeValidationAPIServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listen) }()
	t.Cleanup(grpcServer.Stop)
	cfg.SubtreeValidation.GRPCAddress = listen.Addr().String()
	clientI, err := NewClient(ctx, logger, cfg, "bulk-authority-test")
	require.NoError(t, err)
	client := clientI.(*Client)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	req := shortCircuitTestBlock(t, &chainhash.Hash{0x72})
	block, err := model.NewBlockFromBytes(req.Block)
	require.NoError(t, err)
	// The real SQLite authority has initialized RUNNING but subscription
	// readiness is false. Each bulk call must fail before validation begins.
	_, err = chainServer.ReadFSMState(ctx, &emptypb.Empty{})
	require.Error(t, err)
	err = client.CheckBlockSubtrees(ctx, block, "peer-1", req.BaseUrl)
	require.Error(t, err)
	require.Equal(t, int32(3), calls.Load(), "production interceptor must exhaust its configured attempts")
	validatorClient.mu.Lock()
	validationCalls := len(validatorClient.callsByTx)
	validatorClient.mu.Unlock()
	require.Zero(t, validationCalls, "unknown authority must stop before transaction validation")
	require.True(t, errors.IsTransientLocalError(err), "an exhausted local authority outage must retain local provenance")
	require.False(t, errors.Is(err, errors.ErrBlockInvalid))
	require.False(t, errors.Is(err, errors.ErrTxInvalid))
	require.ErrorContains(t, err, "FSM")

	// Detailed native verdicts must keep their original classification and
	// bypass the Unavailable-only conversion and its retry branch.
	returnNative.Store(true)
	err = client.CheckBlockSubtrees(ctx, block, "peer-1", req.BaseUrl)
	require.Error(t, err)
	require.Equal(t, int32(4), calls.Load())
	require.True(t, errors.Is(err, errors.ErrBlockInvalid))
	require.False(t, errors.IsTransientLocalError(err))

	// A native detail remains authoritative even when its transport code is
	// Unavailable and the existing interceptor exhausts three attempts.
	returnNative.Store(false)
	returnDetailedUnavailable.Store(true)
	err = client.CheckBlockSubtrees(ctx, block, "peer-1", req.BaseUrl)
	require.Error(t, err)
	require.Equal(t, int32(7), calls.Load())
	require.True(t, errors.Is(err, errors.ErrServiceUnavailable))
	require.False(t, errors.Is(err, errors.ErrServiceError))
}
