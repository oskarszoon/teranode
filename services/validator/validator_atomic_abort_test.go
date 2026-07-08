package validator

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/meta"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/test/utils/transactions"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/require"
)

// TestValidate_AbortReversesPartialSpends reproduces the eu-1 dangling-spender
// mechanism (#1214): a child tx spends two parents, A (spendable) and B
// (locked). The sql store's batched Spend() applies A's spend successfully
// and fails B's with ErrTxLocked in the same call — and, because
// needsSpendRollback only covers ErrSpent/ErrTxConflicting/ErrFrozen/
// ErrUtxoHashMismatch (stores/utxo/sql/sql.go), the store does NOT roll back
// A's already-applied spend on an ErrTxLocked failure. validateInternal must
// reverse that partial spend at its fall-through abort before returning the
// error, leaving A unspent and never creating a record for the child.
func TestValidate_AbortReversesPartialSpends(t *testing.T) {
	tracing.SetupMockTracer()

	ctx := context.Background()
	logger := ulogger.NewErrorTestLogger(t)

	tSettings := test.CreateBaseTestSettings(t)
	tSettings.BlockAssembly.Disabled = true
	// TX_LOCKED is retried by ValidateWithOptions by default (parent/child race
	// mitigation). Parent B stays locked for the lifetime of this test, so
	// disable the retry to keep the repro deterministic and fast — this is the
	// documented test-settings recommendation (validator_txlocked_maxRetries).
	tSettings.Validator.TxLockedMaxRetries = 0

	utxoStoreURL, err := url.Parse("sqlitememory:///validator_atomic_abort_test")
	require.NoError(t, err)

	utxoStore, err := sql.New(ctx, logger, tSettings, utxoStoreURL)
	require.NoError(t, err)

	require.NoError(t, utxoStore.SetBlockHeight(100))
	require.NoError(t, utxoStore.SetMedianBlockTime(uint32(time.Now().Unix()))) //nolint:gosec

	initPrometheusMetrics()

	v := &Validator{
		logger:      logger,
		settings:    tSettings,
		txValidator: NewTxValidator(logger, tSettings),
		utxoStore:   utxoStore,
		stats:       gocore.NewStat("validator"),
	}

	// A single key pays both parents. transactions.Create signs by calling
	// go-bt's FillAllInputs once per distinct private key in the tx, and
	// FillAllInputs (re-)signs EVERY input on each call — so a child spending
	// two parents under two DIFFERENT keys would have its first input's
	// signature clobbered by the second call and fail OP_EQUALVERIFY before
	// ever reaching spendUtxos. One key for both parents avoids that helper
	// limitation and lets the repro actually reach the UTXO-store spend batch.
	privKey, pubKey := bec.PrivateKeyFromBytes([]byte("ATOMIC_ABORT_TEST_SHARED_KEY!!!!"))

	// Two independent 1-output funding ("coinbase-shaped") txs, mirroring the
	// pattern in TestValidateTransactionBatch_DuplicateOutpointCreatesConflicting:
	// created directly in the store (not via Validate), and mined so
	// consensus-mode validation doesn't reject the child on
	// "bad-txns-unconfirmed-input-in-block".
	parentA := transactions.Create(t,
		transactions.WithCoinbaseData(1, "/atomic abort test parent A/"),
		transactions.WithP2PKHOutputs(1, 100_000, pubKey),
	)
	parentB := transactions.Create(t,
		transactions.WithCoinbaseData(1, "/atomic abort test parent B/"),
		transactions.WithP2PKHOutputs(1, 100_000, pubKey),
	)

	_, err = utxoStore.Create(ctx, parentA, 1, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 1, BlockHeight: 1, SubtreeIdx: 0}))
	require.NoError(t, err)
	_, err = utxoStore.Create(ctx, parentB, 1, utxo.WithMinedBlockInfo(utxo.MinedBlockInfo{BlockID: 1, BlockHeight: 1, SubtreeIdx: 0}))
	require.NoError(t, err)

	// Force ErrTxLocked on parent B's spend.
	require.NoError(t, utxoStore.SetLocked(ctx, []chainhash.Hash{*parentB.TxIDChainHash()}, true))

	child := transactions.Create(t,
		transactions.WithPrivateKey(privKey),
		transactions.WithInput(parentA, 0, privKey),
		transactions.WithInput(parentB, 0, privKey),
		transactions.WithP2PKHOutputs(1, 190_000, pubKey),
	)

	_, err = v.Validate(ctx, child, 100, WithSkipPolicyChecks(true))
	require.Error(t, err) // aborts (ErrTxLocked / processing)

	// The load-bearing assertion: parent A's output must NOT be left spent.
	utxoHashA, err := util.UTXOHashFromOutput(parentA.TxIDChainHash(), parentA.Outputs[0], 0)
	require.NoError(t, err)

	sp, getErr := utxoStore.GetSpend(ctx, &utxo.Spend{
		TxID:     parentA.TxIDChainHash(),
		Vout:     0,
		UTXOHash: utxoHashA,
	})
	require.NoError(t, getErr)
	require.Nil(t, sp.SpendingData, "parent A output must be unspent after abort (no dangling ref)")

	// And the child never got a record.
	var childMeta meta.Data
	require.Error(t, utxoStore.GetMeta(ctx, child.TxIDChainHash(), &childMeta))
}
