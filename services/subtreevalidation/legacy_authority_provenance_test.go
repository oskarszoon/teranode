package subtreevalidation

import (
	"context"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation/subtreevalidation_api"
	"github.com/bsv-blockchain/teranode/services/validator"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestLegacyClientRetainsUnavailableAuthorityProvenanceAfterRetries(t *testing.T) {
	InitPrometheusMetrics()
	ctx := t.Context()
	logger := ulogger.TestLogger{}
	utxoStore, _, txStore, subtreeStore, localClient, cleanup := setup(t)
	t.Cleanup(cleanup)
	cfg := test.CreateBaseTestSettings(t)
	cfg.SubtreeValidation.QuorumPath = t.TempDir()
	cfg.BlockValidation.CheckSubtreeFromBlockRetries = 3
	cfg.BlockValidation.CheckSubtreeFromBlockRetryBackoffDuration = time.Millisecond
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)
	chainStore, err := blockchainstore.NewStore(logger, storeURL, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, chainStore.Close(context.Background())) })
	chainServer, err := blockchain.New(ctx, logger, cfg, chainStore, nil, blockchain.FSMStateRUNNING.String())
	require.NoError(t, err)
	require.NoError(t, chainServer.Init(ctx))
	authorityListener := bufconn.Listen(1024 * 1024)
	authorityGRPC := grpc.NewServer()
	blockchain_api.RegisterBlockchainAPIServer(authorityGRPC, chainServer)
	go func() { _ = authorityGRPC.Serve(authorityListener) }()
	t.Cleanup(authorityGRPC.Stop)
	authorityConn, err := grpc.NewClient("passthrough:///legacy-authority", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return authorityListener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authorityConn.Close()) })
	authorityClient := &sqliteGRPCAuthorityClient{ClientI: localClient, rpc: blockchain_api.NewBlockchainAPIClient(authorityConn)}
	recorder := newRecordingValidatorClient(&validator.MockValidator{UtxoStore: utxoStore})
	consumer := &kafka.KafkaConsumerGroup{}
	server, err := New(ctx, logger, cfg, subtreeStore, txStore, utxoStore, recorder, authorityClient, consumer, consumer, nil, nil)
	require.NoError(t, err)
	child := tx1.Clone()
	require.NoError(t, child.Inputs[0].PreviousTxIDAdd(parentTx1.TxIDChainHash()))
	child.Inputs[0].PreviousTxOutIndex = 0
	subtree, err := subtreepkg.NewTreeByLeafCount(1)
	require.NoError(t, err)
	require.NoError(t, subtree.AddNode(*child.TxIDChainHash(), 121, 0))
	serialized, err := subtree.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtreeToCheck, serialized))
	require.NoError(t, subtreeStore.Set(ctx, subtree.RootHash()[:], fileformat.FileTypeSubtreeData, child.ExtendedBytes()))

	var calls atomic.Int32
	var nativeVerdict atomic.Bool
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/subtreevalidation_api.SubtreeValidationAPI/CheckSubtreeFromBlock" {
			calls.Add(1)
			if nativeVerdict.Load() {
				return nil, errors.WrapGRPC(errors.NewBlockInvalidError("native block verdict"))
			}
		}
		return handler(ctx, req)
	}))
	subtreevalidation_api.RegisterSubtreeValidationAPIServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listen) }()
	t.Cleanup(grpcServer.Stop)
	cfg.SubtreeValidation.GRPCAddress = listen.Addr().String()
	clientI, err := NewClient(ctx, logger, cfg, "legacy-authority-test")
	require.NoError(t, err)
	client := clientI.(*Client)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	blockHash := parentTx1.TxIDChainHash()
	prevHash := parentTx1.TxIDChainHash()
	err = client.CheckSubtreeFromBlock(ctx, *subtree.RootHash(), "legacy", 100, blockHash, prevHash)
	require.Error(t, err)
	require.Equal(t, int32(3), calls.Load(), "the production gRPC interceptor must retry authority Unavailable")
	require.True(t, errors.IsTransientLocalError(err), "legacy sync must attribute an authority outage to this node")
	require.False(t, errors.Is(err, errors.ErrBlockInvalid))
	require.Empty(t, recorder.recordedOptions(*child.TxIDChainHash()), "unknown authority must stop before validation")

	chainServer.SetSubscriptionManagerReadyForTesting(true)
	require.NoError(t, client.CheckSubtreeFromBlock(ctx, *subtree.RootHash(), "legacy", 100, blockHash, prevHash), "the same request resumes when authority is ready")
	require.NotEmpty(t, recorder.recordedOptions(*child.TxIDChainHash()))
	require.Equal(t, int32(4), calls.Load())

	nativeVerdict.Store(true)
	err = client.CheckSubtreeFromBlock(ctx, *subtree.RootHash(), "legacy", 100, blockHash, prevHash)
	require.Error(t, err)
	require.Equal(t, int32(5), calls.Load(), "native verdicts must not be retried")
	require.True(t, errors.Is(err, errors.ErrBlockInvalid))
	require.False(t, errors.IsTransientLocalError(err))
}
