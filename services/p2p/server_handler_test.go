package p2p

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/p2p/p2p_api"
	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
)

// --- handleRejectedTxTopic tests ---

func TestHandleRejectedTxTopic(t *testing.T) {
	t.Run("valid message from matching peer", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		selfPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		_, pub2, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		remotePeerID, err := peer.IDFromPublicKey(pub2)
		require.NoError(t, err)

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = selfPeerID

		reg := &mockPeerRegistryClient{}
		reg.On("IsPeerBanned", mock.Anything).Return(false, nil)
		reg.On("RegisterPeer", mock.Anything).Return(nil)
		reg.On("UpdatePeerMetrics", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		reg.On("GetPeer", mock.Anything).Return((*blockchain.PeerInfo)(nil), false, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull

		server := &Server{
			logger:          ulogger.New("test"),
			P2PClient:       mockP2P,
			centralRegistry: reg,
			settings:        tSettings,
			notificationCh:  make(chan *notificationMsg, 10),
		}

		msg := RejectedTxMessage{
			PeerID: remotePeerID.String(),
			TxID:   "abc123",
			Reason: "invalid",
		}
		msgBytes, err := json.Marshal(msg)
		require.NoError(t, err)

		server.handleRejectedTxTopic(context.Background(), msgBytes, remotePeerID.String())

		// Verify the handler completes without errors (central registry receives calls asynchronously)
	})

	t.Run("invalid json returns early", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		// Should not panic on invalid JSON
		server.handleRejectedTxTopic(context.Background(), []byte("not-json"), "peer1")
	})

	t.Run("mismatched fromID and peerID returns early", func(t *testing.T) {
		mockP2P := new(MockServerP2PClient)
		mockP2P.On("GetID").Return(peer.ID("self-peer"))

		server := &Server{
			logger:    ulogger.New("test"),
			P2PClient: mockP2P,
		}

		msg := RejectedTxMessage{
			PeerID: "peer-A",
			TxID:   "tx1",
			Reason: "test",
		}
		msgBytes, _ := json.Marshal(msg)

		// fromID is "peer-B" but message says peer-A: should log error and return
		server.handleRejectedTxTopic(context.Background(), msgBytes, "peer-B")
	})

	t.Run("own message is ignored", func(t *testing.T) {
		selfID := peer.ID("self-peer")
		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = selfID

		server := &Server{
			logger:    ulogger.New("test"),
			P2PClient: mockP2P,
		}

		msg := RejectedTxMessage{
			PeerID: selfID.String(),
			TxID:   "tx1",
			Reason: "test",
		}
		msgBytes, _ := json.Marshal(msg)

		// Should return early without updating anything
		server.handleRejectedTxTopic(context.Background(), msgBytes, selfID.String())
	})
}

// --- handlePeerFailureNotification tests ---

func TestHandlePeerFailureNotification(t *testing.T) {
	t.Run("nil metadata returns nil", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		notification := &blockchain.Notification{
			Metadata: nil,
		}

		err := server.handlePeerFailureNotification(context.Background(), notification)
		require.NoError(t, err)
	})

	t.Run("nil metadata map returns nil", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		notification := &blockchain.Notification{
			Metadata: &blockchain.NotificationMetadata{
				Metadata: nil,
			},
		}

		err := server.handlePeerFailureNotification(context.Background(), notification)
		require.NoError(t, err)
	})

	t.Run("catchup failure records in central registry", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", "12D3KooWTest", uint32(0), uint64(0), uint64(0), false, true, false, int64(0)).Return(nil)

		server := &Server{
			logger:          ulogger.New("test"),
			gCtx:            context.Background(),
			centralRegistry: reg,
		}

		notification := &blockchain.Notification{
			Metadata: &blockchain.NotificationMetadata{
				Metadata: map[string]string{
					"peer_id":      "12D3KooWTest",
					"failure_type": "catchup",
					"reason":       "timeout downloading block",
				},
			},
		}

		err := server.handlePeerFailureNotification(context.Background(), notification)
		require.NoError(t, err)

		// Allow async goroutine to execute
		time.Sleep(50 * time.Millisecond)
	})

	t.Run("non-catchup failure does not trigger sync coordinator", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		notification := &blockchain.Notification{
			Metadata: &blockchain.NotificationMetadata{
				Metadata: map[string]string{
					"peer_id":      "12D3KooWTest",
					"failure_type": "validation",
					"reason":       "invalid block",
				},
			},
		}

		err := server.handlePeerFailureNotification(context.Background(), notification)
		require.NoError(t, err)
	})
}

