package subtreevalidation

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	inmemorykafka "github.com/bsv-blockchain/teranode/util/kafka/in_memory_kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

type upgradeWarningLogger struct {
	ulogger.Logger
	warnings atomic.Int32
}

func (l *upgradeWarningLogger) Warnf(format string, args ...interface{}) {
	if strings.Contains(format, "upgrade blockchain") {
		l.warnings.Add(1)
	}
	l.Logger.Warnf(format, args...)
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

func TestSubtreeMessageRetainsUnimplementedUntilUpgradeOrClose(t *testing.T) {
	for _, closeBeforeUpgrade := range []bool{false, true} {
		name := "upgrade"
		if closeBeforeUpgrade {
			name = "close_with_live_parent"
		}
		t.Run(name, func(t *testing.T) {
			InitPrometheusMetrics()
			ctx := t.Context()
			logger := &upgradeWarningLogger{Logger: ulogger.TestLogger{}}
			cfg := test.CreateBaseTestSettings(t)
			cfg.SubtreeValidation.QuorumPath = t.TempDir()
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
			chainServer.SetSubscriptionManagerReadyForTesting(true)
			var oldServer atomic.Bool
			oldServer.Store(true)
			var reads atomic.Int32
			listener := bufconn.Listen(1024 * 1024)
			grpcServer := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				if info.FullMethod == "/blockchain_api.BlockchainAPI/ReadFSMState" {
					reads.Add(1)
					if oldServer.Load() {
						return nil, status.Error(codes.Unimplemented, "older blockchain server")
					}
				}
				return handler(ctx, req)
			}))
			blockchain_api.RegisterBlockchainAPIServer(grpcServer, chainServer)
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(grpcServer.Stop)
			conn, err := grpc.NewClient("passthrough:///subtree-upgrade-authority", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			localClient, err := blockchain.NewLocalClient(logger, cfg, chainStore, subtreeStore, utxoStore)
			require.NoError(t, err)
			authorityClient := &sqliteGRPCAuthorityClient{ClientI: localClient, rpc: blockchain_api.NewBlockchainAPIClient(conn)}
			recorder := newRecordingValidatorClient(&validator.MockValidator{UtxoStore: utxoStore})
			consumerForServer := &kafka.KafkaConsumerGroup{}
			server, err := New(ctx, logger, cfg, subtreeStore, txStore, utxoStore, recorder, authorityClient, consumerForServer, consumerForServer, nil, nil)
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

			topic := fmt.Sprintf("subtree-upgrade-%d", time.Now().UnixNano())
			broker := inmemorykafka.GetSharedBroker()
			broker.DropTopic(topic)
			t.Cleanup(func() { broker.DropTopic(topic) })
			kafkaURL, err := url.Parse("memory://localhost/" + topic)
			require.NoError(t, err)
			consumer, err := kafka.NewKafkaConsumerGroupFromURL(logger, kafkaURL, topic+"-group", true, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, consumer.Close()) })
			handlerCtx, handlerCancel := context.WithCancel(ctx)
			defer handlerCancel()
			finished := make(chan error, 1)
			handler := server.subtreeMessageHandler(handlerCtx)
			consumer.Start(ctx, func(msg *kafka.KafkaMessage) error {
				err := handler(msg)
				finished <- err
				return err
			}, kafka.WithLogErrorAndMoveOn(), kafka.WithWaitForFetchHandlers(handlerCancel))
			require.Eventually(t, func() bool { return broker.HasConsumer(topic) }, time.Second, 5*time.Millisecond)
			require.NoError(t, broker.Produce(ctx, topic, []byte("subtree"), payload))
			require.Eventually(t, func() bool { return reads.Load() >= 2 }, 3*time.Second, 10*time.Millisecond)
			require.Equal(t, int32(1), logger.warnings.Load(), "repeat Unimplemented must emit one actionable warning per retained record")
			select {
			case err := <-finished:
				t.Fatalf("subtree record was discarded while RPC was Unimplemented: %v", err)
			default:
			}
			require.Empty(t, recorder.recordedOptions(*child.TxIDChainHash()))

			if closeBeforeUpgrade {
				closed := make(chan error, 1)
				go func() { closed <- consumer.Close() }()
				select {
				case err := <-closed:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Fatal("Close blocked behind Unimplemented authority with live parent")
				}
				select {
				case err := <-finished:
					require.Error(t, err, "canceled handler must leave the record uncommitted")
				case <-time.After(3 * time.Second):
					t.Fatal("authority retry did not stop after Close")
				}
				return
			}

			oldServer.Store(false)
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("subtree record did not resume after blockchain upgrade")
			}
			require.Eventually(t, func() bool {
				return len(recorder.recordedOptions(*child.TxIDChainHash())) > 0
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, int32(1), logger.warnings.Load())
		})
	}
}
