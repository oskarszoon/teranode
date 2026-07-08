package validator

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/spend"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// reversalSpyStore records what Unspend received and can be told to fail N times.
type reversalSpyStore struct {
	utxo.Store // embed so only Unspend is overridden; nil methods will panic if called
	gotSpends  []*utxo.Spend
	gotCtxErr  error
	failFirst  int
	callCount  int
}

func (r *reversalSpyStore) Unspend(ctx context.Context, spends []*utxo.Spend, _ ...bool) error {
	r.callCount++
	r.gotCtxErr = ctx.Err()
	r.gotSpends = spends
	if r.callCount <= r.failFirst {
		return errors.NewStorageError("forced failure %d", r.callCount)
	}
	return nil
}

func newSpend(v uint32, withErr error) *utxo.Spend {
	h := chainhash.Hash{byte(v)}
	return &utxo.Spend{TxID: &h, Vout: v, UTXOHash: &h, SpendingData: &spend.SpendingData{TxID: &h, Vin: 0}, Err: withErr}
}

func TestReverseSpends_FiltersFailedSpends(t *testing.T) {
	initPrometheusMetrics()

	store := &reversalSpyStore{}
	v := &Validator{utxoStore: store, logger: ulogger.TestLogger{}}

	spends := []*utxo.Spend{newSpend(1, nil), newSpend(2, errors.ErrTxNotFound), newSpend(3, nil)}
	require.NoError(t, v.reverseSpends(context.Background(), spends))

	require.Len(t, store.gotSpends, 2, "only Err==nil spends must be passed to Unspend")
	for _, s := range store.gotSpends {
		require.NoError(t, s.Err)
	}
}

func TestReverseSpends_RunsUnderCancelledContext(t *testing.T) {
	initPrometheusMetrics()

	store := &reversalSpyStore{}
	v := &Validator{utxoStore: store, logger: ulogger.TestLogger{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller context already dead (tracing-off deadline path)

	require.NoError(t, v.reverseSpends(ctx, []*utxo.Spend{newSpend(1, nil)}))
	require.NoError(t, store.gotCtxErr, "reversal must use a fresh context, not the cancelled caller ctx")
	require.Equal(t, 1, store.callCount)
}

func TestReverseSpends_ExhaustionReturnsError(t *testing.T) {
	initPrometheusMetrics()

	store := &reversalSpyStore{failFirst: 99}
	v := &Validator{utxoStore: store, logger: ulogger.TestLogger{}}

	err := v.reverseSpends(context.Background(), []*utxo.Spend{newSpend(1, nil)})
	require.Error(t, err)
	require.Equal(t, 3, store.callCount, "3 bounded attempts")
}