// --- processBlockchainNotification PeerFailure tests ---

func TestProcessBlockchainNotificationPeerFailure(t *testing.T) {
	hash := &chainhash.Hash{0x1}
	hashBytes := hash.CloneBytes()

	notification := &blockchain.Notification{
		Type: model.NotificationType_PeerFailure,
		Hash: hashBytes[:],
		Metadata: &blockchain.NotificationMetadata{
			Metadata: map[string]string{
				"peer_id":      "test-peer",
				"failure_type": "catchup",
				"reason":       "timeout",
			},
		},
	}

	server := &Server{
		logger: ulogger.New("test"),
	}

	err := server.processBlockchainNotification(context.Background(), notification)
	require.NoError(t, err)
}

// --- handleNodeStatusNotification tests ---

func TestHandleNodeStatusNotification(t *testing.T) {
	t.Run("successful publish", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		myPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		header := model.GenesisBlockHeader
		meta := &model.BlockHeaderMeta{Height: 100, ChainWork: []byte{0x01, 0x02}}

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = myPeerID
		mockP2P.On("Publish", mock.Anything, mock.Anything, mock.Anything).Return(nil)

		mockBC := new(blockchain.Mock)
		mockBC.On("GetBestBlockHeader", mock.Anything).Return(header, meta, nil)
		fsmState := blockchain_api.FSMStateType_RUNNING
		mockBC.On("GetFSMCurrentState", mock.Anything).Return(&fsmState, nil)

		blockPersisterData := make([]byte, 4)
		binary.LittleEndian.PutUint32(blockPersisterData, 0)
		mockBC.On("GetState", mock.Anything, "BlockPersisterHeight").Return(blockPersisterData, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull
		tSettings.Version = "v1.0.0"
		tSettings.Commit = "abc123"
		tSettings.ClientName = "test-node"

		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    mockBC,
			settings:            tSettings,
			startTime:           time.Now(),
			syncConnectionTimes: sync.Map{},
			notificationCh:      make(chan *notificationMsg, 10),
			nodeStatusTopicName: "test-node-status",
			AssetHTTPAddressURL: testAssetURL,
			centralRegistry:     reg,
		}

		err = server.handleNodeStatusNotification(context.Background())
		require.NoError(t, err)

		// Verify notification was sent to channel
		select {
		case msg := <-server.notificationCh:
			assert.Equal(t, "node_status", msg.Type)
			assert.Equal(t, myPeerID.String(), msg.PeerID)
		default:
			t.Fatal("expected notification in channel")
		}

		mockP2P.AssertCalled(t, "Publish", mock.Anything, "test-node-status", mock.Anything)
	})

	t.Run("publish error returns error", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		errPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = errPeerID
		mockP2P.On("Publish", mock.Anything, mock.Anything, mock.Anything).Return(assert.AnError)

		mockBC := new(blockchain.Mock)
		mockBC.On("GetBestBlockHeader", mock.Anything).Return(model.GenesisBlockHeader, model.GenesisBlockHeaderMeta, nil)
		fsmState := blockchain_api.FSMStateType_RUNNING
		mockBC.On("GetFSMCurrentState", mock.Anything).Return(&fsmState, nil)

		blockPersisterData := make([]byte, 4)
		mockBC.On("GetState", mock.Anything, "BlockPersisterHeight").Return(blockPersisterData, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull

		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    mockBC,
			settings:            tSettings,
			startTime:           time.Now(),
			syncConnectionTimes: sync.Map{},
			notificationCh:      make(chan *notificationMsg, 10),
			nodeStatusTopicName: "node-status",
			centralRegistry:     reg,
		}

		err = server.handleNodeStatusNotification(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "publish error")
	})
}

// --- getNodeStatusMessage tests ---

