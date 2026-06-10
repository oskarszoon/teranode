package subtreevalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation/subtreevalidation_api"
	"github.com/bsv-blockchain/teranode/services/validator"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func TestBuildInBlockParentAccumulator(t *testing.T) {
	t.Run("empty list returns nil accumulator", func(t *testing.T) {
		acc, err := buildInBlockParentAccumulator(nil, 1730003)
		require.NoError(t, err)
		require.Nil(t, acc)

		acc, err = buildInBlockParentAccumulator([][]byte{}, 1730003)
		require.NoError(t, err)
		require.Nil(t, acc)
	})

	t.Run("valid hashes are seeded at the candidate block height", func(t *testing.T) {
		acc, err := buildInBlockParentAccumulator([][]byte{hash1[:], hash2[:]}, 1730003)
		require.NoError(t, err)
		require.NotNil(t, acc)

		m := acc.lookup(*hash1)
		require.NotNil(t, m)
		require.Equal(t, uint32(1730003), m.BlockHeight)

		m = acc.lookup(*hash2)
		require.NotNil(t, m)
		require.Equal(t, uint32(1730003), m.BlockHeight)

		require.Nil(t, acc.lookup(*hash3))
	})

	t.Run("duplicate hashes collapse to one entry", func(t *testing.T) {
		acc, err := buildInBlockParentAccumulator([][]byte{hash1[:], hash1[:]}, 42)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Len(t, acc.delta, 1)

		m := acc.lookup(*hash1)
		require.NotNil(t, m)
		require.Equal(t, uint32(42), m.BlockHeight)
	})

	t.Run("invalid hash length returns error", func(t *testing.T) {
		acc, err := buildInBlockParentAccumulator([][]byte{{0x01, 0x02}}, 42)
		require.Error(t, err)
		require.Nil(t, acc)
	})
}

// TestLegacySubtreeInBlockParentHint reproduces the legacy-sync wedge fixed by
// the in_block_parent_hashes request hint: a child transaction spends an
// output of a parent located in the same block but a different subtree, so the
// parent is neither in the child's subtree nor in the UTXO store with
// BlockHeights set. Without the hint the validator receives no ParentMetadata
// for the parent and falls back to the unconfirmedParentHeight sentinel (BDK:
// bad-txns-unconfirmed-input-in-block); with the hint it receives the
// candidate block height.
func TestLegacySubtreeInBlockParentHint(t *testing.T) {
	const blockHeight = uint32(1730003)

	// child spends output 0 of parentTx1; cloning tx1 and repointing its input
	// keeps the tx extended (PreviousTxSatoshis/Script survive the clone).
	parentHash := parentTx1.TxIDChainHash()

	childTx := tx1.Clone()
	require.NoError(t, childTx.Inputs[0].PreviousTxIDAdd(parentHash))
	childTx.Inputs[0].PreviousTxOutIndex = 0
	childHash := childTx.TxIDChainHash()

	setupServer := func(t *testing.T) (*Server, *recordingValidatorClient, *subtreepkg.Subtree) {
		utxoStore, _, txStore, subtreeStore, blockchainClient, deferFunc := setup(t)
		t.Cleanup(deferFunc)

		recordingClient := newRecordingValidatorClient(&validator.MockValidatorClient{UtxoStore: utxoStore})

		st, err := subtreepkg.NewTreeByLeafCount(1)
		require.NoError(t, err)
		require.NoError(t, st.AddNode(*childHash, 121, 0))

		nodeBytes, err := st.SerializeNodes()
		require.NoError(t, err)

		require.NoError(t, subtreeStore.Set(t.Context(), st.RootHash()[:], fileformat.FileTypeSubtreeToCheck, nodeBytes))
		require.NoError(t, subtreeStore.Set(t.Context(), st.RootHash()[:], fileformat.FileTypeSubtreeData, childTx.ExtendedBytes()))

		nilConsumer := &kafka.KafkaConsumerGroup{}
		tSettings := test.CreateBaseTestSettings(t)

		server, err := New(context.Background(), ulogger.TestLogger{}, tSettings, subtreeStore, txStore, utxoStore, recordingClient, blockchainClient, nilConsumer, nilConsumer, nil)
		require.NoError(t, err)

		return server, recordingClient, st
	}

	t.Run("hinted parent resolves at candidate block height", func(t *testing.T) {
		InitPrometheusMetrics()

		server, recordingClient, st := setupServer(t)

		// seed exactly as the legacy branch of checkSubtreeFromBlock does from
		// request.InBlockParentHashes
		acc, err := buildInBlockParentAccumulator([][]byte{parentHash[:]}, blockHeight)
		require.NoError(t, err)
		require.NotNil(t, acc)

		v := ValidateSubtree{
			SubtreeHash:   *st.RootHash(),
			BaseURL:       "legacy",
			TxHashes:      []chainhash.Hash{*childHash},
			AllowFailFast: false,
		}

		_, err = server.validateSubtreeInternalImpl(context.Background(), v, blockHeight, nil, acc)
		require.NoError(t, err)

		recorded := recordingClient.recordedOptions(*childHash)
		require.NotEmpty(t, recorded, "child transaction was not validated")

		for _, opts := range recorded {
			require.NotNil(t, opts.ParentMetadata, "child validation options carry no ParentMetadata — parent would resolve to the unconfirmedParentHeight sentinel and BDK would reject with bad-txns-unconfirmed-input-in-block")

			parentMeta, found := opts.ParentMetadata[*parentHash]
			require.True(t, found, "in-block parent missing from ParentMetadata")
			require.Equal(t, blockHeight, parentMeta.BlockHeight)
		}
	})

	t.Run("no hint preserves prior behaviour: no ParentMetadata", func(t *testing.T) {
		InitPrometheusMetrics()

		server, recordingClient, st := setupServer(t)

		acc, err := buildInBlockParentAccumulator(nil, blockHeight)
		require.NoError(t, err)
		require.Nil(t, acc)

		v := ValidateSubtree{
			SubtreeHash:   *st.RootHash(),
			BaseURL:       "legacy",
			TxHashes:      []chainhash.Hash{*childHash},
			AllowFailFast: false,
		}

		_, err = server.validateSubtreeInternalImpl(context.Background(), v, blockHeight, nil, acc)
		require.NoError(t, err)

		recorded := recordingClient.recordedOptions(*childHash)
		require.NotEmpty(t, recorded, "child transaction was not validated")

		for _, opts := range recorded {
			require.Nil(t, opts.ParentMetadata, "nil accumulator must not produce ParentMetadata")
		}
	})
}

