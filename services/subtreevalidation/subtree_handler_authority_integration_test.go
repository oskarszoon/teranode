package subtreevalidation

import (
	"context"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/validator"
	blobmemory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	blockchainstore "github.com/bsv-blockchain/teranode/stores/blockchain"
	utxosql "github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Chain reads use LocalClient's real SQLite store; authority always traverses
// gRPC to the real Blockchain FSM and its readiness gate.
type sqliteGRPCAuthorityClient struct {
	blockchain.ClientI
	rpc blockchain_api.BlockchainAPIClient
}

func (c *sqliteGRPCAuthorityClient) ReadFSMState(ctx context.Context) (blockchain.FSMStateType, error) {
	response, err := c.rpc.ReadFSMState(ctx, &emptypb.Empty{})
	if err != nil {
		return blockchain.FSMStateIDLE, err
	}
	return response.State, nil
}

func TestSubtreeMessageRetainsFeedUntilSQLiteAuthorityReady(t *testing.T) {
	InitPrometheusMetrics()
	ctx := t.Context()
	logger := ulogger.TestLogger{}
	tSettings := test.CreateBaseTestSettings(t)
	tSettings.SubtreeValidation.QuorumPath = t.TempDir()
	storeURL, err := url.Parse("sqlitememory:///")
	require.NoError(t, err)
	chainStore, err := blockchainstore.NewStore(logger, storeURL, tSettings)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, chainStore.Close(context.Background())) })
	utxoStore, err := utxosql.New(ctx, logger, tSettings, storeURL)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, utxoStore.Close(context.Background())) })
	subtreeStore, txStore := blobmemory.New(), blobmemory.New()
	chainServer, err := blockchain.New(ctx, logger, tSettings, chainStore, nil, blockchain.FSMStateRUNNING.String())
	require.NoError(t, err)
	require.NoError(t, chainServer.Init(ctx))
	// Keep subscription readiness false for the first read, then release it.
	firstRead := make(chan struct{})
	var firstOnce sync.Once
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if info.FullMethod == "/blockchain_api.BlockchainAPI/ReadFSMState" {
			firstOnce.Do(func() { close(firstRead) })
		}
		return resp, err
	}))
	blockchain_api.RegisterBlockchainAPIServer(grpcServer, chainServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient("passthrough:///subtree-sqlite-authority", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	localClient, err := blockchain.NewLocalClient(logger, tSettings, chainStore, subtreeStore, utxoStore)
	require.NoError(t, err)
	client := &sqliteGRPCAuthorityClient{ClientI: localClient, rpc: blockchain_api.NewBlockchainAPIClient(conn)}
	recorder := newRecordingValidatorClient(&validator.MockValidator{UtxoStore: utxoStore})
	consumer := &kafka.KafkaConsumerGroup{}
	server, err := New(ctx, logger, tSettings, subtreeStore, txStore, utxoStore, recorder, client, consumer, consumer, nil, nil)
	require.NoError(t, err)
	server.bestBlockHeaderMeta.Store(&model.BlockHeaderMeta{Height: 99})
	blockIDs := map[uint32]bool{}
	server.currentBlockIDsMap.Store(&blockIDs)
	child := tx1.Clone()
	require.NoError(t, child.Inputs[0].PreviousTxIDAdd(parentTx1.TxIDChainHash()))
	child.Inputs[0].PreviousTxOutIndex = 0
	_, err = utxoStore.Create(ctx, parentTx1, 99)
	require.NoError(t, err)
	st, err := subtreepkg.NewTreeByLeafCount(1)
	require.NoError(t, err)
	require.NoError(t, st.AddNode(*child.TxIDChainHash(), 121, 0))
	serialized, err := st.Serialize()
	require.NoError(t, err)
	require.NoError(t, subtreeStore.Set(ctx, st.RootHash()[:], fileformat.FileTypeSubtreeToCheck, serialized))
	require.NoError(t, subtreeStore.Set(ctx, st.RootHash()[:], fileformat.FileTypeSubtreeData, child.ExtendedBytes()))
	payload, err := proto.Marshal(&kafkamessage.KafkaSubtreeTopicMessage{Hash: st.RootHash().String(), URL: "http://peer.invalid", PeerId: "peer-1"})
	require.NoError(t, err)

	finished := make(chan error, 1)
	go func() { finished <- server.subtreeMessageHandler(ctx)(&kafka.KafkaMessage{Value: payload}) }()
	require.Eventually(t, func() bool {
		select {
		case <-firstRead:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)
	select {
	case err := <-finished:
		t.Fatalf("subtree record was discarded while FSM authority was unavailable: %v", err)
	default:
	}
	require.Empty(t, recorder.recordedOptions(*child.TxIDChainHash()))
	chainServer.SetSubscriptionManagerReadyForTesting(true)
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("subtree record did not resume after FSM authority became ready")
	}
	require.Eventually(t, func() bool {
		exists, existsErr := subtreeStore.Exists(ctx, st.RootHash()[:], fileformat.FileTypeSubtree)
		return existsErr == nil && exists
	}, 5*time.Second, 10*time.Millisecond)
	recorded := recorder.recordedOptions(*child.TxIDChainHash())
	require.NotEmpty(t, recorded)
	for _, opts := range recorded {
		require.True(t, opts.AddTXToBlockAssembly)
	}
}