func TestGetNodeStatusMessage(t *testing.T) {
	t.Run("with blockchain client returning best block", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		myPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		header := model.GenesisBlockHeader
		meta := &model.BlockHeaderMeta{
			Height:    42,
			Miner:     "TestMiner",
			ChainWork: []byte{0x00, 0x01},
		}

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = myPeerID

		mockBC := new(blockchain.Mock)
		mockBC.On("GetBestBlockHeader", mock.Anything).Return(header, meta, nil)
		fsmState := blockchain_api.FSMStateType_RUNNING
		mockBC.On("GetFSMCurrentState", mock.Anything).Return(&fsmState, nil)

		blockPersisterData := make([]byte, 4)
		binary.LittleEndian.PutUint32(blockPersisterData, 40)
		mockBC.On("GetState", mock.Anything, "BlockPersisterHeight").Return(blockPersisterData, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull
		tSettings.Version = "v2.0.0"
		tSettings.Commit = "def456"
		tSettings.ClientName = "my-client"

		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    mockBC,
			settings:            tSettings,
			startTime:           time.Now().Add(-10 * time.Minute),
			syncConnectionTimes: sync.Map{},
			AssetHTTPAddressURL: testAssetURL,
			PropagationURL:      testPropagationURL,
			centralRegistry:     reg,
		}

		msg := server.getNodeStatusMessage(context.Background())
		require.NotNil(t, msg)
		assert.Equal(t, "node_status", msg.Type)
		assert.Equal(t, myPeerID.String(), msg.PeerID)
		assert.Equal(t, uint32(42), msg.BestHeight)
		assert.Equal(t, "v2.0.0", msg.Version)
		assert.Equal(t, "def456", msg.CommitHash)
		assert.Equal(t, "my-client", msg.ClientName)
		assert.Equal(t, "TestMiner", msg.MinerName)
		assert.Equal(t, "RUNNING", msg.FSMState)
		assert.Equal(t, testAssetURL, msg.BaseURL)
		assert.Equal(t, testPropagationURL, msg.PropagationURL)
		assert.Greater(t, msg.Uptime, float64(0))
	})

	t.Run("nil blockchain client uses genesis fallback", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = testPeerID

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    nil,
			settings:            tSettings,
			startTime:           time.Now(),
			syncConnectionTimes: sync.Map{},
		}

		msg := server.getNodeStatusMessage(context.Background())
		require.NotNil(t, msg)
		assert.Equal(t, model.GenesisBlockHeaderMeta.Height, msg.BestHeight)
	})

	t.Run("listen only mode clears baseURL and propagationURL", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = testPeerID

		mockBC := new(blockchain.Mock)
		mockBC.On("GetBestBlockHeader", mock.Anything).Return(model.GenesisBlockHeader, model.GenesisBlockHeaderMeta, nil)
		fsmState := blockchain_api.FSMStateType_RUNNING
		mockBC.On("GetFSMCurrentState", mock.Anything).Return(&fsmState, nil)

		blockPersisterData := make([]byte, 4)
		mockBC.On("GetState", mock.Anything, "BlockPersisterHeight").Return(blockPersisterData, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeListenOnly

		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    mockBC,
			settings:            tSettings,
			startTime:           time.Now(),
			syncConnectionTimes: sync.Map{},
			AssetHTTPAddressURL: testAssetURL,
			PropagationURL:      testPropagationURL,
			centralRegistry:     reg,
		}

		msg := server.getNodeStatusMessage(context.Background())
		require.NotNil(t, msg)
		assert.Empty(t, msg.BaseURL)
		assert.Empty(t, msg.PropagationURL)
	})

	t.Run("with connected peers counted", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		_, pub1, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		countPeer1, err := peer.IDFromPublicKey(pub1)
		require.NoError(t, err)

		_, pub2, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		countPeer2, err := peer.IDFromPublicKey(pub2)
		require.NoError(t, err)

		mockP2P := new(MockServerP2PClient)
		mockP2P.peerID = testPeerID

		mockBC := new(blockchain.Mock)
		mockBC.On("GetBestBlockHeader", mock.Anything).Return(model.GenesisBlockHeader, model.GenesisBlockHeaderMeta, nil)
		fsmState := blockchain_api.FSMStateType_RUNNING
		mockBC.On("GetFSMCurrentState", mock.Anything).Return(&fsmState, nil)

		blockPersisterData := make([]byte, 4)
		mockBC.On("GetState", mock.Anything, "BlockPersisterHeight").Return(blockPersisterData, nil)

		tSettings := createBaseTestSettings()
		tSettings.P2P.ListenMode = settings.ListenModeFull

		// Central registry returns 2 peers
		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{ID: countPeer1.String(), Height: 100},
			{ID: countPeer2.String(), Height: 200},
		}, nil)

		server := &Server{
			logger:              ulogger.New("test"),
			P2PClient:           mockP2P,
			blockchainClient:    mockBC,
			settings:            tSettings,
			startTime:           time.Now(),
			syncConnectionTimes: sync.Map{},
			centralRegistry:     reg,
		}

		msg := server.getNodeStatusMessage(context.Background())
		require.NotNil(t, msg)
		assert.Equal(t, 2, msg.ConnectedPeersCount)
	})
}

