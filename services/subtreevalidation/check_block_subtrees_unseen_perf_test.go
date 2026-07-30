//go:build aerospike

// Reproduction harness for issue #1379: block validation collapses to ~30-50
// tx/s on blocks whose transactions were never seen via propagation.
//
// The discriminator is the pre-check in processTransactionsInLevels
// (check_block_subtrees.go:1009-1028): when every tx in the block is already in
// the txMeta cache or UTXO store, missed == 0 and the function returns
// immediately. This harness deliberately establishes the opposite state — only
// the PARENTS are seeded into the UTXO store, the block's own txs are absent
// from both cache and store — so every tx goes through blessMissingTransaction.
//
// Nothing is mocked below the server: real Aerospike (testcontainer, with the
// Lua UDFs registered by aerospike.New), real validator.Validator, real
// TxValidator, real GoBDK. The mainnet nodes run useLocalValidator = true
// (settings.conf:1255), so an in-process validator is topology-faithful rather
// than an approximation.
//
// Two boundaries are deliberate and must be remembered when reading numbers
// out of this harness:
//
//   - The block-assembly gRPC hop is NOT measured. blockAssemblyStub records
//     calls and returns immediately. What IS measured on that axis is the part
//     that touches the store: Create-with-locked plus the SetLocked two-phase
//     commit unlock (Validator.go:940-1057), which is a real Aerospike round
//     trip per tx.
//   - The container namespace is in-memory single-node; mainnet is persistent.
//     A SLOW result here is therefore strong evidence and a FAST one is weak.
package subtreevalidation

