//go:build aerospike

// Measurement and profiling for the issue #1379 reproduction. Phases 2-4 of
// plans/1379-unseen-tx-throughput-plan.md.
package subtreevalidation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/subtreevalidation/subtreevalidation_api"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/fields"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// levelEvent is one captured per-level boundary from processTransactionsInLevels.
type levelEvent struct {
	level int
	txs   int
	done  bool
	at    time.Time
}

// levelCaptureLogger records the per-level and pre-check progress that
// processTransactionsInLevels only emits at Debugf (check_block_subtrees.go:1015,
// :1030, :1112, :1223). Capturing them here means the harness needs no change to
// production log levels to produce a per-level breakdown.
//
// The prefix checks matter for correctness of the measurement, not just speed:
// the same function also Debugf's once per transaction (:1180, :1211), so a
// logger that took its mutex on every call would serialise 6,258 debug lines
// across the 2048-wide fan-out and show up in the profile as contention the
// production path does not have. Only the ~50 level-boundary lines take the lock.
type levelCaptureLogger struct {
	ulogger.TestLogger

	mu       sync.Mutex
	events   []levelEvent
	preCheck []string
}

func (l *levelCaptureLogger) Debugf(format string, args ...interface{}) {
	switch {
	case strings.HasPrefix(format, "[processTransactionsInLevels] Processing level "):
		if len(args) < 3 {
			return
		}

		level, ok1 := args[0].(uint32)
		count, ok2 := args[2].(int)

		if !ok1 || !ok2 {
			return
		}

		l.mu.Lock()
		l.events = append(l.events, levelEvent{
			level: int(level),
			txs:   count,
			done:  strings.HasSuffix(format, "DONE"),
			at:    time.Now(),
		})
		l.mu.Unlock()

	case strings.HasPrefix(format, "[processTransactionsInLevels] Pre-check:"):
		l.mu.Lock()
		l.preCheck = append(l.preCheck, fmt.Sprintf(format, args...))
		l.mu.Unlock()
	}
}

// The server replaces its logger via these in places; returning the same
// instance keeps the capture attached.
func (l *levelCaptureLogger) New(_ string, _ ...ulogger.Option) ulogger.Logger { return l }
func (l *levelCaptureLogger) Duplicate(_ ...ulogger.Option) ulogger.Logger     { return l }
func (l *levelCaptureLogger) WithTraceContext(_ context.Context) ulogger.Logger {
	return l
}

// levelDurations pairs start and DONE events into per-level wall times.
func (l *levelCaptureLogger) levelDurations() map[int]time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	starts := make(map[int]time.Time)
	out := make(map[int]time.Duration)

	for _, e := range l.events {
		if e.done {
			if start, ok := starts[e.level]; ok {
				out[e.level] = e.at.Sub(start)
			}

			continue
		}

		starts[e.level] = e.at
	}

	return out
}

// assertUnseenPrecondition proves both halves of the fixture contract before any
// timing is taken: the block's own txs are absent from the UTXO store, and the
// seeded parents are present and mined.
//
// This is the assertion the whole reproduction rests on. If the block txs were
// present, processTransactionsInLevels would take the missed == 0 early return
// (check_block_subtrees.go:1026) and the measured number would be the fast path
// — the exact thing being investigated, silently inverted.
func assertUnseenPrecondition(t *testing.T, h *perfHarness, fx *unseenFixture) {
	t.Helper()

	ctx := context.Background()

	items := make([]*utxostore.UnresolvedMetaData, 0, len(fx.txs))
	for _, tx := range fx.txs {
		items = append(items, &utxostore.UnresolvedMetaData{Hash: *tx.TxIDChainHash()})
	}

	require.NoError(t, h.utxoStore.BatchDecorate(ctx, items, fields.BlockIDs))

	present := 0

	for _, item := range items {
		if item.Err == nil {
			present++
			continue
		}

		require.True(t, errors.Is(item.Err, errors.ErrTxNotFound),
			"unexpected error probing block tx %s: %v", item.Hash, item.Err)
	}

	require.Zero(t, present,
		"%d/%d block transactions were already in the UTXO store — the unseen precondition does not hold and this run would measure the missed==0 fast path",
		present, len(fx.txs))

	// Spot-check the parent side. A parent that is missing, or present but
	// unmined, sends the run down the missing-parent or unconfirmed-parent path
	// instead of the steady-state one.
	firstLevelTx := fx.txs[0]
	parentHash := firstLevelTx.Inputs[0].PreviousTxIDChainHash()

	parentMeta, err := h.utxoStore.Get(ctx, parentHash, fields.BlockHeights, fields.BlockIDs)
	require.NoError(t, err, "seeded parent %s must be in the store", parentHash)
	require.NotEmpty(t, parentMeta.BlockHeights,
		"seeded parent must be mined — empty BlockHeights sends the validator down the unconfirmed-parent path instead of steady state")
}