// --- updatePeerLastMessageTime tests ---
// TODO: adapt to central registry — updatePeerLastMessageTime now delegates to centralRegistry.
// These tests verified local PeerRegistry state changes which no longer occur.

func TestUpdatePeerLastMessageTime(t *testing.T) {
	t.Run("nil central registry does not panic", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}
		// Should not panic
		server.updatePeerLastMessageTime("peer1", "peer2")
	})

	t.Run("invalid sender peer ID logs error", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		// Invalid base58 peer ID - should log error and return
		server.updatePeerLastMessageTime("not-a-valid-peer-id", "")
	})
}

// --- updateBytesReceived tests ---
// TODO: adapt to central registry — updateBytesReceived now delegates to centralRegistry.UpdatePeerMetrics.

func TestUpdateBytesReceived(t *testing.T) {
	t.Run("nil central registry does not panic", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}
		server.updateBytesReceived("peer1", "peer2", 1024)
	})

	t.Run("invalid sender ID logs error", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		// Should not panic on invalid peer ID
		server.updateBytesReceived("invalid-peer", "", 100)
	})
}

// Constants for handler tests
const (
	testPeerIDStr1     = "peer-1"
	testPeerIDStr2     = "peer-2"
	testAssetURL       = "http://example.com"
	testPropagationURL = "http://propagation.example.com"
)

// --- RecordBytesDownloaded gRPC tests ---

func TestRecordBytesDownloaded(t *testing.T) {
	t.Run("successful recording via central registry", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		reg := &mockPeerRegistryClient{}
		reg.On("UpdatePeerMetrics", testPeerID.String(), uint32(0), uint64(0), uint64(5000), false, false, false, int64(0)).Return(nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
		}

		req := &p2p_api.RecordBytesDownloadedRequest{
			PeerId:          testPeerID.String(),
			BytesDownloaded: 5000,
		}

		resp, err := server.RecordBytesDownloaded(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, resp.Ok)
	})

	t.Run("nil central registry still returns ok", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		server := &Server{
			logger: ulogger.New("test"),
		}

		req := &p2p_api.RecordBytesDownloadedRequest{
			PeerId:          testPeerID.String(),
			BytesDownloaded: 5000,
		}

		resp, err := server.RecordBytesDownloaded(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, resp.Ok)
	})
}

// --- ResetReputation gRPC tests ---
// TODO: adapt to central registry — ResetReputation is not yet implemented via central registry.

func TestResetReputation(t *testing.T) {
	t.Run("returns success stub", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		req := &p2p_api.ResetReputationRequest{
			PeerId: "",
		}

		resp, err := server.ResetReputation(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, resp.Ok)
		assert.Equal(t, int32(0), resp.PeersReset)
	})
}

// --- GetPeerRegistry gRPC tests ---

