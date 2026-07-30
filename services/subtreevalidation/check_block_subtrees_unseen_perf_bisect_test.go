//go:build aerospike

// Bisect matrix for the issue #1379 reproduction. Phase 5 of
// plans/1379-unseen-tx-throughput-plan.md.
//
// The shape-matched baseline (TestUnseenTxThroughput) came out at ~5,100 tx/s
// against mainnet's observed 27-50 tx/s, so it does NOT reproduce the collapse.
// These runs flip one axis at a time off that baseline to find which dimension,
// if any, moves the number toward mainnet — and to nail down the fixed per-level
// cost the baseline exposed.
package subtreevalidation

import (
	"fmt"
	"testing"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/stretchr/testify/require"
)

// bisectShape is a smaller version of the mainnet 959979 histogram: same 25-level
// depth and same wide-head/thin-tail character, ~1/4 the transactions. The
// baseline showed runtime is dominated by a fixed per-level cost rather than a
// per-tx one, so level depth is what has to be preserved; shrinking the widths
// keeps each axis cheap enough to sweep.
func bisectShape() []int {
	head := []int{734, 98, 65, 57, 48, 46}

	sizes := append([]int(nil), head...)
	for i := 0; i < 19; i++ {
		sizes = append(sizes, 27)
	}

	return sizes
}

// runBisectAxis executes one axis and returns tx/s.
func runBisectAxis(t *testing.T, label string, opts perfHarnessOptions, cfg unseenFixtureConfig) float64 {
	t.Helper()

	h, capture := newCapturingPerfHarness(t, opts)

	fx := generateUnseenFixture(t, h, cfg)
	assertUnseenPrecondition(t, h, fx)

	elapsed := runUnseenBlock(t, h, capture, fx, label)

	return float64(fx.txCount) / elapsed.Seconds()
}

func baseBisectConfig() unseenFixtureConfig {
	return unseenFixtureConfig{
		levelSizes:    bisectShape(),
		txsPerSubtree: 1024,
	}
}

// TestUnseenTxBisect_Baseline is the reference point every other axis is read
// against, at the reduced bisect scale.
func TestUnseenTxBisect_Baseline(t *testing.T) {
	rate := runBisectAxis(t, "bisect-baseline", defaultPerfOptions(), baseBisectConfig())
	t.Logf("AXIS baseline: %.1f tx/s", rate)
}

// TestUnseenTxBisect_BatcherTimers tests the explanation the baseline's level
// table suggests: tail levels of 27-108 txs each cost ~45 ms regardless of width,
// which is close to four 10 ms batcher windows (get, spend, store, setLocked —
// settings.conf:1282, 1304, 1308). A level's transactions never fill a
// 1024-2048-deep batch, so each store phase waits out its timer, giving a fixed
// per-level floor that scales with level COUNT and not with transaction count.
//
// If that is right, dropping every batcher window to 1 ms should cut the
// per-level floor roughly tenfold.
func TestUnseenTxBisect_BatcherTimers(t *testing.T) {
	opts := defaultPerfOptions()
	opts.tune = func(s *settings.Settings) {
		s.UtxoStore.GetBatcherDurationMillis = 1
		s.UtxoStore.SpendBatcherDurationMillis = 1
		s.UtxoStore.StoreBatcherDurationMillis = 1
	}

	rate := runBisectAxis(t, "bisect-batcher-1ms", opts, baseBisectConfig())
	t.Logf("AXIS batcherTimers=1ms: %.1f tx/s", rate)
}

// TestUnseenTxBisect_FlatLevel collapses the same transaction count into a single
// dependency level. If the per-level floor explanation holds, this should be
// dramatically faster than the 25-level baseline at identical tx count — which
// would also mean level depth, not block size, is what makes a block expensive.
func TestUnseenTxBisect_FlatLevel(t *testing.T) {
	cfg := baseBisectConfig()
	cfg.levelSizes = []int{sumInts(bisectShape())}

	rate := runBisectAxis(t, "bisect-flat-level", defaultPerfOptions(), cfg)
	t.Logf("AXIS flatLevel: %.1f tx/s", rate)
}

// TestUnseenTxBisect_ExternalParents pushes every seeded parent past
// MaxTxSizeInStoreInBytes (32 KB, stores/utxo/aerospike/aerospike.go:92) so
// Aerospike stores it externally. That is the axis the baseline never exercised:
// with small parents no external read happens at all, so the serial
// GetTxFromExternalStore loop inside BatchDecorate
// (stores/utxo/aerospike/get.go:697-742) was never entered.
//
// That loop walks batch results one at a time and fetches each external parent
// inline, so N external parents in a level cost N sequential blob reads. Here the
// blob store is a local file store; on a node backed by S3 each of those is a
// network round trip, and getExternalTransaction additionally tries FileTypeTx
// first and only falls back to FileTypeOutputs (get.go:1628-1637), so an
// outputs-only parent pays a wasted lookup first.
func TestUnseenTxBisect_ExternalParents(t *testing.T) {
	cfg := baseBisectConfig()
	cfg.parentFillerBytes = 40 * 1024

	rate := runBisectAxis(t, "bisect-external-parents", defaultPerfOptions(), cfg)
	t.Logf("AXIS externalParents: %.1f tx/s", rate)
}