// profileDir resolves where profiles are written. TERANODE_PERF_PROFILE_DIR keeps
// them after the test process exits; otherwise they land in the test temp dir and
// the path is logged.
func profileDir(t *testing.T) string {
	t.Helper()

	if dir := os.Getenv("TERANODE_PERF_PROFILE_DIR"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o750))
		return dir
	}

	return t.TempDir()
}

// runUnseenBlock drives one measured CheckBlockSubtrees pass and reports
// throughput, the per-level breakdown, and profile paths.
func runUnseenBlock(t *testing.T, h *perfHarness, capture *levelCaptureLogger, fx *unseenFixture, label string) time.Duration {
	t.Helper()

	dir := profileDir(t)

	// Block and mutex profiling must be armed explicitly. Without these two calls
	// both profiles come back empty — and for a latency-bound path they are the
	// primary artefacts, because CPU will look idle while goroutines park on store
	// round trips.
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)

	defer func() {
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	}()

	cpuPath := filepath.Join(dir, label+"-cpu.pprof")
	cpuFile, err := os.Create(cpuPath) // nolint:gosec // test-controlled path
	require.NoError(t, err)

	require.NoError(t, pprof.StartCPUProfile(cpuFile))

	start := time.Now()

	response, err := h.server.CheckBlockSubtrees(context.Background(), &subtreevalidation_api.CheckBlockSubtreesRequest{
		Block:   fx.blockBytes,
		BaseUrl: "legacy",
	})

	elapsed := time.Since(start)

	pprof.StopCPUProfile()
	require.NoError(t, cpuFile.Close())

	require.NoError(t, err)
	require.True(t, response.Blessed)

	for _, name := range []string{"block", "mutex", "goroutine", "heap"} {
		path := filepath.Join(dir, label+"-"+name+".pprof")

		f, ferr := os.Create(path) // nolint:gosec // test-controlled path
		require.NoError(t, ferr)
		require.NoError(t, pprof.Lookup(name).WriteTo(f, 0))
		require.NoError(t, f.Close())
	}

	txPerSec := float64(fx.txCount) / elapsed.Seconds()

	t.Logf("=== %s ===", label)
	t.Logf("txs=%d seededParents=%d subtrees=%d", fx.txCount, fx.seededParents, len(fx.subtreeHashes))
	t.Logf("elapsed=%s throughput=%.1f tx/s", elapsed.Round(time.Millisecond), txPerSec)
	t.Logf("blockAssemblyStores=%d txMetaPublished=%d", h.blockAssembly.stores.Load(), h.txMetaPublished.Load())
	t.Logf("profiles written to %s", dir)

	for _, line := range capture.preCheck {
		t.Logf("precheck: %s", line)
	}

	durations := capture.levelDurations()
	if len(durations) > 0 {
		levels := make([]int, 0, len(durations))
		for level := range durations {
			levels = append(levels, level)
		}

		// Report slowest levels first: with 25 levels the interesting signal is
		// which few dominate, not the full ordered list.
		sortIntsByDurationDesc(levels, durations)

		t.Logf("--- slowest levels (of %d) ---", len(durations))

		for i, level := range levels {
			if i >= 10 {
				break
			}

			t.Logf("level %d: %s", level, durations[level].Round(time.Millisecond))
		}
	}

	return elapsed
}