// TestCheckSubtreeFromBlockLegacyParentHints exercises the full gRPC handler
// path for the legacy branch: the request's in_block_parent_hashes must reach
// the per-tx validation Options as ParentMetadata at the request's block
// height, and a malformed parent hash must be rejected as an invalid argument.
func TestCheckSubtreeFromBlockLegacyParentHints(t *testing.T) {
	// regtest CSVHeight is 576; stay below it so the handler skips the
	// candidate-parent MTP fetch, which would require real headers
	const blockHeight = uint32(100)

	parentHash := parentTx1.TxIDChainHash()

	childTx := tx1.Clone()
	require.NoError(t, childTx.Inputs[0].PreviousTxIDAdd(parentHash))
	childTx.Inputs[0].PreviousTxOutIndex = 0
	childHash := childTx.TxIDChainHash()

	setupServer := func(t *testing.T) (*Server, *recordingValidatorClient, *subtreepkg.Subtree) {
		utxoStore, _, txStore, subtreeStore, blockchainClient, deferFunc := setup(t)
		t.Cleanup(deferFunc)

		recordingClient := newRecordingValidatorClient(&validator.MockValidatorClient{UtxoStore: utxoStore})

		st, err := subtreepkg.NewTreeByLeafCount(1)
		require.NoError(t, err)
		require.NoError(t, st.AddNode(*childHash, 121, 0))

		// the legacy handler branch reads the FULL subtree serialization
		// (NewSubtreeFromReader), not just the nodes
		subtreeBytes, err := st.Serialize()
		require.NoError(t, err)

		require.NoError(t, subtreeStore.Set(t.Context(), st.RootHash()[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes))
		require.NoError(t, subtreeStore.Set(t.Context(), st.RootHash()[:], fileformat.FileTypeSubtreeData, childTx.ExtendedBytes()))

		nilConsumer := &kafka.KafkaConsumerGroup{}
		tSettings := test.CreateBaseTestSettings(t)

		server, err := New(context.Background(), ulogger.TestLogger{}, tSettings, subtreeStore, txStore, utxoStore, recordingClient, blockchainClient, nilConsumer, nilConsumer, nil)
		require.NoError(t, err)

		return server, recordingClient, st
	}

	t.Run("request hint reaches per-tx validation options", func(t *testing.T) {
		InitPrometheusMetrics()

		server, recordingClient, st := setupServer(t)

		request := &subtreevalidation_api.CheckSubtreeFromBlockRequest{
			Hash:                st.RootHash()[:],
			BaseUrl:             "legacy",
			BlockHeight:         blockHeight,
			BlockHash:           make([]byte, 32),
			PreviousBlockHash:   make([]byte, 32),
			InBlockParentHashes: [][]byte{parentHash[:]},
		}

		response, err := server.CheckSubtreeFromBlock(context.Background(), request)
		require.NoError(t, err)
		require.True(t, response.Blessed)

		recorded := recordingClient.recordedOptions(*childHash)
		require.NotEmpty(t, recorded, "child transaction was not validated")

		for _, opts := range recorded {
			require.NotNil(t, opts.ParentMetadata)

			parentMeta, found := opts.ParentMetadata[*parentHash]
			require.True(t, found, "in-block parent missing from ParentMetadata")
			require.Equal(t, blockHeight, parentMeta.BlockHeight)
		}
	})

	t.Run("malformed parent hash is rejected as invalid argument", func(t *testing.T) {
		InitPrometheusMetrics()

		server, _, st := setupServer(t)

		request := &subtreevalidation_api.CheckSubtreeFromBlockRequest{
			Hash:                st.RootHash()[:],
			BaseUrl:             "legacy",
			BlockHeight:         blockHeight,
			BlockHash:           make([]byte, 32),
			PreviousBlockHash:   make([]byte, 32),
			InBlockParentHashes: [][]byte{{0x01, 0x02}},
		}

		_, err := server.CheckSubtreeFromBlock(context.Background(), request)
		require.Error(t, err)
		require.Contains(t, err.Error(), "in-block parent hash")
	})
}