// TestUnseenTxBisect_MultiInput raises the inputs per transaction. The baseline
// gave every tx exactly one input, which is the least stressful possible setting
// and not representative: mainnet block 959979 averages ~873 bytes/tx, implying
// several inputs each. Every extra input is another distinct parent to resolve,
// multiplying both the prefetchLevelParents batch and the validator's per-parent
// work.
func TestUnseenTxBisect_MultiInput(t *testing.T) {
	for _, inputs := range []int{4, 16} {
		inputs := inputs

		t.Run(fmt.Sprintf("inputs%d", inputs), func(t *testing.T) {
			cfg := baseBisectConfig()
			cfg.inputsPerTx = inputs

			rate := runBisectAxis(t, "bisect-inputs", defaultPerfOptions(), cfg)
			t.Logf("AXIS inputsPerTx=%d: %.1f tx/s", inputs, rate)
		})
	}
}

// TestUnseenTxBisect_BlockAssemblyDisabled removes the per-tx block-assembly
// insert and, with it, the Create-with-locked plus SetLocked 2PC unlock
// (Validator.go:940-1057). This is the catchup path's cost profile. The gap
// against baseline is what the tip path pays extra for the same transactions.
func TestUnseenTxBisect_BlockAssemblyDisabled(t *testing.T) {
	opts := defaultPerfOptions()
	opts.tune = func(s *settings.Settings) {
		s.BlockAssembly.Disabled = true
	}

	h, capture := newCapturingPerfHarness(t, opts)

	fx := generateUnseenFixture(t, h, baseBisectConfig())
	assertUnseenPrecondition(t, h, fx)

	elapsed := runUnseenBlock(t, h, capture, fx, "bisect-ba-disabled")

	require.Zero(t, h.blockAssembly.stores.Load(),
		"block assembly must have received nothing on this axis")

	t.Logf("AXIS blockAssemblyDisabled: %.1f tx/s", float64(fx.txCount)/elapsed.Seconds())
}

// TestUnseenTxBisect_ConnectionPool A/Bs mainnet's Aerospike connection cap
// against an unbounded pool at the same fan-out.
//
// Production runs ConnectionQueueSize=128 with LimitConnectionsToQueueSize=true
// (settings.conf:1271) — a hard ceiling where the client refuses to open a 129th
// connection and callers block waiting for one. processTransactionsInLevels
// independently fans out to SpendBatcherSize*2 = 2048 concurrent validations
// (check_block_subtrees.go:1156), each issuing several store operations. The two
// numbers come from unrelated settings and nothing reconciles them, so the fan-out
// oversubscribes the pool ~16x.
//
// Every earlier harness run used InitAerospikeContainer's bare URL, which carries
// no connection tuning at all — so the whole bisect matrix ran on the client
// default and never modelled this. That makes it the leading unmodelled difference
// between the harness (~4,700 tx/s) and production (~30 tx/s) for a Class B block.
func TestUnseenTxBisect_ConnectionPool(t *testing.T) {
	capped := runBisectAxis(t, "bisect-pool-capped", defaultPerfOptions(), baseBisectConfig())
	t.Logf("AXIS pool=128 (production): %.1f tx/s", capped)

	unlimited := runBisectAxis(t, "bisect-pool-default", unlimitedPoolPerfOptions(), baseBisectConfig())
	t.Logf("AXIS pool=client-default: %.1f tx/s", unlimited)

	t.Logf("RATIO default/capped = %.2fx", unlimited/capped)
}

// TestUnseenTxBisect_ConflictingTxs is the conflicting-transaction axis: the only
// block-content marker that separated the slow mainnet blocks from fast ones (189
// and 13 conflicting warnings in the two Class B windows, 0 in a fast window the
// same day), and wholly unmodelled by the fixture until now.
//
// Each conflicting block tx double-spends an outpoint already spent by a squatter
// that carries an unconfirmed descendant chain, so the run exercises the
// conflicting-create path plus checkCounterConflictingOnCurrentChain and the
// children walk.
//
// The subtests assert conflictingTxsSeen > 0. Without that assertion this axis could
// silently measure nothing and report "no effect", which is precisely the false
// negative the external-parents axis produced.
func TestUnseenTxBisect_ConflictingTxs(t *testing.T) {
	baseline := runBisectAxis(t, "bisect-conflict-none", defaultPerfOptions(), baseBisectConfig())
	t.Logf("AXIS conflicts=0: %.1f tx/s", baseline)

	for _, c := range []struct {
		n, depth int
	}{
		{50, 0},
		{50, 5},
		{200, 5},
	} {
		c := c

		t.Run(fmt.Sprintf("n%d_depth%d", c.n, c.depth), func(t *testing.T) {
			h, capture := newCapturingPerfHarness(t, defaultPerfOptions())

			cfg := baseBisectConfig()
			cfg.conflictingTxs = c.n
			cfg.conflictChainDepth = c.depth

			fx := generateUnseenFixture(t, h, cfg)
			assertUnseenPrecondition(t, h, fx)

			elapsed := runUnseenBlock(t, h, capture, fx, fmt.Sprintf("bisect-conflict-n%d-d%d", c.n, c.depth))
			rate := float64(fx.txCount) / elapsed.Seconds()

			require.Positive(t, capture.conflictingSeen.Load(),
				"no conflicting-tx warnings observed — the conflict path was never entered, so this axis measured nothing")

			t.Logf("AXIS conflicts=%d depth=%d: %.1f tx/s (%.2fx vs baseline %.1f)",
				c.n, c.depth, rate, rate/baseline, baseline)
		})
	}
}