import (
	"context"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/go-chaincfg"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/services/blockassembly"
	"github.com/bsv-blockchain/teranode/services/blockchain"
	"github.com/bsv-blockchain/teranode/services/blockchain/blockchain_api"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation/subtreevalidation_api"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/settings"
	blobmemory "github.com/bsv-blockchain/teranode/stores/blob/memory"
	aerospikestore "github.com/bsv-blockchain/teranode/stores/utxo/aerospike"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	aerospiketest "github.com/bsv-blockchain/teranode/test/utils/aerospike"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

// perfHarnessOptions selects the consensus context a run executes in. It exists
// because the two kinds of fixture need different eras.
//
// Generated fixtures (the throughput runs) sign through go-bt's unlocker, which
// uses SIGHASH_FORKID. That is only valid at or above the UAHF fork, so they
// cannot run at a pre-2018 mainnet height — GoBDK computes the pre-fork sighash
// and the signature fails with "false/empty top stack element". Regtest has
// UahfForkHeight: 0 ("UAHF is always enabled on regtest") and CSVHeight 576, so
// regtest at height 100 gives valid FORKID signatures AND stays below CSV, which
// keeps the candidate-parent MTP fetch and the validator's EnsureMTPLoaded
// no-ops (Validator.go:1654) so the harness needs no real headers. This is the
// same height/params pairing the other tests in this package use.
//
// The smoke test instead replays real 2013-era mainnet transactions, whose
// signatures are pre-fork, so it needs mainnet params at a pre-fork height.
//
// Chain params change which consensus rules apply, not how many store round
// trips a tx costs, so this choice does not affect the throughput being
// measured.
type perfHarnessOptions struct {
	chainParams *chaincfg.Params
	blockHeight uint32
	tune        func(*settings.Settings)

	// aerospikeURLParams are appended to the testcontainer's Aerospike URL. The
	// InitAerospikeContainer helper builds a bare URL with no connection tuning, so
	// without this the harness silently runs on the client's default pool while
	// production runs a hard cap — see productionAerospikeParams.
	aerospikeURLParams string
}

// productionAerospikeParams are the connection settings mainnet actually runs
// (utxostore.docker.m, settings.conf:1271).
//
// ConnectionQueueSize=128 with LimitConnectionsToQueueSize=true is a HARD ceiling:
// the client will not open a 129th connection, and callers block waiting for one to
// free. Meanwhile processTransactionsInLevels fans out to SpendBatcherSize*2 = 2048
// concurrent validations (check_block_subtrees.go:1156), each of which issues
// several store operations. That is a ~16x oversubscription of a blocking,
// hard-capped pool, and the fan-out width is derived from a batcher setting with no
// relationship to the pool size.
const productionAerospikeParams = "WarmUp=0&ConnectionQueueSize=128&LimitConnectionsToQueueSize=true&MinConnectionsPerNode=16&IdleTimeout=60s"

// defaultPerfOptions is the context for generated fixtures: regtest at height
// 100, post-UAHF and pre-CSV, with mainnet's Aerospike connection caps applied so
// the harness does not flatter itself with an unbounded pool.

func defaultPerfOptions() perfHarnessOptions {
	return perfHarnessOptions{blockHeight: 100, aerospikeURLParams: productionAerospikeParams}
}

// unlimitedPoolPerfOptions is defaultPerfOptions with no connection cap, for A/B
// against the production caps.
func unlimitedPoolPerfOptions() perfHarnessOptions {
	return perfHarnessOptions{blockHeight: 100}
}

// blockAssemblyStub implements the blockassembly client surface the validator
// uses, with an atomic counter instead of a testify mock. A testify mock takes a
// mutex and runs reflection on every call; at the 2048-wide per-level fan-out
// (SpendBatcherSize * 2, check_block_subtrees.go:1156) that contention would
// show up in the profile as a bottleneck the production path does not have.
type blockAssemblyStub struct {
	blockassembly.ClientI
	stores atomic.Uint64
}

func (s *blockAssemblyStub) Store(_ context.Context, _ *chainhash.Hash, _, _ uint64, _ subtreepkg.TxInpoints) (bool, error) {
	s.stores.Add(1)
	return true, nil
}

// perfBlockchainClient overrides only the blockchain methods this path is
// expected to call. The embedded nil ClientI is intentional: any method that
// turns out to be in the hot path and is not overridden here will nil-panic and
// name itself, rather than being silently absorbed by a permissive mock.
type perfBlockchainClient struct {
	blockchain.ClientI
	state          blockchain.FSMStateType
	blockHeaderIDs []uint32
	height         uint32
	fsmCalls       atomic.Uint64
	headerIDsCalls atomic.Uint64
}

func (c *perfBlockchainClient) GetFSMCurrentState(_ context.Context) (*blockchain.FSMStateType, error) {
	c.fsmCalls.Add(1)
	state := c.state
	return &state, nil
}

func (c *perfBlockchainClient) GetBlockHeaderIDs(_ context.Context, _ *chainhash.Hash, _ uint64) ([]uint32, error) {
	c.headerIDsCalls.Add(1)
	return c.blockHeaderIDs, nil
}

// Subscribe feeds Server.blockchainSubscriptionListener, a background goroutine
// started by New (Server.go:309). It is handed a channel that never receives, so
// the listener parks on it for the life of the test and never competes with the
// measurement.
func (c *perfBlockchainClient) Subscribe(_ context.Context, _ string) (chan *blockchain_api.Notification, error) {
	return make(chan *blockchain_api.Notification), nil
}

// GetBestBlockHeader is called once from Server.updateBestBlock during New
// (Server.go:387), not per tx, so a static header costs the measurement nothing.
// The reported height must match the fixture height: updateBestBlock pushes it
// into subtreeStore.SetCurrentBlockHeight, and a mismatch would put the DAH
// arithmetic in CheckBlockSubtrees on a different height than the fixture.
func (c *perfBlockchainClient) GetBestBlockHeader(_ context.Context) (*model.BlockHeader, *model.BlockHeaderMeta, error) {
	return &model.BlockHeader{
		Version:        1,
		HashPrevBlock:  &chainhash.Hash{},
		HashMerkleRoot: &chainhash.Hash{},
		Timestamp:      1,
		Bits:           model.NBit{},
		Nonce:          0,
	}, &model.BlockHeaderMeta{ID: 1, Height: c.height}, nil
}

// perfHarness bundles the wired-up server and the collaborators a test needs to
// seed state and read counters back.
type perfHarness struct {
	server          *Server
	utxoStore       *aerospikestore.Store
	subtreeStore    *blobmemory.Memory
	blockAssembly   *blockAssemblyStub
	blockchain      *perfBlockchainClient
	settings        *settings.Settings
	logger          ulogger.Logger
	txMetaPublished *atomic.Uint64
	blockHeight     uint32
}

// newPerfHarness stands up the harness with a quiet logger. Tests that need the
// per-level breakdown use newPerfHarnessWithLogger instead.
func newPerfHarness(t *testing.T, opts perfHarnessOptions) *perfHarness {
	t.Helper()

	return newPerfHarnessWithLogger(t, ulogger.TestLogger{}, opts)
}

// newPerfHarnessWithLogger stands up real Aerospike + real validator +
// subtreevalidation server. opts.tune runs after the base settings are built so a
// test can flip a single bisect axis without forking the whole setup.
func newPerfHarnessWithLogger(t *testing.T, logger ulogger.Logger, opts perfHarnessOptions) *perfHarness {
	t.Helper()

	require.Positive(t, opts.blockHeight, "blockHeight must be set")

	InitPrometheusMetrics()

	ctx := context.Background()

	aeroURL, cleanup, err := aerospiketest.InitAerospikeContainer()
	require.NoError(t, err, "aerospike testcontainer must start — this harness has no in-memory fallback by design")
	t.Cleanup(func() {
		if cleanup != nil {
			_ = cleanup()
		}
	})

	tSettings := test.CreateBaseTestSettings(t)

	// CreateBaseTestSettings defaults to regtest, which is what generated fixtures
	// need (see perfHarnessOptions). Only the smoke test overrides it.
	if opts.chainParams != nil {
		tSettings.ChainCfgParams = opts.chainParams
	}

	// CreateBaseTestSettings installs a test-only Aerospike write policy
	// (util/test/helpers.go:38-41: MaxRetries=30, SleepBetweenRetries=50ms,
	// SleepMultiplier=2, TotalTimeout=30s) to paper over hot-key errors in
	// ordinary tests. Left in place it would add up to ~25s of exponential backoff
	// to a single contended write and this harness would report production code as
	// slow when the cost was a test setting. Restore the production policy from
	// settings.conf (:172-173) so the numbers mean something.
	prodSettings := settings.NewSettings()
	tSettings.Aerospike.WritePolicyURL = prodSettings.Aerospike.WritePolicyURL
	tSettings.Aerospike.ReadPolicyURL = prodSettings.Aerospike.ReadPolicyURL

	// Block assembly ENABLED is the baseline: at the tip the FSM is RUNNING, so
	// addTXToBlockAssembly is true (check_block_subtrees.go:504) and every unseen
	// tx pays Create-with-locked plus the SetLocked 2PC unlock. That is the state
	// the slow mainnet blocks were actually in, and cached txs skip all of it —
	// which is exactly the fast/slow discriminator.
	tSettings.BlockAssembly.Disabled = false

	if opts.tune != nil {
		opts.tune(tSettings)
	}

	if opts.aerospikeURLParams != "" {
		sep := "&"
		if !strings.Contains(aeroURL, "?") {
			sep = "?"
		}

		aeroURL += sep + opts.aerospikeURLParams
	}

	parsedURL, err := url.Parse(aeroURL)
	require.NoError(t, err)
	t.Logf("aerospike URL: %s", parsedURL.String())

	utxoStore, err := aerospikestore.New(ctx, logger, tSettings, parsedURL)
	require.NoError(t, err, "aerospike store must construct — this also proves Lua UDF registration succeeded")

	require.NoError(t, utxoStore.SetBlockHeight(opts.blockHeight))

	subtreeStore := blobmemory.New()
	txStore := blobmemory.New()

	blockchainClient := &perfBlockchainClient{
		state:          blockchain.FSMStateRUNNING,
		blockHeaderIDs: []uint32{1, 2, 3},
		height:         opts.blockHeight,
	}

	baStub := &blockAssemblyStub{}

	// KafkaAsyncProducerMock.Publish is a blocking send into a 100-deep buffer
	// (util/kafka/kafka_producer_async_mock.go:19,47) that nothing drains. The
	// validator publishes one txmeta per validated tx
	// (Validator.go:1005-1016), so past 100 txs every remaining
	// blessMissingTransaction goroutine parks in Publish forever. Draining keeps
	// the real per-tx enqueue cost in the measurement without the deadlock.
	//
	// Boundary, same class as the block-assembly hop: the Kafka broker round trip
	// itself is not measured, only the enqueue.
	txMetaProducer := kafka.NewKafkaAsyncProducerMock()
	rejectedProducer := kafka.NewKafkaAsyncProducerMock()

	drained := drainProducer(t, txMetaProducer)
	drainProducer(t, rejectedProducer)

	realValidator, err := validator.New(ctx, logger, tSettings, utxoStore,
		txMetaProducer, rejectedProducer, nil,
		baStub, blockchainClient)
	require.NoError(t, err)

	nilConsumer := &kafka.KafkaConsumerGroup{}

	server, err := New(ctx, logger, tSettings, subtreeStore, txStore, utxoStore,
		realValidator, blockchainClient, nilConsumer, nilConsumer, nil, nil)
	require.NoError(t, err)

	return &perfHarness{
		server:          server,
		utxoStore:       utxoStore,
		subtreeStore:    subtreeStore,
		blockAssembly:   baStub,
		blockchain:      blockchainClient,
		settings:        tSettings,
		logger:          logger,
		txMetaPublished: drained,
		blockHeight:     opts.blockHeight,
	}
}

// drainProducer consumes a mock producer's publish channel for the life of the
// test and counts the messages, so Publish never blocks. Returns the counter.
func drainProducer(t *testing.T, producer *kafka.KafkaAsyncProducerMock) *atomic.Uint64 {
	t.Helper()

	counter := &atomic.Uint64{}
	done := make(chan struct{})
	ch := producer.PublishChannel()

	go func() {
		for {
			select {
			case <-ch:
				counter.Add(1)
			case <-done:
				return
			}
		}
	}()

	t.Cleanup(func() { close(done) })

	return counter
}

// TestUnseenTxThroughput_Smoke is phase 1: prove every real component connects
// and, critically, that the RUNNING FSM pin actually took effect. A harness that
// silently ran with addTXToBlockAssembly == false would measure the catchup path
// instead of the tip path and every later number would be wrong.
func TestUnseenTxThroughput_Smoke(t *testing.T) {
	// Mainnet params at a pre-fork height: the fixtures below are real 2013-era
	// mainnet transactions whose signatures predate SIGHASH_FORKID, so they only
	// verify under the pre-UAHF sighash algorithm. 257727 is also below mainnet
	// CSVHeight (419328), keeping the MTP paths no-ops.
	mainnetParams := chaincfg.MainNetParams

	h := newPerfHarness(t, perfHarnessOptions{
		chainParams: &mainnetParams,
		blockHeight: 257727,
	})

	ctx := context.Background()

	// Same real-mainnet parent/child pair the validator-level consensus tests and
	// legacy_real_validator_integration_test.go use: the child spends parent
	// output 1 with a valid signature, so GoBDK script validation genuinely runs.
	childTx, err := bt.NewTxFromString("010000000000000000ef01febe0cbd7d87d44cbd4b5adac0a5bfcdbd2b672c9113f5d74a6459a2b85569db010000008b48304502207ec38d0a4ef79c3a4286ba3e5a5b6ede1fa678af9242465140d78a901af9e4e0022100c26c377d44b761469cf0bdcdbf4931418f2c5a02ce6b72bbb7af52facd7228c1014104bc9eb4fe4cb53e35df7e7734c4c3cd91c6af7840be80f4a1fff283e2cd6ae8f7713cb263a4590263240e3c01ec36bc603c32281ac08773484dc69b8152e48cecffffffff60b74700000000001976a9148ac9bdc626352d16e18c26f431e834f9aae30e2888ac0230424700000000001976a9148ac9bdc626352d16e18c26f431e834f9aae30e2888ac1027000000000000166a148ac9bdc626352d16e18c26f431e834f9aae30e2800000000")
	require.NoError(t, err)

	parentTx, err := bt.NewTxFromString("010000000000000000ef01154d5d31268f7ea94c80a7bf6de54e47812712feec25c17b8feceb570dfd9daf000000008b4830450220612b3ec065ec2b2a1757d97b7f57fba3c363645355cf6e1a5a1834411e6ab425022100bd071b90d391eb75dc9e2eea8b6774f36bf9c55439a971f0d1f4470b6448aef601410426e4e0654f72721b97a03c8170417c9ddabadcef97fe8ea626176ea62665b55ca2ff485f84df12ddec171e01ee8f9c7472c6c8467b0cf74ae8b3b614ed16cbdbffffffff008a6600000000001976a91429be45311cc66a5a6cc4a42516dbb7c9b126a3c188ac0280841e00000000001976a914996ed5e55d68aef653c85339f83873fac1321f0788ac60b74700000000001976a9148ac9bdc626352d16e18c26f431e834f9aae30e2888ac00000000")
	require.NoError(t, err)

	// Seed ONLY the parent. The child stays absent from cache and store — that is
	// the unseen precondition the whole reproduction depends on.
	_, err = h.utxoStore.Create(ctx, parentTx, h.blockHeight-1)
	require.NoError(t, err)

	blockBytes, _ := buildUnseenBlockFromTxs(t, h, []*bt.Tx{childTx}, 1, false)

	response, err := h.server.CheckBlockSubtrees(ctx, &subtreevalidation_api.CheckBlockSubtreesRequest{
		Block:   blockBytes,
		BaseUrl: "legacy",
	})
	require.NoError(t, err)
	require.True(t, response.Blessed)

	// The child must exist and be spendable: Create marked it locked because it
	// went to block assembly, then twoPhaseCommitTransaction unlocked it. Locked
	// still true here would mean the 2PC never ran.
	childMeta, err := h.utxoStore.Get(ctx, childTx.TxIDChainHash(), fields.Locked, fields.BlockIDs)
	require.NoError(t, err)
	require.False(t, childMeta.Locked, "child must be unlocked — a still-locked record means the SetLocked 2PC never ran, so the harness is measuring the wrong path")

	// The RUNNING pin must have produced a real block-assembly insert. Zero here
	// means addTXToBlockAssembly was false and this harness is measuring the
	// catchup path, not the tip path.
	require.Equal(t, uint64(1), h.blockAssembly.stores.Load(),
		"block assembly must have received the tx — zero means the RUNNING FSM pin did not take effect")
}