func sortIntsByDurationDesc(levels []int, durations map[int]time.Duration) {
	for i := 1; i < len(levels); i++ {
		for j := i; j > 0 && durations[levels[j]] > durations[levels[j-1]]; j-- {
			levels[j], levels[j-1] = levels[j-1], levels[j]
		}
	}
}

// newCapturingPerfHarness is newPerfHarness with the level-capturing logger
// installed.
func newCapturingPerfHarness(t *testing.T, opts perfHarnessOptions) (*perfHarness, *levelCaptureLogger) {
	t.Helper()

	capture := &levelCaptureLogger{}
	h := newPerfHarnessWithLogger(t, capture, opts)

	return h, capture
}

// TestUnseenTxThroughput_FixtureRoundTrip is phase 2's verification: a small
// fixture must round-trip and, critically, the unseen precondition must actually
// hold and the generator must produce the level count it claims. A generator that
// believes it built 5 levels but built 1 would make every later number
// meaningless.
func TestUnseenTxThroughput_FixtureRoundTrip(t *testing.T) {
	h, capture := newCapturingPerfHarness(t, defaultPerfOptions())

	cfg := unseenFixtureConfig{
		levelSizes:    []int{200, 120, 80, 60, 40},
		txsPerSubtree: 1024,
	}

	fx := generateUnseenFixture(t, h, cfg)
	require.Equal(t, 500, fx.txCount)

	assertUnseenPrecondition(t, h, fx)

	runUnseenBlock(t, h, capture, fx, "fixture-roundtrip")

	// The generator's claimed shape must match what the production level splitter
	// actually derived.
	durations := capture.levelDurations()
	require.Len(t, durations, len(cfg.levelSizes),
		"production level splitter derived %d levels but the fixture was built for %d — the dependency graph is not the shape the generator thinks it is",
		len(durations), len(cfg.levelSizes))

	// Every block tx should now be in the store, unlocked.
	assertAllValidatedAndUnlocked(t, h, fx)
}

// TestUnseenTxThroughput is phase 4: the shape-matched baseline against mainnet
// block 959979's measured dependency histogram. This is the decision point —
// <=100 tx/s reproduces the issue, >1000 tx/s means the synthetic shape is wrong
// and the real-block fallback is needed.
func TestUnseenTxThroughput(t *testing.T) {
	h, capture := newCapturingPerfHarness(t, defaultPerfOptions())

	cfg := unseenFixtureConfig{
		levelSizes:    mainnet959979Shape(),
		txsPerSubtree: 1024,
	}

	fx := generateUnseenFixture(t, h, cfg)
	require.Equal(t, 6258, fx.txCount, "baseline must match the measured tx count of mainnet block 959979")

	assertUnseenPrecondition(t, h, fx)

	elapsed := runUnseenBlock(t, h, capture, fx, "baseline-959979-shape")

	txPerSec := float64(fx.txCount) / elapsed.Seconds()
	t.Logf("BASELINE RESULT: %.1f tx/s (mainnet observed 27-50 tx/s)", txPerSec)
}

func assertAllValidatedAndUnlocked(t *testing.T, h *perfHarness, fx *unseenFixture) {
	t.Helper()

	ctx := context.Background()

	hashes := make([]chainhash.Hash, 0, len(fx.txs))
	for _, tx := range fx.txs {
		hashes = append(hashes, *tx.TxIDChainHash())
	}

	locked := 0
	missing := 0

	for i := range hashes {
		md, err := h.utxoStore.Get(ctx, &hashes[i], fields.Locked)
		if err != nil {
			missing++
			continue
		}

		if md.Locked {
			locked++
		}
	}

	require.Zero(t, missing, "%d/%d validated txs are absent from the store", missing, len(hashes))
	require.Zero(t, locked, "%d/%d validated txs are still locked — the SetLocked 2PC did not complete for them", locked, len(hashes))
}