func TestGetPeerRegistry(t *testing.T) {
	t.Run("nil central registry returns empty list", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		resp, err := server.GetPeerRegistry(context.Background(), &emptypb.Empty{})
		require.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Empty(t, resp.Peers)
	})

	t.Run("returns all peers from central registry", func(t *testing.T) {
		blockHash, _ := chainhash.NewHashFromStr("000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f")

		_, pub1, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		peerID1, err := peer.IDFromPublicKey(pub1)
		require.NoError(t, err)

		_, pub2, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		peerID2, err := peer.IDFromPublicKey(pub2)
		require.NoError(t, err)

		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{
			{
				ID:                   peerID1.String(),
				Height:               100,
				BlockHash:            blockHash,
				DataHubURL:           "http://peer1:8080",
				InteractionSuccesses: 1,
				Storage:              "full",
				ClientName:           "client-a",
			},
			{
				ID:                  peerID2.String(),
				Height:              200,
				InteractionFailures: 2,
				MaliciousCount:      1,
			},
		}, nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
		}

		resp, err := server.GetPeerRegistry(context.Background(), &emptypb.Empty{})
		require.NoError(t, err)
		require.Len(t, resp.Peers, 2)

		// Find peers in response by ID
		var peer1Info, peer2Info *p2p_api.PeerRegistryInfo
		for _, p := range resp.Peers {
			if p.Id == peerID1.String() {
				peer1Info = p
			}
			if p.Id == peerID2.String() {
				peer2Info = p
			}
		}

		require.NotNil(t, peer1Info)
		assert.Equal(t, uint32(100), peer1Info.Height)
		assert.Equal(t, blockHash.String(), peer1Info.BlockHash)
		assert.Equal(t, "http://peer1:8080", peer1Info.DataHubUrl)
		assert.Equal(t, int64(1), peer1Info.InteractionSuccesses)
		assert.Equal(t, "full", peer1Info.Storage)
		assert.Equal(t, "client-a", peer1Info.ClientName)

		require.NotNil(t, peer2Info)
		assert.Equal(t, uint32(200), peer2Info.Height)
		assert.Empty(t, peer2Info.BlockHash)
		assert.Equal(t, int64(2), peer2Info.InteractionFailures)
		assert.Equal(t, int64(1), peer2Info.MaliciousCount)
	})

	t.Run("empty central registry returns empty list", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("ListPeers").Return([]*blockchain.PeerInfo{}, nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
		}

		resp, err := server.GetPeerRegistry(context.Background(), &emptypb.Empty{})
		require.NoError(t, err)
		assert.Empty(t, resp.Peers)
	})
}

// --- GetPeer gRPC endpoint tests ---

func TestGetPeerGRPC(t *testing.T) {
	t.Run("nil central registry returns not found", func(t *testing.T) {
		server := &Server{
			logger: ulogger.New("test"),
		}

		req := &p2p_api.GetPeerRequest{PeerId: "some-peer"}
		resp, err := server.GetPeer(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, resp.Found)
	})

	t.Run("peer not in central registry returns not found", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", "nonexistent-peer").Return((*blockchain.PeerInfo)(nil), false, nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
		}

		req := &p2p_api.GetPeerRequest{PeerId: "nonexistent-peer"}
		resp, err := server.GetPeer(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, resp.Found)
	})

	t.Run("existing peer returns full info", func(t *testing.T) {
		_, pub, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
		require.NoError(t, err)
		testPeerID, err := peer.IDFromPublicKey(pub)
		require.NoError(t, err)

		blockHash, _ := chainhash.NewHashFromStr("000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f")

		reg := &mockPeerRegistryClient{}
		reg.On("GetPeer", testPeerID.String()).Return(&blockchain.PeerInfo{
			ID:                   testPeerID.String(),
			Height:               500,
			BlockHash:            blockHash,
			DataHubURL:           "http://test:8080",
			InteractionSuccesses: 1,
			Storage:              "pruned",
			ClientName:           "my-client",
		}, true, nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
		}

		req := &p2p_api.GetPeerRequest{PeerId: testPeerID.String()}
		resp, err := server.GetPeer(context.Background(), req)
		require.NoError(t, err)
		require.True(t, resp.Found)
		require.NotNil(t, resp.Peer)

		assert.Equal(t, testPeerID.String(), resp.Peer.Id)
		assert.Equal(t, uint32(500), resp.Peer.Height)
		assert.Equal(t, blockHash.String(), resp.Peer.BlockHash)
		assert.Equal(t, "http://test:8080", resp.Peer.DataHubUrl)
		assert.Equal(t, int64(1), resp.Peer.InteractionSuccesses)
		assert.Equal(t, "pruned", resp.Peer.Storage)
		assert.Equal(t, "my-client", resp.Peer.ClientName)
	})
}

// --- AddBanScore gRPC tests ---

func TestAddBanScoreGRPC(t *testing.T) {
	t.Run("maps known reason strings via central registry", func(t *testing.T) {
		reg := &mockPeerRegistryClient{}
		reg.On("AddBanScore", mock.Anything, mock.Anything, mock.Anything).Return(int32(10), false, nil)

		server := &Server{
			logger:          ulogger.New("test"),
			centralRegistry: reg,
			banChan:         make(chan BanEvent, 10),
		}

		reasons := []string{"invalid_subtree", "protocol_violation", "spam", "invalid_block", "unknown_reason"}
		for _, reason := range reasons {
			req := &p2p_api.AddBanScoreRequest{
				PeerId: "test-peer",
				Reason: reason,
			}
			resp, err := server.AddBanScore(context.Background(), req)
			require.NoError(t, err)
			assert.True(t, resp.Ok)
		}
	})
}
