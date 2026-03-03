package aerospike

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/aerospike/aerospike-client-go/v8"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/uaerospike"
	aeroTest "github.com/bsv-blockchain/testcontainers-aerospike-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupStoreForVerifyTest(t *testing.T) (*Store, *uaerospike.Client) {
	t.Helper()
	logger := ulogger.NewErrorTestLogger(t)
	ctx := context.Background()
	tSettings := test.CreateBaseTestSettings(t)

	container, err := aeroTest.RunContainer(ctx)
	if err != nil {
		t.Skipf("Skipping: Aerospike container not available (%v)", err)
	}

	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.ServicePort(ctx)
	require.NoError(t, err)

	client, err := uaerospike.NewClient(host, port)
	require.NoError(t, err)

	t.Cleanup(func() {
		client.Close()
	})

	aeroURL, err := url.Parse(fmt.Sprintf("aerospike://%s:%d/test?set=utxo&externalStore=file://./data/external&block_retention=100", host, port))
	require.NoError(t, err)

	store, err := New(ctx, logger, tSettings, aeroURL)
	require.NoError(t, err)

	return store, client
}

func TestVerifyAllChildrenSpent(t *testing.T) {
	store, client := setupStoreForVerifyTest(t)

	txID := chainhash.HashH([]byte("test-verify-children"))

	t.Run("childCount zero returns true", func(t *testing.T) {
		allSpent, err := store.verifyAllChildrenSpent(context.Background(), &txID, 0)
		require.NoError(t, err)
		assert.True(t, allSpent)
	})

	t.Run("context cancelled returns error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		allSpent, err := store.verifyAllChildrenSpent(ctx, &txID, 2)
		require.Error(t, err)
		assert.False(t, allSpent)
	})

	t.Run("all children spent returns true", func(t *testing.T) {
		// Create child records with spentUtxos == recordUtxos
		for i := uint32(1); i <= 2; i++ {
			keySource := uaerospike.CalculateKeySourceInternal(&txID, i)
			key, aErr := aerospike.NewKey(store.namespace, store.setName, keySource)
			require.NoError(t, aErr)

			aErr = client.Put(nil, key, aerospike.BinMap{
				fields.SpentUtxos.String():  5,
				fields.RecordUtxos.String(): 5,
			})
			require.NoError(t, aErr)
		}

		allSpent, err := store.verifyAllChildrenSpent(context.Background(), &txID, 2)
		require.NoError(t, err)
		assert.True(t, allSpent)
	})

	t.Run("not all children spent returns false", func(t *testing.T) {
		// Child 1: fully spent. Child 2: not fully spent.
		for i := uint32(1); i <= 2; i++ {
			keySource := uaerospike.CalculateKeySourceInternal(&txID, i)
			key, aErr := aerospike.NewKey(store.namespace, store.setName, keySource)
			require.NoError(t, aErr)

			spent := 5
			if i == 2 {
				spent = 3 // not fully spent
			}
			aErr = client.Put(nil, key, aerospike.BinMap{
				fields.SpentUtxos.String():  spent,
				fields.RecordUtxos.String(): 5,
			})
			require.NoError(t, aErr)
		}

		allSpent, err := store.verifyAllChildrenSpent(context.Background(), &txID, 2)
		require.NoError(t, err)
		assert.False(t, allSpent)
	})

	t.Run("invalid spentUtxos type returns error", func(t *testing.T) {
		keySource := uaerospike.CalculateKeySourceInternal(&txID, 1)
		key, aErr := aerospike.NewKey(store.namespace, store.setName, keySource)
		require.NoError(t, aErr)

		// Write a string instead of int for spentUtxos
		aErr = client.Put(nil, key, aerospike.BinMap{
			fields.SpentUtxos.String():  "not-an-int",
			fields.RecordUtxos.String(): 5,
		})
		require.NoError(t, aErr)

		allSpent, verifyErr := store.verifyAllChildrenSpent(context.Background(), &txID, 1)
		require.Error(t, verifyErr)
		assert.False(t, allSpent)
		assert.Contains(t, verifyErr.Error(), "invalid type for spentUtxos")
	})

	t.Run("invalid recordUtxos type returns error", func(t *testing.T) {
		keySource := uaerospike.CalculateKeySourceInternal(&txID, 1)
		key, aErr := aerospike.NewKey(store.namespace, store.setName, keySource)
		require.NoError(t, aErr)

		// Write valid spentUtxos but string for recordUtxos
		aErr = client.Put(nil, key, aerospike.BinMap{
			fields.SpentUtxos.String():  5,
			fields.RecordUtxos.String(): "not-an-int",
		})
		require.NoError(t, aErr)

		allSpent, verifyErr := store.verifyAllChildrenSpent(context.Background(), &txID, 1)
		require.Error(t, verifyErr)
		assert.False(t, allSpent)
		assert.Contains(t, verifyErr.Error(), "invalid type for recordUtxos")
	})

	t.Run("missing record returns error", func(t *testing.T) {
		missingTxID := chainhash.HashH([]byte("does-not-exist"))

		allSpent, err := store.verifyAllChildrenSpent(context.Background(), &missingTxID, 1)
		require.Error(t, err)
		assert.False(t, allSpent)
		assert.Contains(t, err.Error(), "child 1 read failed")
	})
}
